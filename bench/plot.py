#!/usr/bin/env python3
"""Render bench/results/*.jsonl into docs/img/*.svg and bench/results/tables.md.

    python3 bench/plot.py
    PLOT_THEME=dark python3 bench/plot.py   # docs/img/dark/*.svg, for the deck
"""
import json
import os
import statistics
from collections import defaultdict
from pathlib import Path

import matplotlib

matplotlib.use("Agg")
import matplotlib.pyplot as plt  # noqa: E402

ROOT = Path(__file__).resolve().parent.parent
RES = ROOT / "bench" / "results"
THEME = os.environ.get("PLOT_THEME", "light")
IMG = ROOT / "docs" / "img" / ("dark" if THEME == "dark" else "")
IMG.mkdir(parents=True, exist_ok=True)

HOSTS = {
    ("desktop", "rocm"): "RX 6900 XT · ROCm",
    ("desktop", "vulkan"): "RX 6900 XT · Vulkan",
    ("server", "rocm"): "RX 6700 XT · ROCm",
    ("mac", "metal"): "M4 Air · Metal",
}
COLORS = {
    ("desktop", "rocm"): "#d1495b",
    ("desktop", "vulkan"): "#edae49",
    ("server", "rocm"): "#00798c",
    ("mac", "metal"): "#7a7a7a",
}
POLICY_COLORS = {
    "round-robin": "#9e9e9e",
    "least-loaded": "#edae49",
    "cache-aware": "#00798c",
    "cost-aware": "#d1495b",
}

plt.rcParams.update({
    "figure.dpi": 110, "font.size": 10, "axes.spines.top": False, "axes.spines.right": False,
    "axes.grid": True, "grid.alpha": 0.25, "legend.frameon": False, "svg.fonttype": "none",
})

# Dark theme: the deck's palette. Series colors are brightened in the written SVG
# so the plotting code keeps one set of literals.
DARK_BG, DARK_FG = "#12151c", "#d7dce4"
DARK_SWAP = {
    "#d1495b": "#ff6b7f", "#00798c": "#2ec4d6", "#edae49": "#f6c35f", "#2a9d8f": "#5ad8c4",
    "#bbbbbb": "#596171", "#9e9e9e": "#6c7586", "#7a7a7a": "#9aa3b2", "#000000": "#c3c9d3",
}
if THEME == "dark":
    plt.rcParams.update({
        "figure.facecolor": DARK_BG, "axes.facecolor": DARK_BG, "savefig.facecolor": DARK_BG,
        "text.color": DARK_FG, "axes.labelcolor": DARK_FG, "axes.titlecolor": DARK_FG,
        "axes.edgecolor": "#4a5160", "xtick.color": DARK_FG, "ytick.color": DARK_FG,
        "grid.color": "#8b93a1",
    })

tables = []


def load(name):
    p = RES / name
    if not p.exists():
        return []
    rows = [json.loads(l) for l in p.read_text().splitlines() if l.strip()]
    if rows and "label" in rows[0]:
        # A re-run supersedes earlier records with the same label.
        latest = {}
        for r in rows:
            latest[r["label"]] = r
        rows = list(latest.values())
    return rows


def save(fig, name, rect=None):
    fig.tight_layout(rect=rect)
    fig.savefig(IMG / name)
    plt.close(fig)
    if THEME == "dark":
        svg = (IMG / name).read_text()
        for a, b in DARK_SWAP.items():
            svg = svg.replace(a, b)
        (IMG / name).write_text(svg)
    print("wrote", IMG / name)


def table(title, header, rows):
    out = [f"### {title}", "", "| " + " | ".join(header) + " |", "|" + "---|" * len(header)]
    out += ["| " + " | ".join(str(c) for c in r) + " |" for r in rows]
    tables.append("\n".join(out) + "\n")


# ---------------------------------------------------------------- micro

micro = load("micro.jsonl")


def mkey(d):
    return (d["host"], d["backend_label"])


def fa_on(d):
    v = d.get("flash_attn")
    return v in (1, True, "1", "on", "enabled")


def suite(name):
    return [d for d in micro if d["suite"] == name]


