// Package config loads docker-reach configuration from a YAML file,
// falling back to built-in defaults for every missing field.
package config

import (
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// Config holds all tuneable parameters for docker-reach.
// Every field is optional in the YAML file; omitted fields keep their defaults.
type Config struct {
	TunnelPort          int    `yaml:"tunnel_port"`
	DashboardPort       int    `yaml:"dashboard_port"`
	TunSubnet           string `yaml:"tun_subnet"`
	GatewayImage        string `yaml:"gateway_image"`
	GatewayName         string `yaml:"gateway_name"`
	AdapterName         string `yaml:"adapter_name"`
	LogFile             string `yaml:"log_file"`
	WSLProtectSubnet    string `yaml:"wsl_protect_subnet"`
	DockerDefaultBridge string `yaml:"docker_default_bridge"`

	// detectedWSL is set at runtime by calling SetWSLSubnet after detecting
	// the actual WSL2 network interface. Not persisted to YAML.
	detectedWSL *net.IPNet `yaml:"-"`
}

// Default returns a Config populated with all default values.
func Default() *Config {
	return &Config{
		TunnelPort:          9999,
		DashboardPort:       9998,
		TunSubnet:           "10.0.85.0/24",
		GatewayImage:        "docker-reach-gateway:latest",
		GatewayName:         "docker-reach-gateway",
		AdapterName:         "DockerReach",
		LogFile:             "docker-reach.log",
		WSLProtectSubnet:    "172.16.0.0/12",
		DockerDefaultBridge: "172.17.0.0/16",
	}
}

// Load reads a YAML config file at path and merges it with defaults.
// If the file does not exist, defaults are returned with no error.
func Load(path string) (*Config, error) {
	cfg := Default()

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return cfg, nil
		}
		return cfg, fmt.Errorf("config: read %s: %w", path, err)
	}

	// Unmarshal into a partial struct; yaml.v3 only overwrites fields present
	// in the file, but we need explicit zero-value detection, so we use a
	// temporary struct and copy non-zero values manually.
	var partial Config
	if err := yaml.Unmarshal(data, &partial); err != nil {
		return cfg, fmt.Errorf("config: parse %s: %w", path, err)
	}

	if partial.TunnelPort != 0 {
		cfg.TunnelPort = partial.TunnelPort
	}
	if partial.DashboardPort != 0 {
		cfg.DashboardPort = partial.DashboardPort
	}
	if partial.TunSubnet != "" {
		cfg.TunSubnet = partial.TunSubnet
	}
	if partial.GatewayImage != "" {
		cfg.GatewayImage = partial.GatewayImage
	}
	if partial.GatewayName != "" {
		cfg.GatewayName = partial.GatewayName
	}
	if partial.AdapterName != "" {
		cfg.AdapterName = partial.AdapterName
	}
	if partial.LogFile != "" {
		cfg.LogFile = partial.LogFile
	}
	if partial.WSLProtectSubnet != "" {
		cfg.WSLProtectSubnet = partial.WSLProtectSubnet
	}
	if partial.DockerDefaultBridge != "" {
		cfg.DockerDefaultBridge = partial.DockerDefaultBridge
	}

	return cfg, nil
}

// tunIP parses TunSubnet and returns the IP at the given host offset (.1, .2, …).
func (c *Config) tunIP(offset int) string {
	ip, _, err := net.ParseCIDR(c.TunSubnet)
	if err != nil {
		return ""
	}
	// Work on a 4-byte copy so we don't mutate the parsed IP.
	ip4 := ip.To4()
	if ip4 == nil {
		return ""
	}
	base := make(net.IP, 4)
	copy(base, ip4)
	base[3] = byte(offset)
	return base.String()
}

// TunGatewayIP returns the .1 address of TunSubnet (used as the Linux gateway).
func (c *Config) TunGatewayIP() string {
	return c.tunIP(1)
}

// TunLocalIP returns the .2 address of TunSubnet (assigned to the WinTUN adapter).
func (c *Config) TunLocalIP() string {
	return c.tunIP(2)
}

// TunMask returns the dotted-decimal subnet mask derived from TunSubnet.
func (c *Config) TunMask() string {
	_, ipnet, err := net.ParseCIDR(c.TunSubnet)
	if err != nil {
		return ""
	}
	return net.IP(ipnet.Mask).String()
}

// TunnelAddr returns the TCP address the Windows side uses to reach the tunnel.
func (c *Config) TunnelAddr() string {
	return "127.0.0.1:" + strconv.Itoa(c.TunnelPort)
}

// TunCIDR returns the gateway-side CIDR for the TUN device (e.g. "10.0.85.1/24").
func (c *Config) TunCIDR() string {
	_, ipnet, err := net.ParseCIDR(c.TunSubnet)
	if err != nil {
		return c.TunSubnet
	}
	ones, _ := ipnet.Mask.Size()
	return fmt.Sprintf("%s/%d", c.TunGatewayIP(), ones)
}

// TunSubnetCIDR returns the subnet in CIDR notation without a host address
// (e.g. "10.0.85.0/24"), suitable for iptables MASQUERADE rules.
func (c *Config) TunSubnetCIDR() string {
	_, ipnet, err := net.ParseCIDR(c.TunSubnet)
	if err != nil {
		return c.TunSubnet
	}
	return ipnet.String()
}

// OverlapsWSL reports whether cidr would conflict with the WSL2 address space.
// If a live WSL subnet has been detected (via SetWSLSubnet), only that specific
// subnet is protected. Otherwise falls back to the broad WSLProtectSubnet from
// config, exempting DockerDefaultBridge.
func (c *Config) OverlapsWSL(cidr *net.IPNet) bool {
	// Prefer the runtime-detected WSL subnet (exact match).
	if c.detectedWSL != nil {
		return c.detectedWSL.Contains(cidr.IP) && cidr.Contains(c.detectedWSL.IP)
	}

	// Fallback: broad protection with Docker default bridge exemption.
	_, wslBlock, err := net.ParseCIDR(c.WSLProtectSubnet)
	if err != nil {
		return false
	}
	_, dockerDefault, err := net.ParseCIDR(c.DockerDefaultBridge)
	if err != nil {
		return false
	}
	if dockerDefault.Contains(cidr.IP) {
		return false
	}
	return wslBlock.Contains(cidr.IP)
}

// SetWSLSubnet stores the runtime-detected WSL2 subnet so OverlapsWSL can
// check against the real subnet rather than the broad /12 fallback.
func (c *Config) SetWSLSubnet(cidr *net.IPNet) {
	c.detectedWSL = cidr
}

// DashboardPortStr returns the dashboard listen address string (e.g. ":9998").
func (c *Config) DashboardPortStr() string {
	return ":" + strconv.Itoa(c.DashboardPort)
}

// TunnelPortStr returns just the port as a colon-prefixed string (e.g. ":9999").
func (c *Config) TunnelPortStr() string {
	return ":" + strconv.Itoa(c.TunnelPort)
}

// TunnelCheckAddr returns the address used by the dashboard to health-check the
// tunnel connection (127.0.0.1:<port>).
func (c *Config) TunnelCheckAddr() string {
	return strings.Replace(c.TunnelAddr(), "127.0.0.1:", "127.0.0.1:", 1)
}
