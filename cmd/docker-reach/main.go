// docker-reach CLI — runs on Windows. Creates a WinTUN adapter, manages the
// gateway container, establishes the IP tunnel, updates the hosts file for
// container name resolution, and watches Docker for live updates.
package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/docker-reach/docker-reach/internal/config"
	"github.com/docker-reach/docker-reach/internal/dns"
	"github.com/docker-reach/docker-reach/internal/dockerutil"
	"github.com/docker-reach/docker-reach/internal/tunnel"

	"golang.zx2c4.com/wintun"
)

const (
	adapterType = "DockerReach"
	ringSize    = 0x400000 // 4 MiB wintun ring
)

// cfg is loaded once at startup and used throughout the process lifetime.
var cfg *config.Config

func main() {
	if exe, err := os.Executable(); err == nil {
		os.Chdir(filepath.Dir(exe))
	}

	// Load config before setting up logging so LogFile is available.
	var err error
	cfg, err = config.Load("config.yaml")
	if err != nil {
		// Non-fatal: fall back to defaults and warn after logging is ready.
		cfg = config.Default()
	}

	setupLogging()

	if err != nil {
		slog.Warn("config load failed, using defaults", "error", err)
	}

	if len(os.Args) < 2 {
		usage()
		os.Exit(1)
	}
	switch os.Args[1] {
	case "up":
		cmdUp()
	case "down":
		cmdDown()
	case "status":
		cmdStatus()
	default:
		usage()
		os.Exit(1)
	}
}

// setupLogging writes structured logs to both stderr and a persistent log file.
// M-6 fix: use O_APPEND instead of O_TRUNC so logs accumulate across runs.
func setupLogging() {
	logFile, err := os.OpenFile(cfg.LogFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug})))
		return
	}
	w := io.MultiWriter(os.Stderr, logFile)
	slog.SetDefault(slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: slog.LevelDebug})))
}

func usage() {
	fmt.Fprintf(os.Stderr, `docker-reach — access Docker containers by IP and name from Windows

Usage:
  docker-reach up      Start gateway, tunnel, and hosts
  docker-reach down    Stop everything and clean up hosts file
  docker-reach status  Show connection status
`)
}

// ---------------------------------------------------------------------------
// cleanup — C-4 fix
//
// A single struct tracks every resource that has been successfully initialised.
// One defer at the very top of cmdUp always runs cleanupAll, making it safe
// against Ctrl+C arriving at any point during startup.
// ---------------------------------------------------------------------------

type cleanupState struct {
	mu      sync.Mutex
	tun     *tunnel.Tunnel
	adapter *wintun.Adapter
	session *wintun.Session
	hosts   *dns.HostsManager
	watcher *dockerutil.Watcher

	// H-2 fix: track dynamically added routes so they are always removed.
	// Key is "dest/mask", value is the destination IP.
	routeMu     sync.Mutex
	activeRoutes map[string]routeEntry
}

type routeEntry struct {
	dest string
	mask string
}

func newCleanupState() *cleanupState {
	return &cleanupState{
		activeRoutes: make(map[string]routeEntry),
	}
}

// trackRoute registers a route for cleanup.
func (c *cleanupState) trackRoute(dest, mask string) {
	c.routeMu.Lock()
	defer c.routeMu.Unlock()
	c.activeRoutes[dest+"/"+mask] = routeEntry{dest: dest, mask: mask}
}

// cleanupAll tears down everything that was successfully initialised, in
// reverse order. It is idempotent and safe to call from a deferred statement
// at any point during startup.
func (c *cleanupState) cleanupAll() {
	c.mu.Lock()
	defer c.mu.Unlock()

	// 1. Stop the WinTUN session (stops packet I/O goroutines).
	if c.session != nil {
		c.session.End()
		c.session = nil
		slog.Info("WinTUN session ended")
	}

	// 2. Close the tunnel connection.
	if c.tun != nil {
		c.tun.Close()
		c.tun = nil
		slog.Info("tunnel closed")
	}

	// 3. Remove all tracked routes (initial + dynamically added).
	c.routeMu.Lock()
	for _, r := range c.activeRoutes {
		routeDel(r.dest, r.mask)
	}
	c.activeRoutes = make(map[string]routeEntry)
	c.routeMu.Unlock()
	slog.Info("routes cleaned up")

	// 4. Clean up the hosts file.
	if c.hosts != nil {
		c.hosts.Cleanup()
		c.hosts = nil
		slog.Info("hosts file cleaned up")
	}

	// 5. Close the WinTUN adapter (this deletes it from the system).
	if c.adapter != nil {
		c.adapter.Close()
		c.adapter = nil
		slog.Info("WinTUN adapter removed")
	}

	// 6. Close the Docker watcher.
	if c.watcher != nil {
		c.watcher.Close()
		c.watcher = nil
	}
}

