// Package web provides an HTTP dashboard served from inside the gateway container.
// It queries the Docker daemon via /var/run/docker.sock.
package web

import (
	"context"
	"embed"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/client"
)

const gatewayName = "docker-reach-gateway"

//go:embed dashboard.html
var dashboardFS embed.FS

type PortInfo struct {
	PrivatePort int    `json:"private_port"`
	PublicPort  int    `json:"public_port,omitempty"`
	Type        string `json:"type"`
}

type ContainerInfo struct {
	Name    string     `json:"name"`
	IP      string     `json:"ip"`
	Network string     `json:"network"`
	Status  string     `json:"status"`
	Created string     `json:"created"`
	Ports   []PortInfo `json:"ports"`
	Image   string     `json:"image"`
}

type SubnetInfo struct {
	Name string `json:"name"`
	CIDR string `json:"cidr"`
}

type GatewayInfo struct {
	Status string `json:"status"`
	IP     string `json:"ip,omitempty"`
}

type StatusResponse struct {
	TunnelConnected bool            `json:"tunnel_connected"`
	UptimeSeconds   int64           `json:"uptime_seconds"`
	Subnets         []SubnetInfo    `json:"subnets"`
	Containers      []ContainerInfo `json:"containers"`
	Gateway         *GatewayInfo    `json:"gateway,omitempty"`
}

type Server struct {
	cli       *client.Client
	startTime time.Time
	srv       *http.Server
}

// New creates a dashboard server listening on the given port (e.g. "9998").
func New(port string) (*Server, error) {
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, err
	}

	s := &Server{
		cli:       cli,
		startTime: time.Now(),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/status", s.handleStatus)
	mux.HandleFunc("/", s.handleDashboard)

	addr := ":" + port
	s.srv = &http.Server{
		Addr:         addr,
		Handler:      mux,
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 10 * time.Second,
	}
	return s, nil
}

func (s *Server) Start() {
	go func() {
		slog.Info("dashboard listening", "addr", s.srv.Addr)
		if err := s.srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("dashboard error", "error", err)
		}
	}()
}

func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	data, _ := dashboardFS.ReadFile("dashboard.html")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(data)
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	resp := StatusResponse{
		UptimeSeconds: int64(time.Since(s.startTime).Seconds()),
		Subnets:       []SubnetInfo{},
		Containers:    []ContainerInfo{},
	}

	// Tunnel check — use the configured port from env (defaults to 9999).
	tunnelPort := os.Getenv("TUNNEL_PORT")
	if tunnelPort == "" {
		tunnelPort = "9999"
	}
	conn, err := net.DialTimeout("tcp", "127.0.0.1:"+tunnelPort, 500*time.Millisecond)
	if err == nil {
		conn.Close()
		resp.TunnelConnected = true
	}

	// Subnets
	networks, _ := s.cli.NetworkList(ctx, network.ListOptions{})
	for _, n := range networks {
		if n.Driver != "bridge" {
			continue
		}
		for _, cfg := range n.IPAM.Config {
			resp.Subnets = append(resp.Subnets, SubnetInfo{Name: n.Name, CIDR: cfg.Subnet})
		}
	}

	// Containers
	containers, _ := s.cli.ContainerList(ctx, container.ListOptions{All: true})
	for _, c := range containers {
		name := primaryName(c.Names)
		isGateway := name == gatewayName

		var chosenIP, chosenNet string
		for netName, ep := range c.NetworkSettings.Networks {
			if ep.IPAddress == "" {
				continue
			}
			if chosenIP == "" || netName == "bridge" {
				chosenIP = ep.IPAddress
				chosenNet = netName
				if netName == "bridge" {
					break
				}
			}
		}

		var ports []PortInfo
		for _, p := range c.Ports {
			ports = append(ports, PortInfo{
				PrivatePort: int(p.PrivatePort),
				PublicPort:  int(p.PublicPort),
				Type:        p.Type,
			})
		}
		if ports == nil {
			ports = []PortInfo{}
		}

		created := ""
		if c.Created > 0 {
			created = time.Unix(c.Created, 0).UTC().Format(time.RFC3339)
		}

		ci := ContainerInfo{
			Name:    name,
			IP:      chosenIP,
			Network: chosenNet,
			Status:  c.State,
			Created: created,
			Ports:   ports,
			Image:   c.Image,
		}

		if isGateway {
			resp.Gateway = &GatewayInfo{Status: c.State, IP: chosenIP}
		}
		resp.Containers = append(resp.Containers, ci)
	}
	if resp.Containers == nil {
		resp.Containers = []ContainerInfo{}
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-cache")
	json.NewEncoder(w).Encode(resp)
}

func primaryName(names []string) string {
	for _, n := range names {
		return strings.TrimPrefix(n, "/")
	}
	return ""
}