if micro:
    # 1. flash attention on/off
    rows = suite("fa")
    keys = [k for k in HOSTS if any(mkey(d) == k for d in rows)]
    fig, axes = plt.subplots(1, 2, figsize=(10, 3.6))
    for ax, (metric, title) in zip(axes, [("pp", "Prefill, 512-token prompt"), ("tg", "Decode, 128 tokens")]):
        w = 0.38
        for j, fa in enumerate([False, True]):
            vals = []
            for k in keys:
                m = [d for d in rows if mkey(d) == k and fa_on(d) == fa and
                     ((metric == "pp" and d["n_prompt"] > 0) or (metric == "tg" and d["n_gen"] > 0))]
                vals.append(m[0]["avg_ts"] if m else 0)
            xs = [i + (j - 0.5) * w for i in range(len(keys))]
            bars = ax.bar(xs, vals, w, label=f"flash-attn {'on' if fa else 'off'}",
                          color=["#bbbbbb", "#d1495b"][j])
            ax.bar_label(bars, fmt="%.0f", fontsize=8, padding=2)
        ax.set_xticks(range(len(keys)), [HOSTS[k] for k in keys], rotation=12)
        ax.set_ylabel("tokens/s")
        ax.set_title(title)
    axes[0].legend()
    save(fig, "micro-flash-attn.svg")
    trow = []
    for k in keys:
        def g(metric, fa):
            m = [d for d in rows if mkey(d) == k and fa_on(d) == fa and
                 ((metric == "pp" and d["n_prompt"] > 0) or (metric == "tg" and d["n_gen"] > 0))]
            return f"{m[0]['avg_ts']:.0f}" if m else "–"
        trow.append([HOSTS[k], g("pp", False), g("pp", True), g("tg", False), g("tg", True)])
    table("Prefill / decode, flash attention off vs on (tok/s)",
          ["card · backend", "pp512 FA off", "pp512 FA on", "tg128 FA off", "tg128 FA on"], trow)

    # 2. fresh-prompt prefill vs length
    rows = suite("prefill")
    fig, axes = plt.subplots(1, 2, figsize=(10, 3.6))
    trow = defaultdict(dict)
    for k in HOSTS:
        pts = sorted((d["n_prompt"], d["avg_ts"]) for d in rows if mkey(d) == k)
        if not pts:
            continue
        xs, ys = zip(*pts)
        axes[0].plot(xs, ys, "o-", color=COLORS[k], label=HOSTS[k])
        axes[1].plot(xs, [x / y for x, y in pts], "o-", color=COLORS[k], label=HOSTS[k])
        for x, y in pts:
            trow[HOSTS[k]][x] = f"{y:.0f} ({x / y:.1f}s)"
    for ax in axes:
        ax.set_xscale("log", base=2)
        ax.set_xlabel("prompt tokens (uncached)")
    axes[0].set_ylabel("prefill tokens/s")
    axes[0].set_title("Prefill throughput")
    axes[1].set_ylabel("seconds")
    axes[1].set_title("Time to process a fresh prompt")
    axes[0].legend()
    save(fig, "micro-prefill.svg")
    lens = sorted({d["n_prompt"] for d in rows})
    table("Fresh-prompt prefill: tok/s (seconds to first token)", ["card · backend"] + [str(n) for n in lens],
          [[h] + [v.get(n, "–") for n in lens] for h, v in trow.items()])

    # 3. decode vs depth
    rows = suite("depth")
    fig, ax = plt.subplots(figsize=(6.4, 3.6))
    trow = defaultdict(dict)
    for k in HOSTS:
        pts = sorted((d["n_depth"], d["avg_ts"]) for d in rows if mkey(d) == k)
        if not pts:
            continue
        xs, ys = zip(*pts)
        ax.plot(xs, ys, "o-", color=COLORS[k], label=HOSTS[k])
        for x, y in pts:
            trow[HOSTS[k]][x] = f"{y:.1f}"
    ax.set_xlabel("tokens already in context")
    ax.set_ylabel("decode tokens/s (single stream)")
    ax.set_title("Decode speed vs context depth")
    ax.set_ylim(bottom=0)
    ax.legend()
    save(fig, "micro-depth.svg")
    depths = sorted({d["n_depth"] for d in rows})
    table("Single-stream decode at depth (tok/s)", ["card · backend"] + [f"@{n}" for n in depths],
          [[h] + [v.get(n, "–") for n in depths] for h, v in trow.items()])

    # 4. KV cache type at 16k depth
    kvs = ["f16", "q8_0", "q4_0"]
    fig, axes = plt.subplots(1, 2, figsize=(10, 3.6))
    trow = []
    keys = [k for k in HOSTS if any(mkey(d) == k and d["suite"].startswith("kv-") for d in micro)]
    for ax, metric, title in [(axes[0], "pp", "Prefill 2048 @ 16k depth"), (axes[1], "tg", "Decode 128 @ 16k depth")]:
        w = 0.8 / len(kvs)
        for j, kv in enumerate(kvs):
            vals = []
            for k in keys:
                m = [d for d in suite(f"kv-{kv}") if mkey(d) == k and
                     ((metric == "pp" and d["n_prompt"] > 0) or (metric == "tg" and d["n_gen"] > 0))]
                vals.append(m[0]["avg_ts"] if m else 0)
            xs = [i + (j - 1) * w for i in range(len(keys))]
            bars = ax.bar(xs, vals, w, label=f"KV {kv}", color=["#bbbbbb", "#00798c", "#edae49"][j])
            ax.bar_label(bars, fmt="%.0f", fontsize=7, padding=2)
        ax.set_xticks(range(len(keys)), [HOSTS[k] for k in keys], rotation=12)
        ax.set_ylabel("tokens/s")
        ax.set_title(title)
    axes[0].legend()
    save(fig, "micro-kv.svg")
    for k in keys:
        r = [HOSTS[k]]
        for kv in kvs:
            pp = [d for d in suite(f"kv-{kv}") if mkey(d) == k and d["n_prompt"] > 0]
            tg = [d for d in suite(f"kv-{kv}") if mkey(d) == k and d["n_gen"] > 0]
            r.append(f"{pp[0]['avg_ts']:.0f} / {tg[0]['avg_ts']:.1f}" if pp and tg else "–")
        trow.append(r)
    table("KV cache type at 16k depth: prefill / decode (tok/s)", ["card · backend"] + kvs, trow)

    # 5. ubatch
    rows = suite("ubatch")
    fig, ax = plt.subplots(figsize=(6.4, 3.6))
    for k in HOSTS:
        pts = sorted((d["n_ubatch"], d["avg_ts"]) for d in rows if mkey(d) == k)
        if pts:
            xs, ys = zip(*pts)
            ax.plot(xs, ys, "o-", color=COLORS[k], label=HOSTS[k])
    ax.set_xscale("log", base=2)
    ax.set_xlabel("physical batch size (-ub)")
    ax.set_ylabel("prefill tokens/s (4096 prompt)")
    ax.set_title("Prefill vs micro-batch size")
    ax.legend()
    save(fig, "micro-ubatch.svg")

# ---------------------------------------------------------------- capacity