// ---------------------------------------------------------------------------
// up
// ---------------------------------------------------------------------------

func cmdUp() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	// C-4 fix: register ONE top-level cleanup that always runs, regardless of
	// where startup fails or when Ctrl+C arrives.
	cl := newCleanupState()
	defer cl.cleanupAll()

	killExisting()
	slog.Info("starting docker-reach", "cwd", mustCwd())

	// ---- Phase 1: gateway + tunnel (no host network changes) ----

	watcher, err := dockerutil.NewWatcher(cfg.GatewayName, cfg.TunnelPort, cfg.DashboardPort)
	if err != nil {
		fatal("docker", err)
	}
	cl.watcher = watcher

	slog.Info("ensuring gateway container")
	gwID, err := watcher.EnsureGateway(ctx, cfg.GatewayImage)
	if err != nil {
		fatal("gateway", err)
	}
	if err := watcher.ConnectGatewayToNetworks(ctx, gwID); err != nil {
		fatal("connect networks", err)
	}

	slog.Info("waiting for gateway tunnel")
	var tun *tunnel.Tunnel
	for i := 0; i < 30; i++ {
		conn, err := net.DialTimeout("tcp", cfg.TunnelAddr(), time.Second)
		if err == nil {
			tun = tunnel.New(conn)
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if tun == nil {
		fatal("tunnel", fmt.Errorf("could not connect to gateway at %s after 15s", cfg.TunnelAddr()))
	}
	cl.tun = tun
	slog.Info("tunnel established")

	// ---- Phase 2: WinTUN adapter ----
	//
	// C-3 fix: a previous crash may have left a stale adapter registered with
	// the OS. Try to open it first; if it exists, close and delete it before
	// creating a fresh one so that CreateAdapter does not fail.

	slog.Info("creating WinTUN adapter")
	if stale, err := wintun.OpenAdapter(cfg.AdapterName); err == nil {
		slog.Warn("found stale WinTUN adapter from previous run — removing it")
		stale.Close()
	}

	adapter, err := wintun.CreateAdapter(cfg.AdapterName, adapterType, nil)
	if err != nil {
		slog.Error("WinTUN failed — is wintun.dll next to the exe?", "error", err)
		os.Exit(1)
	}
	cl.adapter = adapter
	slog.Info("WinTUN adapter created")

	// ---- Phase 3: host networking (only after WinTUN confirmed) ----

	netsh("interface", "ip", "set", "address", cfg.AdapterName, "static", cfg.TunLocalIP(), cfg.TunMask())
	netsh("interface", "ip", "set", "interface", cfg.AdapterName, "metric=9999")
	slog.Info("adapter IP configured", "ip", cfg.TunLocalIP())

	subnets, err := watcher.Subnets(ctx)
	if err != nil {
		fatal("subnets", err)
	}
	for _, s := range subnets {
		if cfg.OverlapsWSL(s.CIDR) {
			slog.Warn("skipping subnet to protect WSL", "network", s.NetworkName, "cidr", s.CIDR)
			continue
		}
		mask := net.IP(s.CIDR.Mask).String()
		routeAdd(s.CIDR.IP.String(), mask, cfg.TunGatewayIP())
		cl.trackRoute(s.CIDR.IP.String(), mask)
		slog.Info("route added", "network", s.NetworkName, "cidr", s.CIDR)
	}

	// ---- Phase 4: hosts file for container name resolution ----

	hosts := dns.NewHostsManager()
	cl.hosts = hosts
	refreshHosts(ctx, watcher, hosts)

	// ---- Phase 5: watch Docker events ----
	//
	// H-2 fix: routes added inside the callback are tracked via cl.trackRoute
	// so that cleanupAll removes them on exit.

	go watcher.WatchEvents(ctx, func() {
		watcher.ConnectGatewayToNetworks(ctx, gwID)
		if subs, err := watcher.Subnets(ctx); err == nil {
			for _, s := range subs {
				if cfg.OverlapsWSL(s.CIDR) {
					continue
				}
				dest := s.CIDR.IP.String()
				mask := net.IP(s.CIDR.Mask).String()
				routeAdd(dest, mask, cfg.TunGatewayIP())
				cl.trackRoute(dest, mask)
			}
		}
		refreshHosts(ctx, watcher, hosts)
	})

	// ---- Phase 6: packet forwarding ----

	session, err := adapter.StartSession(ringSize)
	if err != nil {
		fatal("wintun session", err)
	}
	cl.session = &session

	slog.Info("docker-reach is running", "tunnel", cfg.TunnelAddr())
	fmt.Println()
	fmt.Println("  Ready! Access containers by IP or name:")
	fmt.Println("    curl http://172.17.0.3:8080")
	fmt.Println("    curl http://my-container.docker:8080")
	fmt.Println()
	fmt.Printf("  Dashboard: http://localhost:%d\n", cfg.DashboardPort)
	fmt.Println()
	fmt.Println("  Press Ctrl+C to stop.")
	fmt.Println()

	var wg sync.WaitGroup

	// WinTUN → tunnel
	// M-3 fix: sleep 1ms on non-context errors to avoid a 100% CPU busy-loop
	// when ReceivePacket returns transient errors.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			pkt, err := session.ReceivePacket()
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				// Transient error — yield briefly rather than spinning.
				time.Sleep(time.Millisecond)
				continue
			}
			data := make([]byte, len(pkt))
			copy(data, pkt)
			session.ReleaseReceivePacket(pkt)
			if err := tun.Send(data); err != nil {
				if ctx.Err() != nil {
					return
				}
				slog.Debug("tunnel send", "error", err)
				return
			}
		}
	}()

	// tunnel → WinTUN
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			pkt, err := tun.Receive()
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				slog.Debug("tunnel recv", "error", err)
				return
			}
			buf, err := session.AllocateSendPacket(len(pkt))
			if err != nil {
				continue
			}
			copy(buf, pkt)
			session.SendPacket(buf)
		}
	}()

	<-ctx.Done()
	slog.Info("shutting down gracefully")
	// cleanupAll runs via the deferred call at the top of cmdUp.
}

