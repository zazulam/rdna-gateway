#!/usr/bin/env python3
"""Writes dashboards/inference.json. Edit here, then rerun:

    python3 deploy/sff/grafana/dashboard.py
"""
import json
from pathlib import Path

DS = {"type": "prometheus", "uid": "prometheus"}
panels = []
y = 0


def row(title):
    global y
    panels.append({"type": "row", "title": title, "collapsed": False,
                   "gridPos": {"h": 1, "w": 24, "x": 0, "y": y}, "id": len(panels) + 1})
    y += 1


# One color per worker on every panel, matching the deck.
WORKER_COLORS = {"desktop-6900xt": "#ff6b7f", "server-6700xt": "#2ec4d6", "server-cpu": "#f6c35f",
                 "embed": "green", "rerank": "purple"}


def overrides(kind):
    if kind != "timeseries":
        return []
    out = [{"matcher": {"id": "byRegexp", "options": f"^{w}.*"},
            "properties": [{"id": "color", "value": {"mode": "fixed", "fixedColor": c}}]}
           for w, c in WORKER_COLORS.items()]
    out.append({"matcher": {"id": "byRegexp", "options": ".*(cached|queued|slots|total)$"},
                "properties": [{"id": "custom.lineStyle", "value": {"fill": "dash", "dash": [6, 4]}},
                               {"id": "custom.fillOpacity", "value": 0}]})
    return out


def panel(kind, title, targets, x, w, h, unit=None, desc=None, stack=False, minv=None, maxv=None, mappings=None,
          thresholds=None, decimals=None, lines=None):
    defaults = {}
    if decimals is not None:
        defaults["decimals"] = decimals
    if unit:
        defaults["unit"] = unit
    if minv is not None:
        defaults["min"] = minv
    if maxv is not None:
        defaults["max"] = maxv
    if mappings:
        defaults["mappings"] = mappings
    if thresholds:
        defaults["thresholds"] = {"mode": "absolute", "steps": thresholds}
        defaults["color"] = {"mode": "thresholds"}
    if kind == "timeseries":
        defaults["custom"] = {"fillOpacity": 12, "lineWidth": 2, "showPoints": "never",
                              "stacking": {"mode": "normal" if stack else "none"}}
    if lines:  # dashed threshold lines, e.g. warning/critical temperatures
        defaults["thresholds"] = {"mode": "absolute", "steps": [{"color": "transparent", "value": None}] +
                                  [{"color": c, "value": v} for v, c in lines]}
        defaults["custom"]["thresholdsStyle"] = {"mode": "dashed"}
    p = {
        "type": kind, "title": title, "datasource": DS, "id": len(panels) + 1,
        "gridPos": {"h": h, "w": w, "x": x, "y": y},
        "fieldConfig": {"defaults": defaults, "overrides": overrides(kind)},
        "targets": [{"datasource": DS, "refId": chr(65 + i), "expr": e, "legendFormat": l}
                    for i, (e, l) in enumerate(targets)],
    }
    if desc:
        p["description"] = desc
    if kind == "timeseries":
        p["options"] = {"legend": {"displayMode": "list", "placement": "bottom"}, "tooltip": {"mode": "multi"}}
    if kind == "stat":
        p["options"] = {"reduceOptions": {"calcs": ["lastNotNull"]}, "colorMode": "value", "graphMode": "area",
                        "textMode": "value_and_name" if len(targets) > 1 or "{{" in targets[0][1] else "auto"}
    panels.append(p)


R = "$__rate_interval"
UPDOWN = [{"type": "value", "options": {"0": {"text": "down", "color": "red"}, "1": {"text": "up", "color": "green"}}}]
GOOD_HIGH = [{"color": "red", "value": None}, {"color": "orange", "value": 40}, {"color": "green", "value": 70}]

row("Pool")
panel("stat", "Workers", [("rdnagw_worker_up", "{{worker}}")], 0, 7, 4, mappings=UPDOWN,
      desc="Gateway health checks, every 5 s.")