cap = load("capacity.jsonl")
if cap:
    fig, axes = plt.subplots(1, 3, figsize=(13, 3.6))
    groups = defaultdict(list)
    for d in cap:
        t = d["tags"]
        groups[(t["host"], int(t["slots"]), int(t["ctx"]))].append(d)
    styles = {"desktop": ("#d1495b", "RX 6900 XT"), "server": ("#00798c", "RX 6700 XT")}
    dashes = {}
    trow = []
    for (host, slots, ctx), ds in sorted(groups.items()):
        ds.sort(key=lambda d: d["params"]["agents"])
        layout = f"{slots}×{ctx // slots // 1024}k"
        ls = dashes.setdefault(layout, ["-", "--", ":"][len(dashes) % 3])
        color, name = styles[host]
        a = [d["params"]["agents"] for d in ds]
        axes[0].plot(a, [d["aggregate_output_tps"] for d in ds], "o" + ls, color=color, label=f"{name} {layout}")
        axes[1].plot(a, [d["mean_stream_decode_tps"] for d in ds], "o" + ls, color=color)
        axes[2].plot(a, [d["followup_ttft_p50_ms"] / 1000 for d in ds], "o" + ls, color=color)
        for d in ds:
            trow.append([name, layout, d["params"]["agents"], f"{d['aggregate_output_tps']:.1f}",
                         f"{d['mean_stream_decode_tps']:.1f}", f"{d['followup_ttft_p50_ms'] / 1000:.2f}",
                         f"{d['cache_reuse_pct']:.0f}%", d["failed"]])
    for ax, t, y in [(axes[0], "Aggregate output", "tokens/s"), (axes[1], "Per-stream decode", "tokens/s"),
                     (axes[2], "Follow-up TTFT p50", "seconds")]:
        ax.set_xscale("log", base=2)
        ax.set_xticks([1, 2, 4, 8], ["1", "2", "4", "8"])
        ax.set_xlabel("concurrent agents")
        ax.set_ylabel(y)
        ax.set_title(t)
    fig.legend(*axes[0].get_legend_handles_labels(), loc="lower center", ncol=6, fontsize=8)
    save(fig, "capacity.svg", rect=(0, 0.08, 1, 1))
    table("Concurrency sweep (single worker, 8k-byte system prompt, 4 turns)",
          ["card", "slots", "agents", "agg tok/s", "per-stream tok/s", "follow-up TTFT p50 (s)", "cache reuse", "failed"],
          trow)

# ---------------------------------------------------------------- cache / context / host cache

cache = load("cache.jsonl")
if cache:
    by = {d["tags"]["mode"]: d for d in cache}
    order = [m for m in ["cached", "no-cache"] if m in by]
    fig, axes = plt.subplots(1, 3, figsize=(11, 3.4))
    for ax, key, title, scale in [
        (axes[0], "followup_ttft_p50_ms", "Follow-up TTFT p50 (s)", 1000),
        (axes[1], "wall_s", "Wall time for the run (s)", 1),
        (axes[2], "cache_reuse_pct", "Prompt tokens from cache (%)", 1),
    ]:
        vals = [by[m][key] / scale for m in order]
        bars = ax.bar(order, vals, color=["#00798c", "#bbbbbb"][: len(order)])
        ax.bar_label(bars, fmt="%.1f", padding=2)
        ax.set_title(title)
    save(fig, "cache-vs-fresh.svg")
    table("Cached vs fresh prompts (2 agents × 6 turns, ~7k-token system prompt, RX 6900 XT 2×32k)",
          ["mode", "follow-up TTFT p50 (s)", "TTFT p95 (s)", "wall (s)", "agg tok/s", "cache reuse", "mean prompt tok"],
          [[m, f"{by[m]['followup_ttft_p50_ms'] / 1000:.2f}", f"{by[m]['ttft_p95_ms'] / 1000:.2f}",
            f"{by[m]['wall_s']:.1f}", f"{by[m]['aggregate_output_tps']:.1f}", f"{by[m]['cache_reuse_pct']:.0f}%",
            f"{by[m]['mean_prompt_tokens']:.0f}"] for m in order])

ctx = load("context.jsonl")
if ctx:
    fig, ax = plt.subplots(figsize=(6.8, 3.8))
    cached = sorted([d for d in ctx if d["tags"]["mode"] == "cached"], key=lambda d: d["mean_prompt_tokens"])
    fresh = sorted([d for d in ctx if d["tags"]["mode"] == "no-cache"], key=lambda d: d["mean_prompt_tokens"])
    ax.plot([d["mean_prompt_tokens"] for d in cached], [d["first_turn_ttft_p50_ms"] / 1000 for d in cached],
            "o--", color="#edae49", label="first turn (cold)")
    ax.plot([d["mean_prompt_tokens"] for d in cached], [d["followup_ttft_p50_ms"] / 1000 for d in cached],
            "o-", color="#00798c", label="follow-up turns, prefix cached")
    ax.plot([d["mean_prompt_tokens"] for d in fresh], [d["followup_ttft_p50_ms"] / 1000 for d in fresh],
            "o-", color="#bbbbbb", label="follow-up turns, cache off")
    ax.set_xlabel("mean prompt tokens per request")
    ax.set_ylabel("TTFT p50 (s)")
    ax.set_title("Time to first token as context grows (RX 6900 XT)")
    ax.legend()
    save(fig, "context-ttft.svg")
    rows = []
    for c in cached:
        sc = c["tags"]["system_chars"]
        f = next((d for d in fresh if d["tags"]["system_chars"] == sc), None)
        rows.append([f"{c['mean_prompt_tokens']:.0f}", f"{c['first_turn_ttft_p50_ms'] / 1000:.2f}",
                     f"{c['followup_ttft_p50_ms'] / 1000:.2f}",
                     f"{f['followup_ttft_p50_ms'] / 1000:.2f}" if f else "–", f"{c['cache_reuse_pct']:.0f}%"])
    table("TTFT vs context (1 agent × 4 turns)", ["mean prompt tok", "first turn (s)", "follow-ups cached (s)",
                                                  "follow-ups cache off (s)", "cache reuse"], rows)

