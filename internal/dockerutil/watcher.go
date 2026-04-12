// Package dockerutil watches Docker for container and network changes,
// providing live name→IP mappings and subnet information.
package dockerutil

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"strings"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/events"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/client"
	"github.com/docker/go-connections/nat"
)

// Subnet describes a Docker network's CIDR.
type Subnet struct {
	NetworkID   string
	NetworkName string
	CIDR        *net.IPNet
}

// ContainerRecord maps a name/alias to a container IP.
type ContainerRecord struct {
	Name string
	IP   net.IP
}

// Watcher monitors Docker for changes.
type Watcher struct {
	cli           *client.Client
	gatewayName   string
	tunnelPort    int
	dashboardPort int
}

func NewWatcher(gatewayName string, tunnelPort, dashboardPort int) (*Watcher, error) {
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, fmt.Errorf("docker client: %w", err)
	}
	return &Watcher{
		cli:           cli,
		gatewayName:   gatewayName,
		tunnelPort:    tunnelPort,
		dashboardPort: dashboardPort,
	}, nil
}

func (w *Watcher) Close() error {
	return w.cli.Close()
}

// Client returns the underlying Docker client.
// Intended for use by packages that need richer API access than Watcher exposes.
func (w *Watcher) Client() *client.Client {
	return w.cli
}

// Subnets returns all Docker bridge-network subnets.
func (w *Watcher) Subnets(ctx context.Context) ([]Subnet, error) {
	networks, err := w.cli.NetworkList(ctx, network.ListOptions{})
	if err != nil {
		return nil, err
	}
	var out []Subnet
	for _, n := range networks {
		if n.Driver != "bridge" {
			continue
		}
		for _, cfg := range n.IPAM.Config {
			_, cidr, err := net.ParseCIDR(cfg.Subnet)
			if err != nil {
				continue
			}
			out = append(out, Subnet{
				NetworkID:   n.ID,
				NetworkName: n.Name,
				CIDR:        cidr,
			})
		}
	}
	return out, nil
}

// Containers returns name→IP records for all running containers.
// Each container produces exactly one IP — from the "bridge" network when present,
// otherwise the first network found — so name→IP mappings are deterministic.
func (w *Watcher) Containers(ctx context.Context) ([]ContainerRecord, error) {
	containers, err := w.cli.ContainerList(ctx, container.ListOptions{})
	if err != nil {
		return nil, err
	}
	var out []ContainerRecord
	for _, c := range containers {
		// Pick the canonical IP for this container: prefer the network named
		// "bridge", then fall back to the first network that has a valid IP.
		var chosenIP net.IP
		var chosenAliases []string
		for netName, netSettings := range c.NetworkSettings.Networks {
			ip := net.ParseIP(netSettings.IPAddress)
			if ip == nil {
				continue
			}
			if chosenIP == nil || netName == "bridge" {
				chosenIP = ip
				chosenAliases = netSettings.Aliases
				if netName == "bridge" {
					break // preferred network found; no need to keep iterating
				}
			}
		}
		if chosenIP == nil {
			continue
		}

		// Container name (strip leading /)
		for _, name := range c.Names {
			clean := strings.TrimPrefix(name, "/")
			out = append(out, ContainerRecord{Name: clean, IP: chosenIP})
		}
		// Network aliases from the chosen network only
		for _, alias := range chosenAliases {
			out = append(out, ContainerRecord{Name: alias, IP: chosenIP})
		}
	}
	return out, nil
}

