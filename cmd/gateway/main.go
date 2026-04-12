// Gateway runs inside a Docker container. It creates a TUN device, accepts
// tunnel connections from the Windows host, and forwards IP packets between
// the tunnel and Docker bridge networks.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"sync"
	"syscall"

	"github.com/docker-reach/docker-reach/cmd/gateway/web"
	"github.com/docker-reach/docker-reach/internal/tunnel"
	"github.com/songgao/water"
)

const (
	tunName = "tun0"
	mtu     = 1500
)

// envOrDefault returns the value of an environment variable, or def if unset/empty.
func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug})))
	slog.Info("docker-reach gateway starting")

	// Read configuration from environment variables injected by the host.
	tunnelPort := ":" + envOrDefault("TUNNEL_PORT", "9999")
	dashboardPort := envOrDefault("DASHBOARD_PORT", "9998")
	tunSubnet := envOrDefault("TUN_SUBNET", "10.0.85.0/24")

	// Derive gateway CIDR from subnet (e.g. "10.0.85.0/24" → "10.0.85.1/24")
	tunCIDR := subnetToGatewayCIDR(tunSubnet)
	masqSubnet := subnetToMasqCIDR(tunSubnet)

	ensureTunDev()

	tun, err := water.New(water.Config{
		DeviceType: water.TUN,
		PlatformSpecificParams: water.PlatformSpecificParams{
			Name: tunName,
		},
	})
	if err != nil {
		slog.Error("create tun", "error", err)
		os.Exit(1)
	}
	defer tun.Close()

	if err := setupNetworking(tun.Name(), tunCIDR, masqSubnet); err != nil {
		slog.Error("setup networking", "error", err)
		os.Exit(1)
	}

	ln, err := net.Listen("tcp", tunnelPort)
	if err != nil {
		slog.Error("listen", "error", err)
		os.Exit(1)
	}
	defer ln.Close()
	slog.Info("listening", "addr", tunnelPort)

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	go func() { <-ctx.Done(); ln.Close() }()

	// Start web dashboard (best-effort — don't fail if Docker socket is unavailable)
	if dashboard, err := web.New(dashboardPort); err == nil {
		dashboard.Start()
	} else {
		slog.Warn("dashboard unavailable", "error", err)
	}

	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			slog.Error("accept", "error", err)
			continue
		}
		slog.Info("client connected", "remote", conn.RemoteAddr())
		go handleConn(ctx, tun, conn)
	}
}

func handleConn(parent context.Context, tun *water.Interface, conn net.Conn) {
	ctx, cancel := context.WithCancel(parent)

	// Defers run LIFO: wg.Wait fires before t.Close, and both fire after cancel.
	// cancel unblocks the TUN→tunnel goroutine, wg.Wait ensures it has exited,
	// then t.Close tears down the tunnel safely.
	t := tunnel.New(conn)
	defer t.Close()

	var wg sync.WaitGroup
	defer wg.Wait()
	defer cancel()

	wg.Add(1)

	// TUN → tunnel
	go func() {
		defer wg.Done()
		buf := make([]byte, mtu+64)
		for {
			n, err := tun.Read(buf)
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				slog.Debug("tun read", "error", err)
				return
			}
			if err := t.Send(buf[:n]); err != nil {
				slog.Debug("tunnel send", "error", err)
				return
			}
		}
	}()

	// tunnel → TUN
	for {
		pkt, err := t.Receive()
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			slog.Debug("tunnel recv", "error", err)
			return
		}
		if _, err := tun.Write(pkt); err != nil {
			slog.Debug("tun write", "error", err)
			return
		}
	}
}

// ensureTunDev creates /dev/net/tun if it doesn't exist.
func ensureTunDev() {
	if _, err := os.Stat("/dev/net/tun"); err == nil {
		return
	}
	if err := os.MkdirAll("/dev/net", 0755); err != nil {
		slog.Error("mkdir /dev/net", "error", err)
		os.Exit(1)
	}
	// mknod /dev/net/tun c 10 200
	if err := run("mknod", "/dev/net/tun", "c", "10", "200"); err != nil {
		slog.Error("mknod /dev/net/tun", "error", err)
		os.Exit(1)
	}
	if err := os.Chmod("/dev/net/tun", 0666); err != nil {
		slog.Error("chmod /dev/net/tun", "error", err)
		os.Exit(1)
	}
	slog.Info("created /dev/net/tun")
}

func run(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	return cmd.Run()
}

func setupNetworking(dev, tunCIDR, masqSubnet string) error {
	cmds := [][]string{
		{"ip", "addr", "add", tunCIDR, "dev", dev},
		{"ip", "link", "set", dev, "up"},
		{"ip", "link", "set", dev, "mtu", "1500"},
		{"sysctl", "-w", "net.ipv4.ip_forward=1"},
		{"sysctl", "-w", "net.ipv4.conf." + dev + ".accept_local=1"},
	}
	for _, c := range cmds {
		if err := run(c[0], c[1:]...); err != nil {
			return err
		}
	}

	// Add the MASQUERADE rule only if it isn't already present, so that
	// repeated restarts don't accumulate duplicate rules.
	masqRule := []string{"-t", "nat", "-C", "POSTROUTING", "-s", masqSubnet, "-j", "MASQUERADE"}
	if err := run("iptables", masqRule...); err != nil {
		// Rule is absent — add it.
		addRule := []string{"-t", "nat", "-A", "POSTROUTING", "-s", masqSubnet, "-j", "MASQUERADE"}
		if err := run("iptables", addRule...); err != nil {
			return err
		}
	}

	slog.Info("networking configured")
	return nil
}

// subnetToGatewayCIDR converts "10.0.85.0/24" → "10.0.85.1/24".
func subnetToGatewayCIDR(subnet string) string {
	ip, ipnet, err := net.ParseCIDR(subnet)
	if err != nil {
		return subnet
	}
	ip4 := ip.To4()
	if ip4 == nil {
		return subnet
	}
	gw := make(net.IP, 4)
	copy(gw, ip4)
	gw[3] = 1
	ones, _ := ipnet.Mask.Size()
	return fmt.Sprintf("%s/%d", gw.String(), ones)
}

// subnetToMasqCIDR returns the network address in CIDR notation (e.g. "10.0.85.0/24").
func subnetToMasqCIDR(subnet string) string {
	_, ipnet, err := net.ParseCIDR(subnet)
	if err != nil {
		return subnet
	}
	return ipnet.String()
}