hc = load("hostcache.jsonl")
if hc:
    labels = [d["label"] for d in hc]
    fig, axes = plt.subplots(1, 3, figsize=(12, 3.4))
    for ax, key, title, scale in [
        (axes[0], "cache_reuse_pct", "Prompt tokens from cache (%)", 1),
        (axes[1], "followup_ttft_p50_ms", "Follow-up TTFT p50 (s)", 1000),
        (axes[2], "aggregate_output_tps", "Aggregate output (tok/s)", 1),
    ]:
        bars = ax.bar(labels, [d[key] / scale for d in hc], color="#00798c")
        ax.bar_label(bars, fmt="%.1f", padding=2, fontsize=8)
        ax.set_title(title)
        ax.tick_params(axis="x", rotation=20)
    save(fig, "host-cache.svg")
    table("6 agents on 2 slots (slot eviction), unique ~5k-token system prompts",
          ["config", "cache reuse", "follow-up TTFT p50 (s)", "p95 (s)", "agg tok/s", "failed"],
          [[d["label"], f"{d['cache_reuse_pct']:.0f}%", f"{d['followup_ttft_p50_ms'] / 1000:.2f}",
            f"{d['followup_ttft_p95_ms'] / 1000:.2f}", f"{d['aggregate_output_tps']:.1f}", d["failed"]] for d in hc])

# ---------------------------------------------------------------- routing

route = load("route.jsonl")
if route:
    scen = ["short", "long"]
    pols = [p for p in POLICY_COLORS if any(d["tags"]["policy"] == p for d in route)]
    metrics = [("aggregate_output_tps", "Aggregate output (tok/s)", 1),
               ("followup_ttft_p50_ms", "Follow-up TTFT p50 (s)", 1000),
               ("ttft_p95_ms", "TTFT p95 (s)", 1000),
               ("cache_reuse_pct", "Prompt tokens from cache (%)", 1)]
    fig, axes = plt.subplots(len(scen), len(metrics), figsize=(14, 6.4))
    trow = []
    for i, s in enumerate(scen):
        for j, (key, title, scale) in enumerate(metrics):
            ax = axes[i][j]
            means, lo, hi = [], [], []
            for p in pols:
                vals = [d[key] / scale for d in route if d["tags"]["scenario"] == s and d["tags"]["policy"] == p]
                m = statistics.mean(vals) if vals else 0
                means.append(m)
                lo.append(m - min(vals) if vals else 0)
                hi.append(max(vals) - m if vals else 0)
            bars = ax.bar(pols, means, yerr=[lo, hi], capsize=3, color=[POLICY_COLORS[p] for p in pols])
            ax.bar_label(bars, fmt="%.1f", padding=2, fontsize=8)
            ax.set_title(f"{s}: {title}", fontsize=9)
            ax.tick_params(axis="x", rotation=25, labelsize=8)
    save(fig, "routing.svg")
    for s in scen:
        for p in pols:
            ds = [d for d in route if d["tags"]["scenario"] == s and d["tags"]["policy"] == p]
            if not ds:
                continue
            avg = lambda k, sc=1: statistics.mean(d[k] / sc for d in ds)  # noqa: E731
            trow.append([s, p, len(ds), f"{avg('aggregate_output_tps'):.1f}", f"{avg('wall_s'):.0f}",
                         f"{avg('followup_ttft_p50_ms', 1000):.2f}", f"{avg('ttft_p95_ms', 1000):.2f}",
                         f"{avg('cache_reuse_pct'):.0f}%", sum(d["failed"] for d in ds)])
    table("Routing policies, RX 6900 XT + RX 6700 XT (2×32k each), mean of runs",
          ["scenario", "policy", "runs", "agg tok/s", "wall (s)", "follow-up TTFT p50 (s)", "TTFT p95 (s)",
           "cache reuse", "failed"], trow)

mac = load("mac.jsonl")
if mac:
    by = {d["tags"]["pool"]: d for d in mac}
    rows = [[d["label"], f"{d['aggregate_output_tps']:.1f}", f"{d['mean_stream_decode_tps']:.1f}",
             f"{d['followup_ttft_p50_ms'] / 1000:.2f}", f"{d['ttft_p95_ms'] / 1000:.2f}", f"{d['wall_s']:.0f}",
             ", ".join(f"{k}:{v}" for k, v in sorted(d.get("workers", {}).items())) or "–"] for d in mac]
    table("MacBook Air M4 as a worker", ["pool", "agg tok/s", "per-stream tok/s", "follow-up TTFT p50 (s)",
                                         "TTFT p95 (s)", "wall (s)", "requests per worker"], rows)
    pools = [p for p in ["2gpu", "2gpu+mac"] if p in by]
    if pools:
        fig, axes = plt.subplots(1, 3, figsize=(10, 3.2))
        for ax, key, title, scale in [(axes[0], "aggregate_output_tps", "Aggregate output (tok/s)", 1),
                                      (axes[1], "ttft_p95_ms", "TTFT p95 (s)", 1000),
                                      (axes[2], "wall_s", "Wall time (s)", 1)]:
            bars = ax.bar(pools, [by[p][key] / scale for p in pools], color=["#d1495b", "#7a7a7a"][: len(pools)])
            ax.bar_label(bars, fmt="%.1f", padding=2)
            ax.set_title(title)
        save(fig, "mac-third-worker.svg")


# ---------------------------------------------------------------- memory

def load_raw(name):
    p = RES / name
    return [json.loads(l) for l in p.read_text().splitlines() if l.strip()] if p.exists() else []