// EnsureGateway creates and starts the gateway container if it doesn't exist.
// Returns the container ID.
func (w *Watcher) EnsureGateway(ctx context.Context, image string) (string, error) {
	tunnelPortStr := fmt.Sprintf("%d", w.tunnelPort)
	dashboardPortStr := fmt.Sprintf("%d", w.dashboardPort)

	// Check if already running.
	containers, err := w.cli.ContainerList(ctx, container.ListOptions{
		Filters: filters.NewArgs(filters.Arg("name", w.gatewayName)),
	})
	if err != nil {
		return "", err
	}
	if len(containers) > 0 {
		slog.Info("gateway container already running", "id", containers[0].ID[:12])
		return containers[0].ID, nil
	}

	tunnelContainerPort := nat.Port(tunnelPortStr + "/tcp")
	dashboardContainerPort := nat.Port(dashboardPortStr + "/tcp")

	// Create.
	resp, err := w.cli.ContainerCreate(ctx,
		&container.Config{
			Image: image,
			Env: []string{
				"TUNNEL_PORT=" + tunnelPortStr,
				"DASHBOARD_PORT=" + dashboardPortStr,
			},
		},
		&container.HostConfig{
			Privileged: true,
			Binds:      []string{"/var/run/docker.sock:/var/run/docker.sock"},
			PortBindings: nat.PortMap{
				tunnelContainerPort:   []nat.PortBinding{{HostIP: "127.0.0.1", HostPort: tunnelPortStr}},
				dashboardContainerPort: []nat.PortBinding{{HostIP: "127.0.0.1", HostPort: dashboardPortStr}},
			},
			RestartPolicy: container.RestartPolicy{Name: "unless-stopped"},
		},
		nil, nil, w.gatewayName,
	)
	if err != nil {
		// A 409 Conflict means a concurrent caller already created the container
		// (TOCTOU race). Treat it as "already exists" and return that container's ID.
		if client.IsErrNotFound(err) == false && isConflictError(err) {
			info, inspectErr := w.cli.ContainerInspect(ctx, w.gatewayName)
			if inspectErr != nil {
				return "", fmt.Errorf("create gateway (conflict) inspect: %w", inspectErr)
			}
			slog.Info("gateway container already exists (conflict)", "id", info.ID[:12])
			return info.ID, nil
		}
		return "", fmt.Errorf("create gateway: %w", err)
	}
	if err := w.cli.ContainerStart(ctx, resp.ID, container.StartOptions{}); err != nil {
		return "", fmt.Errorf("start gateway: %w", err)
	}
	slog.Info("gateway container started", "id", resp.ID[:12])
	return resp.ID, nil
}

// isConflictError reports whether err is a Docker 409 Conflict response.
func isConflictError(err error) bool {
	return err != nil && strings.Contains(err.Error(), "409")
}

// ConnectGatewayToNetworks connects the gateway container to all bridge networks
// and disconnects it from any networks that no longer exist.
func (w *Watcher) ConnectGatewayToNetworks(ctx context.Context, gatewayID string) error {
	subnets, err := w.Subnets(ctx)
	if err != nil {
		return err
	}

	// Build the desired set of bridge network IDs.
	desired := make(map[string]bool, len(subnets))
	for _, s := range subnets {
		desired[s.NetworkID] = true
	}

	// Inspect the gateway to learn which networks it is currently connected to.
	info, err := w.cli.ContainerInspect(ctx, gatewayID)
	if err != nil {
		return err
	}
	// connected maps networkID → network name for logging.
	connected := make(map[string]string, len(info.NetworkSettings.Networks))
	for netName, ep := range info.NetworkSettings.Networks {
		connected[ep.NetworkID] = netName
	}

	// Disconnect from networks that no longer exist.
	for netID, netName := range connected {
		if desired[netID] {
			continue
		}
		if err := w.cli.NetworkDisconnect(ctx, netID, gatewayID, false); err != nil {
			slog.Warn("failed to disconnect gateway from stale network", "network", netName, "error", err)
			continue
		}
		slog.Info("gateway disconnected from removed network", "network", netName)
	}

	// Connect to networks we are not yet part of.
	for _, s := range subnets {
		if _, ok := connected[s.NetworkID]; ok {
			continue
		}
		if err := w.cli.NetworkConnect(ctx, s.NetworkID, gatewayID, &network.EndpointSettings{}); err != nil {
			slog.Warn("failed to connect gateway to network", "network", s.NetworkName, "error", err)
			continue
		}
		slog.Info("gateway connected to network", "network", s.NetworkName, "cidr", s.CIDR)
	}
	return nil
}

// RemoveGateway stops and removes the gateway container.
func (w *Watcher) RemoveGateway(ctx context.Context) error {
	return w.cli.ContainerRemove(ctx, w.gatewayName, container.RemoveOptions{Force: true})
}

// WatchEvents streams Docker events and calls onChange for container/network events.
func (w *Watcher) WatchEvents(ctx context.Context, onChange func()) {
	eventCh, errCh := w.cli.Events(ctx, events.ListOptions{
		Filters: filters.NewArgs(
			filters.Arg("type", "container"),
			filters.Arg("type", "network"),
		),
	})

	for {
		select {
		case <-ctx.Done():
			return
		case err := <-errCh:
			if err != nil {
				slog.Error("docker events error", "error", err)
			}
			return
		case ev := <-eventCh:
			switch ev.Action {
			case "start", "stop", "die", "connect", "disconnect", "create", "destroy":
				slog.Debug("docker event", "type", ev.Type, "action", ev.Action)
				onChange()
			}
		}
	}
}
