# docker-reach

![Platform: Windows](https://img.shields.io/badge/platform-Windows%2010%2F11-0078D4)
![Language: Go](https://img.shields.io/badge/language-Go%201.22%2B-00ADD8)
![License: MIT](https://img.shields.io/badge/license-MIT-green)
![Status: Beta](https://img.shields.io/badge/status-beta-orange)

Access Docker Desktop containers directly by IP and name from the Windows host -- no `-p` port mapping required.

[中文文档](README_ZH.md) | [Architecture Deep Dive](docs/ARCHITECTURE.md)

---

## The Problem

Docker Desktop for Windows runs containers inside a hidden WSL2 VM. Container networks (`172.17.0.0/16`, etc.) are not routed to Windows. You cannot `ping`, `curl`, or connect to any container without `-p` port mapping.

Every existing workaround (`--net host`, WSL2 mirrored networking, `desktop-docker-connector`, Tailscale subnet router, SOCKS proxy) either doesn't work on Docker Desktop or requires per-application configuration.

## The Solution

docker-reach creates a lightweight IP tunnel between Windows and a gateway container that has real L2 connectivity to all Docker bridge networks. Your normal internet traffic is completely unaffected -- only packets destined for Docker subnets go through the tunnel.

For architecture diagrams and technical details, see [Architecture](docs/ARCHITECTURE.md).

---

## Quick Start

### Prerequisites

- Windows 10/11 with Docker Desktop (WSL2 backend)
- Administrator privileges
- `wintun.dll` from [wintun.net](https://www.wintun.net/), placed next to the exe

### Build

No Go installation required -- everything builds inside Docker.

```powershell
git clone https://github.com/AnFuran/docker-reach.git
cd docker-reach

# Build the gateway image
docker build -t docker-reach-gateway:latest .

# Build the Windows CLI (no local Go needed)
docker run --rm -v "${PWD}:/src" -w /src golang:1.22-alpine sh -c "CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -o docker-reach.exe ./cmd/docker-reach"
```

### Run

```powershell
# As Administrator
.\docker-reach.exe up
```

### Use

```powershell
# By IP
curl http://172.17.0.3:8080
ping 172.17.0.3

# By container name
curl http://my-api.docker:3000
ping my-api.docker
```

### Stop

```powershell
# Ctrl+C in the running terminal, or:
.\docker-reach.exe down
```

---

## Features

- **Direct IP access** -- any container on any Docker bridge network, no port mapping
- **Name resolution** -- `<container-name>.docker` resolves automatically via hosts file
- **Multi-network** -- gateway auto-joins all bridge networks, including ones created after startup
- **Live updates** -- containers start/stop and networks created/removed are detected in real time
- **Zero interference** -- does not touch your internet routes, proxy, VPN, or WSL2 connectivity
- **Clean shutdown** -- Ctrl+C removes the adapter, routes, hosts entries, and gateway container

---

## Commands

| Command | Description |
|---------|-------------|
| `docker-reach up` | Start the tunnel (requires Administrator). Blocks until Ctrl+C. |
| `docker-reach down` | Stop and clean up. Use after a crash or forced kill. |
| `docker-reach status` | Show tunnel state, subnets, and container name-to-IP table. |

Example `status` output:

```
Tunnel:     connected
Subnets:    2
  bridge               172.17.0.0/16
  dev-net              172.18.0.0/16
Containers: 3
  my-api.docker                  172.18.0.3
  my-worker.docker               172.18.0.4
  standalone.docker              172.17.0.3
```

---

## How It Works (briefly)

1. A **gateway container** joins every Docker bridge network, gaining real Ethernet interfaces
2. A **WinTUN virtual adapter** on Windows captures packets destined for Docker subnets
3. Packets are tunneled over a **TCP connection** (`localhost:9999`) between the daemon and gateway
4. The gateway uses **kernel IP forwarding + NAT** to deliver packets to the target container
5. A **Docker event watcher** keeps routes and hosts entries in sync as containers come and go

For the full technical deep dive, see [Architecture](docs/ARCHITECTURE.md).

---

## Known Limitations

- Windows only (macOS/Linux can reach container IPs natively)
- IPv4 only
- Requires Administrator
- `wintun.dll` must be in the same directory as the exe
- Port 9999 must be free

---

## License

MIT. See [LICENSE](LICENSE).

---

This project was built with the assistance of [Claude Code](https://claude.ai/code) (Anthropic Claude Opus 4.6).