memrows = load_raw("memory.jsonl")
if memrows:
    snap = {}
    for r in memrows:
        snap[(r["host"], r["label"], r["phase"])] = r  # latest wins
    labels = list(dict.fromkeys(r["label"] for r in memrows))
    load_perf = {d["label"]: d for d in load("memload.jsonl")}
    fig, axes = plt.subplots(1, 2, figsize=(12, 3.8))
    for ax, host, title, cap in [(axes[0], "desktop", "RX 6900 XT (16 GB)", 16368), (axes[1], "server", "RX 6700 XT (12 GB)", 12272)]:
        idle = [snap.get((host, l, "idle"), {}).get("vram_mib", 0) / 1024 for l in labels]
        bars = ax.bar(labels, idle, color="#00798c")
        ax.bar_label(bars, fmt="%.1f", fontsize=8, padding=2)
        ax.axhline(cap / 1024, color="#d1495b", ls="--", lw=1)
        ax.set_ylabel("VRAM in use, GiB")
        ax.set_title(title)
        ax.tick_params(axis="x", rotation=30, labelsize=8)
    save(fig, "memory-vram.svg")
    rows = []
    for host in ["desktop", "server"]:
        for l in labels:
            i, ld = snap.get((host, l, "idle")), snap.get((host, l, "loaded"))
            if not i:
                continue
            perf = load_perf.get(f"{host} {l}")
            rows.append([{"desktop": "6900 XT", "server": "6700 XT"}[host], l, f"{i['vram_mib'] / 1024:.1f}",
                         f"{(ld or i)['rss_mib'] / 1024:.1f}",
                         f"{perf['aggregate_output_tps']:.1f}" if perf else "–",
                         f"{perf['cache_reuse_pct']:.0f}%" if perf else "–"])
    table("Memory per configuration (gemma4 12B)", ["card", "config", "VRAM GiB", "host RSS GiB after load",
                                                    "agg tok/s (4 agents)", "cache reuse"], rows)

# ---------------------------------------------------------------- compaction

comp = load("compaction.jsonl")
if comp:
    order = [l for l in ["no compaction", "append @8k", "append @12k", "rewrite @8k"] if any(d["label"] == l for d in comp)]
    by = {d["label"]: d for d in comp}
    fig, axes = plt.subplots(1, 4, figsize=(14, 3.4))
    for ax, key, title, scale in [(axes[0], "wall_s", "Wall time (s)", 1),
                                  (axes[1], "followup_ttft_p50_ms", "Follow-up TTFT p50 (s)", 1000),
                                  (axes[2], "max_prompt_tokens", "Largest prompt (tokens)", 1),
                                  (axes[3], "recall_pct", "Fact recall at the end (%)", 1)]:
        bars = ax.bar(order, [by[o].get(key, 0) / scale for o in order],
                      color=["#9e9e9e", "#00798c", "#2a9d8f", "#edae49"][: len(order)])
        ax.bar_label(bars, fmt="%.0f" if scale == 1 else "%.2f", padding=2, fontsize=8)
        ax.set_title(title)
        ax.tick_params(axis="x", rotation=20, labelsize=8)
    save(fig, "compaction.svg")
    table("Compaction (2 agents × 12 turns, gemma4 12B on RX 6900 XT)",
          ["strategy", "wall (s)", "compactions", "summary call (s)", "next-turn TTFT (s)", "next-turn cached",
           "follow-up TTFT p50 (s)", "largest prompt", "recall (compacted / recent)"],
          [[o, f"{by[o]['wall_s']:.0f}", by[o].get("compactions", 0), f"{by[o].get('compact_mean_ms', 0) / 1000:.1f}",
            f"{by[o].get('post_compact_ttft_p50_ms', 0) / 1000:.2f}", f"{by[o].get('post_compact_cache_pct', 0):.0f}%",
            f"{by[o]['followup_ttft_p50_ms'] / 1000:.2f}", by[o]["max_prompt_tokens"],
            f"{by[o].get('recall_pct', 0):.0f}% ({by[o].get('recall_compacted_pct', 0):.0f} / {by[o].get('recall_recent_pct', 0):.0f})"]
           for o in order])

# ---------------------------------------------------------------- MoE models

MODEL_NAMES = ["gemma4-12b", "lfm2.5-8b-a1b", "gpt-oss-20b", "qwen3.6-35b-a3b"]
MODEL_LABEL = {"gemma4-12b": "gemma4 12B (dense)", "lfm2.5-8b-a1b": "LFM2.5 8B-A1B", "gpt-oss-20b": "gpt-oss 20B-A3.6B",
               "qwen3.6-35b-a3b": "Qwen3.6 35B-A3B"}