panel("stat", "Output tok/s (all workers)", [(f"sum(rate(llamacpp:tokens_predicted_total[{R}]))", "")], 7, 5, 4,
      unit="none", thresholds=[{"color": "blue", "value": None}])
panel("stat", "Prompt cache hit (1h)",
      [("100 * sum(increase(llamacpp:prompt_tokens_cached_total[1h])) / "
        "(sum(increase(llamacpp:prompt_tokens_cached_total[1h])) + sum(increase(llamacpp:prompt_tokens_total[1h])))", "")],
      12, 4, 4, unit="percent", thresholds=GOOD_HIGH,
      desc="Share of prompt tokens reused from the KV / host-RAM cache instead of re-processed.")
panel("stat", "Gateway requests (24h)", [("sum(increase(rdnagw_worker_requests_total[24h]))", "")], 16, 4, 4,
      decimals=0, thresholds=[{"color": "blue", "value": None}])
panel("stat", "Gateway failures (24h)", [("sum(increase(rdnagw_worker_failures_total[24h]))", "")], 20, 4, 4,
      decimals=0, thresholds=[{"color": "green", "value": None}, {"color": "red", "value": 1}])
y += 4

panel("timeseries", "Output tok/s by worker", [(f"sum by (worker) (rate(llamacpp:tokens_predicted_total[{R}]))", "{{worker}}")],
      0, 12, 8, stack=True, desc="Generated tokens per second, all traffic to each llama-server.")
panel("timeseries", "Prompt tok/s: processed vs from cache",
      [(f"sum by (worker) (rate(llamacpp:prompt_tokens_total[{R}]))", "{{worker}} processed"),
       (f"sum by (worker) (rate(llamacpp:prompt_tokens_cached_total[{R}]))", "{{worker}} cached")],
      12, 12, 8, desc="Processed tokens cost prefill time; cached ones are nearly free.")
y += 8

panel("timeseries", "Busy slots and queue",
      [("llamacpp:requests_processing", "{{worker}} busy"), ("llamacpp:requests_deferred", "{{worker}} queued"),
       ("rdnagw_worker_slots", "{{worker}} slots")],
      0, 12, 8, desc="Queued > 0 means agents are waiting for a slot: add slots, a worker, or route elsewhere.")
panel("timeseries", "Decode tok/s per stream",
      [(f"rate(llamacpp:tokens_predicted_total[{R}]) / rate(llamacpp:tokens_predicted_seconds_total[{R}])", "{{worker}}")],
      12, 6, 8, unit="none", desc="Speed of one stream while generating. Falls as more slots are busy.")
panel("timeseries", "Prefill tok/s per stream",
      [(f"rate(llamacpp:prompt_tokens_total[{R}]) / rate(llamacpp:prompt_seconds_total[{R}])", "{{worker}}")],
      18, 6, 8, unit="none", desc="Prompt processing speed on uncached tokens; drives time to first token.")
y += 8

panel("timeseries", "Gateway routing decisions", [(f"sum by (reason) (rate(rdnagw_routed_total[{R}]))", "{{reason}}")],
      0, 8, 8, unit="reqps", stack=True, desc="retry and fallback should stay near zero.")
panel("timeseries", "Gateway requests and failures",
      [(f"sum by (worker) (rate(rdnagw_worker_requests_total[{R}]))", "{{worker}}"),
       (f"sum by (worker) (rate(rdnagw_worker_failures_total[{R}]))", "{{worker}} failed")],
      8, 8, 8, unit="reqps")
panel("timeseries", "Prompt cache hit by worker",
      [(f"100 * rate(llamacpp:prompt_tokens_cached_total[{R}]) / "
        f"(rate(llamacpp:prompt_tokens_cached_total[{R}]) + rate(llamacpp:prompt_tokens_total[{R}]))", "{{worker}}")],
      16, 8, 8, unit="percent", minv=0, maxv=100)
y += 8