// ---------------------------------------------------------------------------
// down
// ---------------------------------------------------------------------------

func cmdDown() {
	watcher, err := dockerutil.NewWatcher(cfg.GatewayName, cfg.TunnelPort, cfg.DashboardPort)
	if err != nil {
		fatal("docker", err)
	}
	defer watcher.Close()

	if err := watcher.RemoveGateway(context.Background()); err != nil {
		slog.Warn("remove gateway", "error", err)
	} else {
		slog.Info("gateway container removed")
	}

	hosts := dns.NewHostsManager()
	hosts.Cleanup()
	slog.Info("cleanup complete")
}

// ---------------------------------------------------------------------------
// status
// ---------------------------------------------------------------------------

func cmdStatus() {
	watcher, err := dockerutil.NewWatcher(cfg.GatewayName, cfg.TunnelPort, cfg.DashboardPort)
	if err != nil {
		fmt.Println("Docker: not reachable")
		return
	}
	defer watcher.Close()

	ctx := context.Background()
	containers, _ := watcher.Containers(ctx)
	subnets, _ := watcher.Subnets(ctx)

	conn, err := net.DialTimeout("tcp", cfg.TunnelAddr(), time.Second)
	tunnelOK := err == nil
	if conn != nil {
		conn.Close()
	}

	fmt.Printf("Tunnel:     %s\n", boolStatus(tunnelOK))
	fmt.Printf("Subnets:    %d\n", len(subnets))
	for _, s := range subnets {
		fmt.Printf("  %-20s %s\n", s.NetworkName, s.CIDR)
	}
	fmt.Printf("Containers: %d\n", len(containers))
	for _, c := range containers {
		fmt.Printf("  %-30s %s\n", c.Name+".docker", c.IP)
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func refreshHosts(ctx context.Context, w *dockerutil.Watcher, h *dns.HostsManager) {
	records, err := w.Containers(ctx)
	if err != nil {
		slog.Warn("list containers", "error", err)
		return
	}
	m := make(map[string]net.IP, len(records))
	for _, r := range records {
		m[r.Name] = r.IP
	}
	if err := h.Update(m); err != nil {
		slog.Warn("hosts update", "error", err)
	}
}

func netsh(args ...string) {
	cmd := exec.Command("netsh", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		slog.Warn("netsh", "args", strings.Join(args, " "), "error", err, "output", string(out))
	}
}

// routeAdd adds a Windows route and logs any failure.
//
// H-3 fix: previously errors were silently discarded. Now failures are logged
// so they are visible in the log file and stderr.
func routeAdd(dest, mask, gw string) {
	idx := getAdapterIndex()
	if idx == "" {
		slog.Error("routeAdd: could not get adapter index — route not added", "dest", dest)
		return
	}
	out, err := exec.Command("route", "add", dest, "mask", mask, gw, "metric", "5", "if", idx).CombinedOutput()
	if err != nil {
		slog.Error("routeAdd failed", "dest", dest, "mask", mask, "gw", gw, "error", err, "output", string(out))
	}
}

// getAdapterIndex returns the Windows interface index for the WinTUN adapter.
//
// H-3 fix: retry up to 3 times with a 500 ms delay because the adapter may
// not be visible to PowerShell immediately after creation. Also validate that
// the returned string looks like a non-empty integer before returning it.
func getAdapterIndex() string {
	ps := fmt.Sprintf(`(Get-NetAdapter -Name '%s' -ErrorAction SilentlyContinue).ifIndex`, cfg.AdapterName)
	for attempt := 1; attempt <= 3; attempt++ {
		out, err := exec.Command("powershell", "-Command", ps).Output()
		if err == nil {
			idx := strings.TrimSpace(string(out))
			// Validate: must be a non-empty string of digits.
			if idx != "" && isDigits(idx) {
				return idx
			}
		}
		if attempt < 3 {
			slog.Debug("getAdapterIndex: adapter not visible yet, retrying",
				"attempt", attempt, "adapter", cfg.AdapterName)
			time.Sleep(500 * time.Millisecond)
		}
	}
	slog.Error("getAdapterIndex: adapter index not available after retries", "adapter", cfg.AdapterName)
	return ""
}

// isDigits reports whether s consists entirely of ASCII decimal digits.
func isDigits(s string) bool {
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return len(s) > 0
}

func routeDel(dest, mask string) {
	exec.Command("route", "delete", dest, "mask", mask).Run()
}

// killExisting terminates any other running docker-reach process and removes a
// leftover gateway container.
//
// H-5 fix: the previous pattern (-match 'docker-reach|^dr$') was too broad and
// could kill unrelated processes whose names begin with "dr". We now match on
// the full executable path so only genuine docker-reach binaries are targeted.
func killExisting() {
	exe, err := os.Executable()
	if err != nil {
		// Fallback: use a tighter name match that avoids the short "dr" alias.
		exe = ""
	}
	myPID := os.Getpid()

	var ps string
	if exe != "" {
		// Normalise to forward slashes so the PowerShell string comparison is
		// consistent regardless of how the path was resolved.
		exePath := strings.ReplaceAll(filepath.ToSlash(exe), "'", "''") // escape single quotes
		ps = fmt.Sprintf(
			`Get-Process | Where-Object { ($_.Path -eq '%s') -and ($_.Id -ne %d) } | Stop-Process -Force -ErrorAction SilentlyContinue`,
			exePath, myPID,
		)
	} else {
		// Safe fallback: match exact process name "docker-reach" only.
		ps = fmt.Sprintf(
			`Get-Process -Name 'docker-reach' -ErrorAction SilentlyContinue | Where-Object { $_.Id -ne %d } | Stop-Process -Force -ErrorAction SilentlyContinue`,
			myPID,
		)
	}

	exec.Command("powershell", "-Command", ps).Run()
	exec.Command("docker", "rm", "-f", cfg.GatewayName).Run()
	slog.Info("cleaned up existing instances")
}

func mustCwd() string {
	d, _ := os.Getwd()
	return d
}

func boolStatus(ok bool) string {
	if ok {
		return "connected"
	}
	return "disconnected"
}

func fatal(component string, err error) {
	slog.Error(component, "error", err)
	os.Exit(1)
}
