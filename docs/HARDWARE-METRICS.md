# Hardware metrics

Every host publishes its power, GPU and temperature readings as the same Prometheus series,
whatever its sensors are, so energy per request (or per agent run) is one query across the
lab. The names follow the OpenTelemetry `hw.*` semantic conventions, with dots as
underscores.

| Series | OpenTelemetry | Labels |
|---|---|---|
| `hw_power_watts` | `hw.power` | `device` (`gpu0`, `cpu0`, `board`, a rail name), `kind` |
| `hw_gpu_utilization_ratio` | `hw.gpu.utilization` | `device` |
| `hw_gpu_memory_used_bytes` | `hw.gpu.memory.usage` | `device` |
| `hw_gpu_memory_limit_bytes` | `hw.gpu.memory.limit` | `device` |
| `hw_temperature_celsius` | `hw.temperature` | `device`, `kind`, `sensor` (`edge`, `junction`, `mem`, `tctl`, ...) |

`kind` is `gpu` (one card's board power), `cpu` (the CPU package), `host` (the whole system,
only where a sensor measures it) or `rail` (one supply rail, e.g. a Jetson's `vdd_soc`). A
reading the hardware doesn't report is left out, never zero. GPU temperature sensors use the
amdgpu hwmon names on every OS, so `sensor="junction"` is the hotspot on Windows and Linux
alike.

## Where they come from

| Host | Source | How it's published |
|---|---|---|
| Windows (desktop) | LibreHardwareMonitor's web server (`:8085`) | The gateway's `/metrics/host` relay translates it: the original `lhm_*` series plus the `hw_*` ones |
| Linux GPU box | amdgpu hwmon: `power1_average` (else `power1_input`, in microwatts), `gpu_busy_percent`, `mem_info_vram_*`, the `edge`/`junction`/`mem` temperatures; k10temp `Tctl` | `hwmetrics` writes `hw.prom` for node_exporter's textfile collector |
| Jetson | The INA3221 rails (`VDD_IN` is the whole board, `kind="host"`), GPU load, the gpu/cpu thermal zones | the same `hwmetrics` binary, built for arm64 |

Linux hosts use node_exporter's textfile collector rather than another listener: node_exporter
already runs and is scraped on both kinds of box, so there's no new port, firewall rule or
scrape job, and the Jetson's temperature script already works this way. `hwmetrics` reads sysfs
directly (no `rocm-smi` or `tegrastats` process), rewrites the file atomically every 15 s, and
puts a 2 s timeout on every read, because some Jetson thermal zones never answer.

What each host can't measure:

- An APU's integrated GPU reports the whole package's power through amdgpu, so `hwmetrics`
  skips any amdgpu card without a `junction` sensor rather than count the CPU twice.
- The Jetson's GPU has no rail of its own (it shares `VDD_CPU_GPU_CV` with the CPU and vision
  engines) and no memory of its own, so it reports utilization and temperature only. Its
  board power is `hw_power_watts{device="board",kind="host"}`.
- Neither the desktop nor the server has a whole-system sensor, so they publish no
  `kind="host"` series; the energy rules add estimated overheads (below). A smart plug or UPS
  that reports watts would fill that in.

Install:

```sh
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o hwmetrics ./cmd/hwmetrics   # GOARCH=arm64 for a Jetson
cp hwmetrics ~/bin/ && ./workers/linux/install.sh metrics                     # or: workers/jetson/setup.sh services
hwmetrics -once -textfile -                                                   # what it sees
```

The `deploy/sff` stack's node_exporter reads `TEXTFILE_DIR` (default `~/.node-textfile`).

## The host label

The `hw_*` exposition carries no `host` label. Prometheus adds it from the scrape target, set in
`deploy/sff/.env`: `DESKTOP_HOST` for the gateway's `/metrics/host`, `SERVER_HOST` (default:
the box's hostname) for the local node_exporter, and the `name=` part of `EXTRA_NODES` for
other boxes. Make these the same names that traces use as `host.name`
(`OTEL_RESOURCE_ATTRIBUTES=host.name=desktop`), so a slow request and the watts on its
worker's host join on one label. LibreHardwareMonitor's own `host` label (the Windows machine
name) is kept as `exported_host`.

## Energy

```promql
# GPU energy over the last hour, in watt-hours, per host
sum by (host) (avg_over_time(hw_power_watts{kind="gpu"}[1h]))
```

`lab:power_watts{box}` (in `deploy/sff/up.sh`) is each box's estimated wall power: the measured
`hw_power_watts` plus the parts no sensor sees (board, RAM, drives, fans, and the server's CPU,
whose power counter isn't readable), divided by the power supply's efficiency. It falls back
to the raw `lhm_*` and `node_hwmon_*` sensors on a host that doesn't publish `hw_*` yet.

## Idle baselines

Most of a home lab's energy is the floor it draws doing nothing. To charge a request only for
what it added, set each box's idle draw in `.env` (`DESKTOP_IDLE_W`, `SFF_IDLE_W`,
`JETSON_IDLE_W`, in watts at the wall); `up.sh` then records `lab:idle_watts{box}` and
`lab:busy_watts{box}`, the draw above idle.

Measure it after a quiet week rather than guessing: the 5th percentile is the box at rest with
its models loaded.

```promql
quantile_over_time(0.05, lab:power_watts[7d])                 # per box, at the wall
quantile_over_time(0.05, hw_power_watts{kind="gpu"}[7d])      # per card
```

Readings on this lab's desktop at rest, with a 12B model loaded (10.5 GB of VRAM):

| Component | Idle | Note |
|---|---|---|
| RX 6900 XT board | 17 W | Its memory clock stays at full speed (about 2 GHz) at idle on this setup; a card whose memory clock drops idles lower |
| Ryzen 9 5950X package | 65 W | At about 1% CPU load |

These are the sensors' figures, not the wall: add the overheads and PSU loss for
`DESKTOP_IDLE_W`. Re-measure after driver, BIOS or display changes, which move the memory clock
and with it the floor.

## Solar

`envoyexporter` publishes `net_grid_import_watts`: `max(0, consumption − production)`, the
house's draw the panels aren't covering right now. Energy used while it is 0 came from solar,
so a cost estimate can count it as free:

```promql
lab:power_watts:sum * on () clamp_max(net_grid_import_watts / (envoy_consumption_watts > 0), 1)
```

is the lab's share of the grid draw, in watts, under the assumption that solar covers every
load in the house in proportion.