row("Agent tools (MCP, retrieval, search)")
panel("timeseries", "Tool calls", [(f"sum by (tool) (rate(rdnamcp_tool_calls_total[{R}]))", "{{tool}}")],
      0, 8, 7, unit="reqps", stack=True)
panel("timeseries", "Tool latency (mean)",
      [(f'sum by (tool) (rate(rdnamcp_tool_seconds_total[{R}])) / sum by (tool) (rate(rdnamcp_tool_calls_total[{R}]))', "{{tool}}")],
      8, 8, 7, unit="s", desc="search_docs includes embedding the query and reranking the candidates on the CPU.")
panel("stat", "Tool errors (24h)", [('sum by (tool) (increase(rdnamcp_tool_calls_total{result="error"}[24h]))', "{{tool}}")],
      16, 4, 7, decimals=0, thresholds=[{"color": "green", "value": None}, {"color": "orange", "value": 1}],
      desc="fetch_url errors include refused private addresses and dead links.")
panel("stat", "Indexed passages", [("sum by (id) (collection_points)", "{{id}}")], 20, 4, 7, decimals=0,
      thresholds=[{"color": "blue", "value": None}])
panels[-1]["targets"][0]["instant"] = True  # only collections that exist now
y += 7
panel("timeseries", "Embedding and rerank tokens/s",
      [(f'sum by (worker) (rate(llamacpp:prompt_tokens_total{{worker=~"embed|rerank"}}[{R}]))', "{{worker}}")],
      0, 12, 7, desc="CPU work done for indexing and searching.")
panel("timeseries", "Qdrant request latency",
      [(f"sum(rate(rest_responses_duration_seconds_sum[{R}])) / sum(rate(rest_responses_duration_seconds_count[{R}]))", "mean")],
      12, 12, 7, unit="s")
y += 7

row("Energy and cost")
BLUE = [{"color": "blue", "value": None}]
panel("stat", "Lab power now", [("lab:power_watts:sum", "")], 0, 4, 5, unit="watt", decimals=0, thresholds=BLUE,
      desc="Desktop + server box + Jetson. GPU/CPU/board power is measured where the hardware reports it; the rest of "
           "each box is an estimate (overheads and PSU efficiency in .env).")
panel("stat", "Energy, last 24 h", [("sum_over_time(lab:power_watts:sum[24h:1m]) / 60 / 1000", "")], 4, 4, 5,
      unit="kwatth", decimals=2, thresholds=BLUE)
panel("stat", "Cost, last 24 h", [("sum_over_time(lab:cost_usd_per_hour[24h:1m]) / 60", "")], 8, 4, 5,
      unit="currencyUSD", decimals=2, thresholds=BLUE,
      desc="At your all-in retail rate. Under retail net metering every kWh the lab uses is worth the retail rate even "
           "while the panels produce (it would otherwise be exported for retail credit); only surplus left at the annual "
           "true-up is credited lower. Reads $0 until ELECTRIC_RATE_SUMMER/WINTER are set in .env.")
panel("stat", "Projected per month", [("avg_over_time(lab:cost_usd_per_hour[24h]) * 730", "")], 12, 4, 5,
      unit="currencyUSD", decimals=2, thresholds=BLUE, desc="Last 24 h average cost per hour x 730 hours.")
panel("stat", "Rate now", [("lab:electric_rate_usd_per_kwh", "")], 16, 4, 5, unit="currencyUSD", decimals=3,
      thresholds=BLUE, desc="$/kWh in effect: the summer rate June-September, otherwise the winter rate.")
panel("stat", "Lab energy from your solar (24 h)", [("sum_over_time(lab:solar_watts[24h:1m]) / 60 / 1000", "")],
      20, 4, 5, unit="kwatth", decimals=2, thresholds=[{"color": "green", "value": None}],
      desc="The lab's share of the house's use times the share of the house covered by solar at that minute. "
           "Needs the gateway's consumption CTs.")