moe = [d for d in micro if d["suite"].startswith("moe-")]
if moe:
    def ncm(d):
        return int(d.get("n_cpu_moe", 0) or 0)

    # best (fastest decode) config per model/host
    best = {}
    for d in moe:
        k = (d["suite"][4:], d["host"])
        if d["n_gen"] > 0 and (k not in best or d["avg_ts"] > best[k][1]):
            best[k] = (ncm(d), d["avg_ts"])
    fig, axes = plt.subplots(1, 2, figsize=(12, 3.8))
    hosts = [("desktop", "RX 6900 XT", "#d1495b"), ("server", "RX 6700 XT", "#00798c"), ("mac", "M4 Air", "#7a7a7a")]
    names = [n for n in MODEL_NAMES if any(k[0] == n for k in best)]
    w = 0.27
    rows = []
    for j, (h, hl, c) in enumerate(hosts):
        tg, pp = [], []
        for n in names:
            b = best.get((n, h))
            tg.append(b[1] if b else 0)
            pm = [d for d in moe if d["suite"] == f"moe-{n}" and d["host"] == h and d["n_prompt"] == 4096 and b and ncm(d) == b[0]]
            pp.append(pm[0]["avg_ts"] if pm else 0)
            if b:
                rows.append([MODEL_LABEL[n], hl, b[0], f"{pm[0]['avg_ts']:.0f}" if pm else "–", f"{b[1]:.1f}"])
        xs = [i + (j - 1) * w for i in range(len(names))]
        axes[0].bar_label(axes[0].bar(xs, tg, w, color=c, label=hl), fmt="%.0f", fontsize=7, padding=2)
        axes[1].bar_label(axes[1].bar(xs, pp, w, color=c, label=hl), fmt="%.0f", fontsize=7, padding=2)
    for ax, t in [(axes[0], "Decode tok/s (single stream)"), (axes[1], "Prefill tok/s (4096-token prompt)")]:
        ax.set_xticks(range(len(names)), [MODEL_LABEL[n] for n in names], rotation=12, fontsize=8)
        ax.set_title(t)
    axes[0].legend(fontsize=8)
    save(fig, "moe-speed.svg")
    table("Dense vs MoE, best placement per card (llama-bench, FA on)",
          ["model", "card", "expert layers on CPU", "pp4096 tok/s", "tg128 tok/s"], rows)

    sweeps = [(n, h) for n in ["gpt-oss-20b", "qwen3.6-35b-a3b"] for h in ["desktop", "server"]
              if len({ncm(d) for d in moe if d["suite"] == f"moe-{n}" and d["host"] == h}) > 1]
    if sweeps:
        fig, axes = plt.subplots(1, 2, figsize=(12, 3.6))
        for n, h in sweeps:
            ds = [d for d in moe if d["suite"] == f"moe-{n}" and d["host"] == h]
            c = {"desktop": "#d1495b", "server": "#00798c"}[h]
            ls = "-" if n.startswith("qwen") else "--"
            tg = sorted((ncm(d), d["avg_ts"]) for d in ds if d["n_gen"] > 0)
            pp = sorted((ncm(d), d["avg_ts"]) for d in ds if d["n_prompt"] == 4096)
            lab = f"{MODEL_LABEL[n]} · {dict(desktop='6900 XT', server='6700 XT')[h]}"
            if tg:
                axes[0].plot(*zip(*tg), "o" + ls, color=c, label=lab)
            if pp:
                axes[1].plot(*zip(*pp), "o" + ls, color=c, label=lab)
        axes[0].set_title("Decode vs expert layers kept in system RAM")
        axes[1].set_title("Prefill (4096) vs expert layers in system RAM")
        for ax in axes:
            ax.set_xlabel("--n-cpu-moe (layers whose experts live on the CPU)")
            ax.set_ylabel("tokens/s")
        axes[0].legend(fontsize=8)
        save(fig, "moe-offload.svg")

models_serve = load("models.jsonl")
if models_serve:
    rows = []
    for n in MODEL_NAMES:
        for a in (1, 4):
            d = next((x for x in models_serve if x["label"] == f"{n} agents={a}"), None)
            if d:
                rows.append([MODEL_LABEL[n], a, f"{d['aggregate_output_tps']:.1f}", f"{d['mean_stream_decode_tps']:.1f}",
                             f"{d['followup_ttft_p50_ms'] / 1000:.2f}", f"{d['cache_reuse_pct']:.0f}%"])
    table("Serving per model (RX 6900 XT, 2×32k)", ["model", "agents", "agg tok/s", "per-stream tok/s",
                                                    "follow-up TTFT p50 (s)", "cache reuse"], rows)

needle = load("needle.jsonl")
if needle:
    fig, axes = plt.subplots(1, len(needle), figsize=(3.4 * len(needle), 3.2), squeeze=False)
    rows = []
    for ax, d in zip(axes[0], sorted(needle, key=lambda d: MODEL_NAMES.index(d["tags"]["model"]) if d["tags"]["model"] in MODEL_NAMES else 9)):
        sizes = sorted({n["target_tokens"] for n in d["needle"]})
        depths = sorted({n["depth_pct"] for n in d["needle"]})
        grid = [[next((1 if n["correct"] else 0 for n in d["needle"] if n["target_tokens"] == s and n["depth_pct"] == dp), float("nan"))
                 for dp in depths] for s in sizes]
        ax.imshow(grid, cmap="RdYlGn", vmin=0, vmax=1, aspect="auto")
        ax.set_xticks(range(len(depths)), [f"{x}%" for x in depths], fontsize=8)
        ax.set_yticks(range(len(sizes)), [f"{s // 1000}k" for s in sizes], fontsize=8)
        ax.set_xlabel("needle depth")
        ax.set_title(MODEL_LABEL.get(d["tags"]["model"], d["tags"]["model"]), fontsize=9)
        ax.grid(False)
        rows.append([MODEL_LABEL.get(d["tags"]["model"], d["tags"]["model"]), f"{d.get('recall_pct', 0):.0f}%"] +
                    [f"{sum(n['correct'] for n in d['needle'] if n['target_tokens'] == s)}/{sum(1 for n in d['needle'] if n['target_tokens'] == s)}" for s in sizes])
    axes[0][0].set_ylabel("context tokens")
    save(fig, "needle.svg")
    sizes = sorted({n["target_tokens"] for d in needle for n in d["needle"]})
    table("Needle in a haystack (found / probes per context size)", ["model", "overall"] + [f"{s // 1000}k" for s in sizes], rows)

recall = load("recall.jsonl")
if recall:
    rows = []
    names = [n for n in MODEL_NAMES if any(d["tags"]["model"] == n for d in recall)]
    fig, ax = plt.subplots(figsize=(7, 3.4))
    w = 0.38
    for j, style in enumerate(["none", "append"]):
        vals = []
        for n in names:
            d = next((x for x in recall if x["tags"]["model"] == n and x["tags"]["style"] == style), None)
            vals.append(d.get("recall_pct", 0) if d else 0)
            if d:
                rows.append([MODEL_LABEL[n], "full history" if style == "none" else "compacted", d.get("recall_asked", 0),
                             f"{d.get('recall_pct', 0):.0f}%", f"{d.get('recall_compacted_pct', 0):.0f}%" if style != "none" else "–",
                             d.get("compactions", 0)])
        xs = [i + (j - 0.5) * w for i in range(len(names))]
        ax.bar_label(ax.bar(xs, vals, w, color=["#9e9e9e", "#00798c"][j], label=["full history", "after compaction"][j]),
                     fmt="%.0f", fontsize=8, padding=2)
    ax.set_xticks(range(len(names)), [MODEL_LABEL[n] for n in names], rotation=12, fontsize=8)
    ax.set_ylabel("facts recalled (%)")
    ax.set_ylim(0, 110)
    ax.set_title("Agent memory: planted facts recalled at session end")
    ax.legend(fontsize=8)
    save(fig, "recall.svg")
    table("Fact recall at session end (2 agents × 10 turns)", ["model", "history", "probes", "recalled",
                                                              "of compacted-away facts", "compactions"], rows)

