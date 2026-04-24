# Reachistorian

Reachistorian is a lightweight uptime and latency collector for home labs and production-style networks.

It continuously pings devices inside and outside your network, then pushes metrics to VictoriaMetrics for long-term storage and visualization in Grafana.

## What It Monitors

- Device availability via the `host_up` metric (1 = reachable, 0 = unreachable)
- Round-trip latency via the `host_rtt` metric (microseconds)
- Targets defined by IP or hostname (for example: router, DNS, cloud services)

## Stack

- Go collector with configurable ping behavior
- VictoriaMetrics as the metrics backend
- Grafana dashboards provisioned from this repository

## Quick Start (Docker Compose)

1. Edit `config.yaml` in the project root.
2. Start the full stack:

```bash
docker compose up -d --build
```

3. Open services:
- VictoriaMetrics: http://localhost:8428
- Grafana: http://localhost:3000

4. Verify collector logs:

```bash
docker logs -f reachistorian-collector
```

If configuration is valid, the collector starts pinging and pushing metrics automatically.

## Configure config.yaml

The collector reads `config.yaml` from its working directory.
With Docker Compose in this repo, the root `config.yaml` is mounted into the container at `/app/config.yaml`.

### Full Example

```yaml
push_url: http://reachistorian-db:8428/api/v1/import/prometheus
interval: 1s
timeout: 1s
bind_addr4: "0.0.0.0"
bind_addr6: "::"
payload_size: 32
devices:
  - host: 192.168.1.1
    name: Router
  - host: 1.1.1.1
    name: Cloudflare DNS
  - host: www.google.com
    name: Google
```

### Field Reference

| Key | Required | Type | Default | Description |
| --- | --- | --- | --- | --- |
| `push_url` | Yes | string | - | VictoriaMetrics import endpoint. |
| `interval` | No | duration | `1s` | Global ping and push cadence. |
| `timeout` | No | duration | `1s` | Default timeout for DNS resolution and ping per device (unless overridden). |
| `bind_addr4` | No | string | `0.0.0.0` | Local IPv4 source address for ICMP sockets. |
| `bind_addr6` | No | string | `::` | Local IPv6 source address for ICMP sockets. |
| `payload_size` | No | int | `32` | ICMP payload size in bytes. |
| `devices` | Yes | list | - | List of hosts to monitor. Must contain at least one device. |

Each device entry supports:

| Key | Required | Type | Default | Description |
| --- | --- | --- | --- | --- |
| `host` | Yes | string | - | Hostname or IP to monitor. |
| `name` | No | string | same as `host` | Label shown in metrics and dashboards. |
| `timeout` | No | duration | inherits global `timeout` | Per-device timeout override. |

### Duration Format

Duration values use Go duration syntax, for example:

- `500ms`
- `1s`
- `2s`
- `1m30s`

## Running the Collector Without Docker

Use this when you already have VictoriaMetrics and Grafana running elsewhere.

1. Configure `config.yaml` and set `push_url` to your VictoriaMetrics endpoint.
2. Run the collector:

```bash
go run .
```

Or build and run:

```bash
go build -o reachistorian ./main.go
./reachistorian
```

Note: ICMP ping requires raw socket privileges.
On Linux, the recommended approach is to grant the collector executable capability instead of running as root:

```bash
sudo setcap cap_net_raw+ep ./reachistorian
```

In Docker, this repository already uses the equivalent container capability in `compose.yaml` via `cap_add: [NET_RAW]` for the collector service.

## Configuration Reloads (No Restart Needed)

Reachistorian automatically reloads `config.yaml` while running.

- The collector re-reads configuration every 30 seconds.
- If the new configuration is valid, it is applied immediately.
- If invalid, the previous working configuration is kept and an error is logged.

You do not need to restart the collector after editing `config.yaml`.

## Metrics Written

- `host_up{host="...",name="..."}`
- `host_rtt{host="...",name="..."}`

Timestamps are written in Unix milliseconds and pushed in Prometheus text format to VictoriaMetrics.