y += 5
panel("timeseries", "Power by box", [("lab:power_watts", "{{box}}")], 0, 12, 8, unit="watt", stack=True, minv=0)
panels[-1]["fieldConfig"]["overrides"] += [
    {"matcher": {"id": "byName", "options": n}, "properties": [{"id": "color", "value": {"mode": "fixed", "fixedColor": c}}]}
    for n, c in [("desktop", "red"), ("server", "blue"), ("jetson", "green")]]
panel("timeseries", "Solar, house and grid", [
    ("envoy_production_watts", "solar"),
    ("envoy_consumption_watts", "house"),
    ("envoy_grid_watts", "grid (+ import / - export)"),
    ("lab:power_watts:sum", "lab")], 12, 12, 8, unit="watt",
      desc="From the Enphase IQ Gateway on the LAN. House and grid need consumption CTs; without them only solar shows.")
y += 8
panel("stat", "Solar gateway", [("envoy_up", "")], 0, 4, 4, mappings=UPDOWN,
      desc="Down: no token in secrets/envoy.token, an expired token (owner tokens last a year), or the gateway is unreachable.")
panel("stat", "Solar covering the house now", [("100 * lab:solar_share", "")], 4, 5, 4, unit="percent", decimals=0,
      thresholds=[{"color": "orange", "value": None}, {"color": "green", "value": 100}])
panel("stat", "Lab share of house use", [("100 * lab:power_watts:sum / on () (envoy_consumption_watts > 0)", "")],
      9, 5, 4, unit="percent", decimals=0, thresholds=BLUE)
panel("stat", "Solar produced (24 h)", [("sum_over_time(envoy_production_watts[24h:1m]) / 60 / 1000", "")],
      14, 5, 4, unit="kwatth", decimals=1, thresholds=[{"color": "green", "value": None}])
panel("stat", "Grid import / export (24 h)", [
    ("sum_over_time(clamp_min(envoy_grid_watts, 0)[24h:1m]) / 60 / 1000", "imported"),
    ("sum_over_time(clamp_min(-envoy_grid_watts, 0)[24h:1m]) / 60 / 1000", "exported")], 19, 5, 4,
      unit="kwatth", decimals=1, thresholds=BLUE)
y += 4

row("Hardware health (server box)")
GPU = 'and on (chip) node_hwmon_sensor_label{label="junction"}'  # the discrete card, not the iGPU
TEMP = "node_hwmon_temp_celsius * on (chip, sensor) group_left (label) node_hwmon_sensor_label"
panel("stat", "GPU junction (max, 24h)", [(f'max_over_time(max({TEMP}{{label="junction"}})[24h:1m])', "")], 0, 4, 6,
      unit="celsius", decimals=0, desc="Hotspot temperature. Warning at 95 °C, critical at 103 °C; the card throttles at 110 °C.",
      thresholds=[{"color": "green", "value": None}, {"color": "orange", "value": 95}, {"color": "red", "value": 103}])
panel("stat", "Minutes above 95 °C (24h)",
      [(f'(sum(count_over_time((max({TEMP}{{label="junction"}}) > 95)[24h:15s])) * 15 / 60) or vector(0)', "")], 4, 4, 6, decimals=1,
      thresholds=[{"color": "green", "value": None}, {"color": "orange", "value": 1}, {"color": "red", "value": 60}],
      desc="Time the GPU hotspot spent above the warning line today.")
panel("alertlist", "Alerts", [], 8, 16, 6, desc="Firing and pending alerts; they also push to ntfy.")
panels[-1]["options"] = {"showOptions": "current", "maxItems": 10, "sortOrder": 1, "stateFilter": {"firing": True, "pending": True,
                         "normal": False, "error": True, "noData": False}, "alertInstanceLabelFilter": "", "folder": None}
panels[-1].pop("targets")
y += 6
panel("timeseries", "GPU temperatures", [(f"({TEMP}{{label=~\"edge|junction|mem\"}}) {GPU}", "{{label}}")],
      0, 8, 8, unit="celsius", lines=[(95, "orange"), (103, "red")],
      desc="junction = hottest point on the die; mem = GDDR6 (rated to 105 °C).")