# ---------------------------------------------------------------- retrieval and agent tools

def latest_by(rows, *keys):
    out = {}
    for r in rows:
        out[tuple(r.get(k) for k in keys)] = r
    return list(out.values())


rag = load_raw("rag.jsonl")
EMB = ["qwen3-embed-0.6b", "embeddinggemma-300m", "nomic-embed-1.5"]
EMB_LABEL = {"qwen3-embed-0.6b": "Qwen3-Embedding 0.6B", "embeddinggemma-300m": "EmbeddingGemma 300M",
             "nomic-embed-1.5": "nomic-embed v1.5"}
RR = ["none", "bge-reranker-v2-m3", "qwen3-reranker-0.6b"]
RR_LABEL = {"none": "vector search only", "bge-reranker-v2-m3": "+ bge-reranker-v2-m3", "qwen3-reranker-0.6b": "+ Qwen3-Reranker 0.6B"}

ret_all = latest_by([r for r in rag if r["kind"] == "retrieval"], "embedder", "reranker", "candidates")
ret = [r for r in ret_all if r.get("candidates", 0) in (0, 30)]
if ret:
    embs = [e for e in EMB if any(r["embedder"] == e for r in ret)]
    rrs = [x for x in RR if any(r["reranker"] == x for r in ret)]
    fig, axes = plt.subplots(1, 2, figsize=(12, 3.8), sharey=True)
    w = 0.8 / len(rrs)
    rows = []
    for ax, key, title in [(axes[0], "recall_at_1", "Right passage ranked first (recall@1)"),
                           (axes[1], "recall_at_5", "Right passage in the top 5 (recall@5)")]:
        for j, x in enumerate(rrs):
            vals = [next((r[key] for r in ret if r["embedder"] == e and r["reranker"] == x), 0) for e in embs]
            xs = [i + (j - (len(rrs) - 1) / 2) * w for i in range(len(embs))]
            ax.bar_label(ax.bar(xs, vals, w, color=["#9e9e9e", "#00798c", "#d1495b"][j], label=RR_LABEL[x]),
                         fmt="%.0f", fontsize=8, padding=2)
        ax.set_xticks(range(len(embs)), [EMB_LABEL[e] for e in embs], fontsize=8)
        ax.set_ylim(0, 105)
        ax.set_title(title)
    axes[0].set_ylabel("% of questions")
    axes[0].legend(fontsize=8, loc="lower left")
    save(fig, "rag-retrieval.svg")
    for e in embs:
        for x in rrs:
            r = next((r for r in ret if r["embedder"] == e and r["reranker"] == x), None)
            if r:
                rows.append([EMB_LABEL[e], RR_LABEL[x], r["questions"], f"{r['recall_at_1']:.0f}%", f"{r['recall_at_5']:.0f}%",
                             f"{r['recall_at_10']:.0f}%", f"{r['mrr_at_10']:.2f}", f"{r['latency_p50_ms']:.0f}"])
    table("Retrieval quality: 150 questions over llama.cpp's docs (1,732 passages); rerankers rescore the top 30",
          ["embedder", "reranker", "questions", "recall@1", "recall@5", "recall@10", "MRR@10", "search p50 ms (6700 XT)"], rows)
    rows = []
    for e in embs:
        for x in rrs[1:]:
            for d in (10, 20, 30):
                r = next((r for r in ret_all if r["embedder"] == e and r["reranker"] == x and r.get("candidates") == d), None)
                if r:
                    rows.append([EMB_LABEL[e], RR_LABEL[x].lstrip("+ "), d, f"{r['recall_at_1']:.0f}%", f"{r['recall_at_5']:.0f}%"])
    table("Rerank depth: how many vector hits the reranker rescores", ["embedder", "reranker", "candidates", "recall@1", "recall@5"], rows)

se = latest_by([r for r in rag if r["kind"] == "speed-embed"], "embedder", "label")
sr = latest_by([r for r in rag if r["kind"] == "speed-rerank"], "reranker", "label")
if se:
    fig, axes = plt.subplots(1, 2, figsize=(12, 3.6))
    embs = [e for e in EMB if any(r["embedder"] == e for r in se)]
    for j, lab in enumerate(["cpu", "gpu"]):
        vals = [next((r["chunks_per_s"] for r in se if r["embedder"] == e and r["label"] == lab), 0) for e in embs]
        xs = [i + (j - 0.5) * 0.38 for i in range(len(embs))]
        axes[0].bar_label(axes[0].bar(xs, vals, 0.38, color=["#9e9e9e", "#00798c"][j],
                                      label={"cpu": "Ryzen 5 7600X (4 threads)", "gpu": "RX 6700 XT, beside gemma4"}[lab]),
                          fmt="%.1f", fontsize=8, padding=2)
    axes[0].set_xticks(range(len(embs)), [EMB_LABEL[e] for e in embs], fontsize=8)
    axes[0].set_ylabel("passages / s")
    axes[0].set_title("Indexing: embedding throughput")
    axes[0].legend(fontsize=8)
    rrs = [x for x in RR[1:] if any(r["reranker"] == x for r in sr)]
    for j, lab in enumerate(["cpu", "gpu"]):
        vals = [next((r["p50_ms"] / 1000 for r in sr if r["reranker"] == x and r["label"] == lab), 0) for x in rrs]
        xs = [i + (j - 0.5) * 0.38 for i in range(len(rrs))]
        axes[1].bar_label(axes[1].bar(xs, vals, 0.38, color=["#9e9e9e", "#00798c"][j]), fmt="%.1f", fontsize=8, padding=2)
    axes[1].set_xticks(range(len(rrs)), [RR_LABEL[x].lstrip("+ ") for x in rrs], fontsize=8)
    axes[1].set_ylabel("seconds (p50)")
    axes[1].set_title("Search: reranking 30 candidates")
    save(fig, "rag-speed.svg")
    rows = []
    for r in se:
        rows.append([EMB_LABEL[r["embedder"]], r["label"].upper(), f"{r['chunks_per_s']:.1f} passages/s", f"{r['query_p50_ms']:.0f} ms"])
    for r in sr:
        rows.append([RR_LABEL[r["reranker"]].lstrip("+ "), r["label"].upper(), f"30 candidates: {r['p50_ms'] / 1000:.2f} s p50",
                     f"{r['p95_ms'] / 1000:.2f} s p95"])
    table("Embedding and rerank speed", ["model", "device", "throughput", "query latency"], rows)