panel("timeseries", "GPU power and fan", [(f"node_hwmon_power_average_watt {GPU}", "power W"),
                                          (f"node_hwmon_power_cap_watt {GPU}", "power cap W"),
                                          (f"100 * node_hwmon_pwm / node_hwmon_pwm_max {GPU}", "fan duty %")],
      8, 8, 8, desc="The card holds itself under its power cap; fan duty rises with temperature.")
panel("timeseries", "CPU, board and drive temperatures",
      [(f'{TEMP}{{label=~"Tctl|VRM|Motherboard"}}', "{{label}}"),
       ('max(node_hwmon_temp_celsius * on (chip) group_left (chip_name) node_hwmon_chip_names{chip_name="spd5118"})', "RAM"),
       ('max(node_hwmon_temp_celsius{chip=~"nvme.*",host!="jetson"})', "NVMe")],
      16, 8, 8, unit="celsius", lines=[(90, "orange")])
y += 8

row("Hardware health (desktop, via LibreHardwareMonitor)")
LHM_GPU = 'lhm_gpuamd_temperature_celsius{sensorName=~"GPU Core|GPU Hot Spot|GPU Memory.*"}'
panel("stat", "6900 XT hotspot (max, 24h)", [('max_over_time(max(lhm_gpuamd_temperature_celsius{sensorName="GPU Hot Spot"})[24h:1m])', "")],
      0, 4, 6, unit="celsius", decimals=0, desc="Warning at 95 °C, critical at 103 °C; RDNA2 throttles at 110 °C.",
      thresholds=[{"color": "green", "value": None}, {"color": "orange", "value": 95}, {"color": "red", "value": 103}])
panel("stat", "Desktop sensors", [('up{job="desktop-host"}', "")], 4, 4, 6, mappings=UPDOWN,
      desc="Down when the desktop is off, asleep, or LibreHardwareMonitor isn't running.")
panel("timeseries", "6900 XT temperatures", [(LHM_GPU, "{{sensorName}}")], 8, 8, 6, unit="celsius",
      lines=[(95, "orange"), (103, "red")])
panel("timeseries", "6900 XT power, fan, VRAM",
      [('lhm_gpuamd_power_watts{sensorName="GPU Package"}', "power W"), ('lhm_gpuamd_control_percent{sensorName="GPU Fan"}', "fan duty %"),
       ('lhm_gpuamd_smalldata_bytes{sensorName="GPU Memory Used"} / 1e9', "VRAM used GB")],
      16, 8, 6, desc="Fan duty 0 at idle is the card's zero-RPM mode.")
y += 6
panel("timeseries", "5950X and board temperatures",
      [('lhm_cpu_temperature_celsius{sensorName=~"Core .Tctl/Tdie.|CCDs Max .Tdie."}', "{{sensorName}}"),
       ('lhm_motherboard_temperature_celsius', "board {{sensorName}}"), ('max by (hardwareName) (lhm_storage_temperature_celsius{sensorName!~"Warning.*|Critical.*"})', "{{hardwareName}}")],
      0, 12, 7, unit="celsius", lines=[(85, "orange")], desc="Ryzen 5000 limit is 90 °C.")
panel("timeseries", "5950X power and load",
      [('lhm_cpu_power_watts{sensorName="Package"}', "package W"), ('lhm_cpu_load_percent{sensorName="CPU Total"}', "load %")],
      12, 12, 7)
y += 7

row("Hardware health (Jetson: embeddings + reranker)")
JT = 'jetson_temp_celsius'
JN = '{host="jetson"}'
panel("stat", "Jetson junction (max, 24h)", [(f'max_over_time(max({JT}{{zone="tj"}})[24h:1m])', "")], 0, 4, 6, unit="celsius",
      decimals=0, desc="Orin junction temperature. Warning at 85 °C, critical at 95 °C; it throttles near 99 °C.",
      thresholds=[{"color": "green", "value": None}, {"color": "orange", "value": 85}, {"color": "red", "value": 95}])
panel("stat", "Jetson sensors", [('up{host="jetson"}', "")], 4, 4, 6, mappings=UPDOWN,
      desc="node_exporter on the Jetson; temperatures come from a textfile script (jetson-sensors.service).")
panel("timeseries", "Jetson temperatures", [(JT, "{{zone}}")], 8, 8, 6, unit="celsius", lines=[(85, "orange"), (95, "red")])
panel("timeseries", "Jetson power", [
    (f'node_hwmon_in_volts{{host="jetson",sensor="in1"}} * on (chip) group_left node_hwmon_curr_amps{{host="jetson",sensor="curr1"}}', "board W"),
    (f'node_hwmon_in_volts{{host="jetson",sensor="in2"}} * on (chip) group_left node_hwmon_curr_amps{{host="jetson",sensor="curr2"}}', "CPU+GPU W"),
    (f'node_hwmon_in_volts{{host="jetson",sensor="in3"}} * on (chip) group_left node_hwmon_curr_amps{{host="jetson",sensor="curr3"}}', "SoC W")],
      16, 8, 6, unit="watt", desc="From the INA3221 rail monitor. MAXN SUPER mode allows up to 25 W.")
y += 6
panel("timeseries", "Jetson load", [
    ("100 * jetson_gpu_load_ratio", "GPU %"),
    (f'100 * (1 - avg(rate(node_cpu_seconds_total{{host="jetson",mode="idle"}}[{R}])))', "CPU %"),
    (f'100 * node_hwmon_pwm{JN} / 255', "fan duty %")], 0, 12, 7, unit="percent", minv=0, maxv=100)
panel("timeseries", "Jetson memory (shared CPU+GPU)", [
    (f'node_memory_MemTotal_bytes{JN} - node_memory_MemAvailable_bytes{JN}', "used"), (f'node_memory_MemTotal_bytes{JN}', "total")],
      12, 12, 7, unit="bytes", minv=0, desc="Embedder + reranker use about 2 GB; the 8 GB is shared with the GPU.")
y += 7

row("Server box (SFF)")
DGPU = "and on (card) node_drm_memory_vram_size_bytes > 4e9"  # skip the CPU's iGPU
panel("timeseries", "GPU busy", [(f"node_drm_gpu_busy_percent {DGPU}", "GPU")], 0, 6, 7, unit="percent", minv=0, maxv=100)
panel("timeseries", "VRAM used", [(f"node_drm_memory_vram_used_bytes {DGPU}", "used"),
                                  (f"node_drm_memory_vram_size_bytes {DGPU}", "total")], 6, 6, 7, unit="bytes", minv=0,
      desc="The worker holds model + KV for all slots; under 10 GB at 4x32k.")
panel("timeseries", "CPU", [(f'100 * (1 - avg(rate(node_cpu_seconds_total{{mode="idle",host!="jetson"}}[{R}])))', "busy")],
      12, 6, 7, unit="percent", minv=0, maxv=100, desc="The CPU model's work shows up here.")
panel("timeseries", "Memory", [('node_memory_MemTotal_bytes{host!="jetson"} - node_memory_MemAvailable_bytes{host!="jetson"}', "used"),
                               ('node_memory_MemTotal_bytes{host!="jetson"}', "total")], 18, 6, 7, unit="bytes", minv=0,
      desc="Includes the GPU worker's host-RAM prompt cache.")
y += 7

dash = {
    "uid": "rdna-inference", "title": "RDNA inference", "tags": ["rdna-gateway"],
    "timezone": "browser", "refresh": "30s", "schemaVersion": 41, "editable": False,
    "time": {"from": "now-6h", "to": "now"}, "panels": panels,
}
out = Path(__file__).parent / "dashboards" / "inference.json"
out.write_text(json.dumps(dash, indent=1) + "\n")
print("wrote", out)