e2e_rows = latest_by([r for r in rag if r["kind"] == "e2e"], "mode", "model")
if e2e_rows:
    modes = [m for m in ["closed-book", "oracle-file", "long-context", "retrieval"] if any(r["mode"] == m for r in e2e_rows)]
    ML = {"closed-book": "no docs", "oracle-file": "the right file\n(if you knew it)", "long-context": "~26k tokens of docs\npasted in",
          "retrieval": "top 5 passages\n(search_docs)"}
    by = {r["mode"]: r for r in e2e_rows}
    fig, axes = plt.subplots(1, 3, figsize=(13, 3.8))
    cols = ["#9e9e9e", "#edae49", "#d1495b", "#00798c"][: len(modes)]
    for ax, key, title, fmt in [(axes[0], "accuracy", "Correct answers (%; overflowing the slot = wrong)", "%.1f"),
                                (axes[1], "prompt_tokens_mean", "Prompt tokens (mean)", "%.0f"),
                                (axes[2], "total_s_mean", "Seconds per answer (mean, fresh prompt)", "%.1f")]:
        ax.bar_label(ax.bar([ML[m] for m in modes], [by[m][key] for m in modes], color=cols), fmt=fmt, fontsize=8, padding=2)
        ax.set_title(title, fontsize=10)
        ax.tick_params(axis="x", labelsize=7)
    axes[0].set_ylim(0, 105)
    save(fig, "rag-e2e.svg")
    table(f"Answering llama.cpp questions with gemma4 12B ({by[modes[0]]['questions']} questions, cache off)",
          ["context", "correct (overflow = wrong)", "overflowed 32k slot", "correct when it fit", "prompt tokens", "prefill s",
           "total s (mean)", "total s (p95)"],
          [[ML[m].replace("\n", " "), f"{by[m]['accuracy']:.1f}%", by[m].get("failed", 0), f"{by[m].get('accuracy_answered', by[m]['accuracy']):.0f}%",
            f"{by[m]['prompt_tokens_mean']:.0f}", f"{by[m]['prefill_s_mean']:.1f}", f"{by[m]['total_s_mean']:.1f}",
            f"{by[m]['total_s_p95']:.1f}"] for m in modes])

webr = latest_by([r for r in rag if r["kind"] == "web"], "mode", "model")
if webr:
    models_w = list(dict.fromkeys(r["model"] for r in webr))
    fig, axes = plt.subplots(1, 2, figsize=(11, 3.6))
    for j, mode in enumerate(["closed-book", "web-tools"]):
        xs = [i + (j - 0.5) * 0.38 for i in range(len(models_w))]
        acc = [next((r["accuracy"] for r in webr if r["model"] == m and r["mode"] == mode), 0) for m in models_w]
        sec = [next((r["seconds_mean"] for r in webr if r["model"] == m and r["mode"] == mode), 0) for m in models_w]
        lab = {"closed-book": "model alone", "web-tools": "with web_search + fetch_url"}[mode]
        axes[0].bar_label(axes[0].bar(xs, acc, 0.38, color=["#9e9e9e", "#00798c"][j], label=lab), fmt="%.0f", fontsize=8, padding=2)
        axes[1].bar_label(axes[1].bar(xs, sec, 0.38, color=["#9e9e9e", "#00798c"][j]), fmt="%.1f", fontsize=8, padding=2)
    for ax, t in [(axes[0], "Current version questions answered correctly (%)"), (axes[1], "Seconds per question (mean)")]:
        ax.set_xticks(range(len(models_w)), models_w)
        ax.set_title(t, fontsize=10)
    axes[0].set_ylim(0, 105)
    axes[0].legend(fontsize=8)
    save(fig, "web-tools.svg")
    table("Web search: 'what is the latest version of X' (truth fetched live)",
          ["model", "mode", "questions", "correct", "tool calls (mean)", "s (mean)", "s (p95)"],
          [[r["model"], r["mode"], r["questions"], f"{r['accuracy']:.0f}%", f"{r['tool_calls_mean']:.1f}", f"{r['seconds_mean']:.1f}",
            f"{r['seconds_p95']:.1f}"] for r in webr])

cont = load_raw("rag-contention.jsonl")
if cont:
    rows = []
    for lab in ["idle", "indexing", "gpu-rerank"]:
        gpu = [r for r in cont if r.get("label") == f"contention-gpu-{lab}"]
        cpu = [r["seconds"] for r in cont if r.get("kind") == "contention-cpu" and r.get("load") == lab]
        if gpu or cpu:
            rows.append([lab, f"{gpu[-1]['aggregate_output_tps']:.1f}" if gpu else "–",
                         f"{statistics.median(cpu):.2f}" if cpu else "–"])
    table("Contention: retrieval work vs. the GPU worker and the CPU model",
          ["background load", "gemma4 on 6700 XT, 4 agents (agg tok/s)", "lfm2 title job (median s)"], rows)

(RES / "tables.md").write_text("\n".join(tables))
print("wrote", RES / "tables.md")
