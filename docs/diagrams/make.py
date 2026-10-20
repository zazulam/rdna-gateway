#!/usr/bin/env python3
"""Draws the homelab diagrams as SVG into docs/img/ (dark, matching the decks).

    python3 docs/diagrams/make.py

Edit the layouts below when the lab changes; every coordinate is explicit.
"""
from pathlib import Path
from xml.sax.saxutils import escape

OUT = Path(__file__).resolve().parent.parent / "img"

BG, PANEL, PANEL2, EDGE = "#12151c", "#1c212b", "#171b23", "#343b4a"
FG, MUTED, DIM = "#d7dce4", "#8b93a1", "#5d6575"
CORAL, TEAL, YELLOW, GREEN, PURPLE, BLUE = "#ff6b7f", "#2ec4d6", "#f6c35f", "#5ad8c4", "#b892ff", "#6aa8ff"
FONT = "Inter, 'Helvetica Neue', Arial, sans-serif"
MONO = "'JetBrains Mono', Menlo, Consolas, monospace"


class Svg:
    def __init__(self, w, h, title, subtitle):
        self.w, self.h, self.parts = w, h, []
        self.colors = set()
        self.text(40, 52, title, 30, FG, bold=True)
        self.text(40, 80, subtitle, 15, MUTED)

    def text(self, x, y, s, size=14, color=FG, bold=False, anchor="start", mono=False, italic=False):
        w = ' font-weight="700"' if bold else ""
        it = ' font-style="italic"' if italic else ""
        fam = MONO if mono else FONT
        self.parts.append(f'<text x="{x}" y="{y}" font-family="{fam}" font-size="{size}" fill="{color}"'
                          f' text-anchor="{anchor}"{w}{it}>{escape(s)}</text>')

    def rect(self, x, y, w, h, fill=PANEL, stroke=EDGE, rx=12, dash=None, sw=1.5, opacity=None):
        d = f' stroke-dasharray="{dash}"' if dash else ""
        o = f' fill-opacity="{opacity}"' if opacity is not None else ""
        self.parts.append(f'<rect x="{x}" y="{y}" width="{w}" height="{h}" rx="{rx}" fill="{fill}"{o}'
                          f' stroke="{stroke}" stroke-width="{sw}"{d}/>')

    def group(self, x, y, w, h, label, color, dash="7 6", fill=None):
        self.rect(x, y, w, h, fill=fill or "none", stroke=color, rx=16, dash=dash, sw=1.6)
        tw = 9 + len(label) * 7.4
        self.rect(x + 18, y - 12, tw, 24, fill=BG, stroke="none", rx=6)
        self.text(x + 24, y + 5, label, 13, color, bold=True)

    def card(self, x, y, w, title, lines=(), accent=TEAL, sub=None, dash=None, pad=16, h=None):
        n = len(lines) + (1 if sub else 0)
        h = h or 44 + n * 20 + 8
        self.rect(x, y, w, h, fill=PANEL, stroke=accent if dash else EDGE, dash=dash)
        self.parts.append(f'<rect x="{x}" y="{y + 10}" width="4" height="{h - 20}" rx="2" fill="{accent}"/>')
        self.text(x + pad, y + 28, title, 16, FG, bold=True)
        yy = y + 50
        if sub:
            self.text(x + pad, yy, sub, 13, accent)
            yy += 20
        for ln in lines:
            if isinstance(ln, tuple):
                s, kind = ln
            else:
                s, kind = ln, ""
            self.text(x + pad, yy, s, 12.5 if kind == "mono" else 13, MUTED if kind != "fg" else FG, mono=kind == "mono")
            yy += 20
        return (x, y, w, h)

    def pill(self, x, y, s, color=MUTED, w=None):
        w = w or 18 + len(s) * 7.1
        self.rect(x, y, w, 28, fill=PANEL2, stroke=EDGE, rx=14)
        self.text(x + w / 2, y + 19, s, 12.5, color, anchor="middle")
        return w

    def arrow(self, pts, color=MUTED, dash=None, label=None, lx=None, ly=None, both=False, sw=2):
        self.colors.add(color)
        mid = color.strip("#")
        d = f' stroke-dasharray="{dash}"' if dash else ""
        p = " ".join(f"{a},{b}" for a, b in pts)
        start = f' marker-start="url(#s{mid})"' if both else ""
        self.parts.append(f'<polyline points="{p}" fill="none" stroke="{color}" stroke-width="{sw}"{d}'
                          f' stroke-linejoin="round" marker-end="url(#a{mid})"{start}/>')
        if label:
            if lx is None:
                (x1, y1), (x2, y2) = pts[len(pts) // 2 - 1], pts[len(pts) // 2]
                lx, ly = (x1 + x2) / 2, (y1 + y2) / 2 - 8
            tw = 10 + len(label) * 6.7
            self.rect(lx - tw / 2, ly - 15, tw, 21, fill=BG, stroke="none", rx=5, opacity=0.92)
            self.text(lx, ly, label, 12, color, anchor="middle")

    def line(self, pts, color=MUTED, dash=None, sw=2):
        d = f' stroke-dasharray="{dash}"' if dash else ""
        p = " ".join(f"{a},{b}" for a, b in pts)
        self.parts.append(f'<polyline points="{p}" fill="none" stroke="{color}" stroke-width="{sw}"{d}/>')

    def write(self, name):
        defs = []
        for c in sorted(self.colors):
            m = c.strip("#")
            defs.append(f'<marker id="a{m}" viewBox="0 0 10 10" refX="9" refY="5" markerWidth="7" markerHeight="7" '
                        f'orient="auto-start-reverse"><path d="M0,0 L10,5 L0,10 z" fill="{c}"/></marker>')
            defs.append(f'<marker id="s{m}" viewBox="0 0 10 10" refX="9" refY="5" markerWidth="7" markerHeight="7" '
                        f'orient="auto-start-reverse"><path d="M0,0 L10,5 L0,10 z" fill="{c}"/></marker>')
        svg = (f'<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 {self.w} {self.h}" width="{self.w}" height="{self.h}">'
               f'<defs>{"".join(defs)}</defs><rect width="100%" height="100%" fill="{BG}"/>'
               + "".join(self.parts) + "</svg>\n")
        OUT.mkdir(parents=True, exist_ok=True)
        (OUT / name).write_text(svg)
        print("wrote", OUT / name)


def topology():
    s = Svg(1600, 1040, "Homelab topology",
            "Devices, networks and addresses. Service traffic rides Tailscale; only the solar gateway is LAN-only.")
    # Internet
    s.group(40, 120, 1520, 96, "Internet", MUTED)
    x = 64
    for label in ["Tailscale coordination", "Hugging Face · GitHub (models, builds)", "Search engines (via SearXNG)",
                  "fetch_url (public sites only)", "ntfy.sh (iOS wake-up, no content)", "Enphase (token site)", "Sense cloud"]:
        x += s.pill(x, 150, label) + 12
    # Router
    s.card(690, 248, 220, "Router", ["LAN gateway"], accent=MUTED)
    s.line([(800, 216), (800, 248)], DIM)
    s.line([(800, 330), (800, 352)], DIM)
    # LAN + tailnet
    s.group(40, 352, 1250, 480, "Home LAN", MUTED)
    s.group(62, 400, 1206, 268, "Tailscale tailnet: service traffic between boxes", TEAL)
    s.card(84, 432, 370, "Desktop", [
        "Windows 11 · Ryzen 9 5950X · 64 GB DDR4",
        ("RX 6900 XT 16 GB", "fg"),
        ("100.64.0.10", "mono"),
        ("rdnagw gateway        :8080", "mono"),
        ("gemma4:12b  4 x 32k    :8081", "mono"),
        ("LibreHardwareMonitor  :8085", "mono"),
        "Logon tasks: worker, gateway",
    ], accent=CORAL, sub="Gateway + chat model")
    s.card(476, 432, 440, "Server box", [
        "Ubuntu 24.04 · Ryzen 5 7600X · 62 GB DDR5 · SFF",
        ("RX 6700 XT 12 GB", "fg"),
        ("100.64.0.20", "mono"),
        ("gpt-oss:20b 1 x 32k (GPU) :8081", "mono"),
        ("lfm2:8b (CPU)  :8082  rdnamcp :8090", "mono"),
        ("Grafana :3000  ntfy :8095  Prometheus", "mono"),
        "Qdrant · SearXNG · node_exporter · Enphase exporter",
    ], accent=BLUE, sub="Agent model + services + monitoring")
    s.card(938, 432, 310, "Jetson Orin Nano Super", [
        "JetPack 6.2 · 8 GB shared · 1 TB NVMe",
        ("MAXN SUPER (25 W)", "fg"),
        ("100.64.0.30", "mono"),
        ("embeddinggemma  :8083", "mono"),
        ("bge-reranker    :8084", "mono"),
        ("node_exporter   :9100", "mono"),
        "~8 W idle, 16 W under load",
    ], accent=GREEN, sub="Retrieval node")
    # LAN-only devices
    s.card(84, 716, 370, "Enphase IQ Gateway", [("LAN only", "mono"),
                                                "Local API (owner token) · integrated meter"], accent=YELLOW)
    s.card(476, 716, 300, "Sense monitor", ["Mains CTs · cloud API only"], accent=YELLOW)
    s.card(798, 716, 300, "RTX 3070 laptop (planned)", ["Ubuntu · 8 GB · not yet joined"], accent=DIM, dash="6 5")
    s.arrow([(560, 645), (560, 690), (300, 690), (300, 716)], YELLOW, dash="5 4",
            label="Enphase exporter reads the gateway over the LAN", lx=430, ly=686)
    s.arrow([(626, 788), (626, 812), (1300, 812), (1300, 216)], DIM, dash="5 4",
            label="Sense uploads to its cloud", lx=1120, ly=808)
    # Roaming clients
    s.card(1320, 400, 240, "MacBook Air (M4)", ["Hive app · Claude Code", ("100.64.0.40", "mono"),
                                                "Anywhere via Tailscale"], accent=PURPLE)
    s.card(1320, 560, 240, "iPhone", ["ntfy alerts · Grafana", "Needs Tailscale on"], accent=PURPLE)
    s.arrow([(1320, 470), (1268, 470)], TEAL, dash="6 5", both=True)
    s.arrow([(1320, 610), (1268, 610)], TEAL, dash="6 5", both=True)
    # Power
    s.group(40, 864, 1520, 150, "Power", YELLOW)
    s.card(64, 894, 270, "Utility net meter", ["Usage In / Out registers", "Net metering at retail"], accent=YELLOW)
    s.card(420, 894, 300, "Main panel", ["Enphase consumption CTs (if fitted)", "Sense mains CTs"], accent=YELLOW)
    s.card(806, 894, 300, "Solar array", ["Enphase microinverters", "Production CT"], accent=YELLOW)
    s.card(1192, 894, 344, "Lab draw (~186 W ≈ $34/mo)", ["Desktop ~127 W · server ~51 W · Jetson ~8 W",
                                                          "at your $/kWh rate"], accent=CORAL)
    s.arrow([(334, 939), (420, 939)], YELLOW, both=True)
    s.arrow([(806, 939), (720, 939)], YELLOW)
    s.arrow([(1192, 960), (1150, 960), (1150, 1002), (570, 1002), (570, 986)], YELLOW, dash="4 4")
    s.write("homelab-topology.svg")


def services():
    s = Svg(1600, 1130, "Service architecture",
            "One OpenAI-compatible endpoint for models, one MCP endpoint for tools, Prometheus + Grafana watching both.")
    # Clients
    s.group(40, 130, 270, 560, "Clients", PURPLE)
    s.card(60, 160, 230, "Hive", ["Chat, agents, workflows", "Offline harness: tool loop,", "built-in tools, compaction"],
           accent=PURPLE, sub="MacBook")
    s.card(60, 330, 230, "Any OpenAI client", ["curl · SDKs · pi · benchmarks"], accent=PURPLE)
    s.card(60, 470, 230, "Browser / phone", ["Grafana dashboards", "ntfy alert pushes"], accent=PURPLE)
    # Gateway
    s.card(360, 200, 330, "rdnagw gateway", [
        ("POST /v1/chat/completions", "mono"),
        ("POST /v1/embeddings, /v1/rerank", "mono"),
        "Routes by model name, then least-loaded,",
        "ties to the worker with the warm prefix",
        "Retries dropped connections, aborts stalls",
        ("/metrics · /metrics/workers/<name>", "mono"),
        ("/metrics/host  (LibreHardwareMonitor)", "mono"),
    ], accent=CORAL, sub="desktop :8080")
    # Workers
    s.group(750, 130, 420, 560, "Model workers (llama.cpp)", TEAL)
    s.card(772, 160, 376, "gemma4:12b", ["RX 6900 XT · ROCm · 4 slots x 32k", "q8 KV · 32 GB host prompt cache"],
           accent=CORAL, sub="desktop · chat")
    s.card(772, 300, 376, "gpt-oss:20b", ["RX 6700 XT · ROCm · 1 slot x 32k · 94 tok/s", "reasoning_effort low"],
           accent=BLUE, sub="server box · agent loops")
    s.card(772, 440, 376, "lfm2:8b", ["Ryzen 5 7600X CPU · 0.2-0.4 s per title", "Non-reasoning MoE"],
           accent=BLUE, sub="server box · quick jobs")
    s.card(772, 568, 180, "embeddinggemma", ["Jetson :8083"], accent=GREEN, h=96)
    s.card(968, 568, 180, "bge-reranker", ["Jetson :8084"], accent=GREEN, h=96)
    for y in (230, 370, 510):
        s.arrow([(690, 290), (720, 290), (720, y), (772, y)], CORAL)
    s.arrow([(690, 290), (720, 290), (720, 616), (772, 616)], CORAL)
    s.arrow([(290, 230), (360, 230)], PURPLE, label="models", lx=325, ly=222)
    # Tools
    s.group(1230, 130, 330, 560, "Agent tools", YELLOW)
    s.card(1250, 160, 290, "rdnamcp", [
        ("web_search · fetch_url", "mono"),
        ("search_docs · index_document", "mono"),
        ("remember · recall", "mono"),
        "MCP over HTTP, bearer token",
        "Refuses private / tailnet fetches",
    ], accent=YELLOW, sub="server box :8090")
    s.card(1250, 380, 140, "Qdrant", ["Vectors", "loopback only"], accent=YELLOW)
    s.card(1400, 380, 140, "SearXNG", ["Web search", "loopback only"], accent=YELLOW)
    s.arrow([(1320, 352), (1320, 380)], YELLOW)
    s.arrow([(1470, 352), (1470, 380)], YELLOW)
    s.arrow([(1250, 330), (1200, 330), (1200, 616), (1148, 616)], YELLOW, label="embed + rerank", lx=1196, ly=470)
    s.arrow([(1470, 480), (1470, 560)], DIM, dash="5 4")
    s.text(1395, 584, "to the internet: searches, and", 12, MUTED, anchor="middle")
    s.text(1395, 602, "fetch_url (public sites only)", 12, MUTED, anchor="middle")
    s.arrow([(175, 160), (175, 112), (1395, 112), (1395, 160)], PURPLE, label="MCP tools (bearer)", lx=785, ly=106)
    # Monitoring
    s.group(40, 760, 1520, 340, "Monitoring and alerts (server box)", GREEN)
    srcs = [("gateway /metrics", CORAL), ("workers via relay", CORAL), ("LibreHardwareMonitor via relay", CORAL),
            ("node_exporter · server", BLUE), ("node_exporter · Jetson", GREEN), ("rdnamcp tool calls", YELLOW),
            ("Qdrant", YELLOW), ("Enphase exporter → IQ Gateway", YELLOW)]
    for i, (lab, c) in enumerate(srcs):
        col, row = i % 2, i // 2
        s.pill(64 + col * 300, 800 + row * 44, lab, c, w=284)
    s.card(700, 800, 300, "Prometheus", ["Scrapes every 15-30 s · 90 days",
                                         "Recording rules: power per box,", "cost at $/kWh, solar share"],
           accent=GREEN, sub="loopback :9090")
    s.arrow([(652, 870), (700, 870)], GREEN)
    s.card(1060, 800, 220, "Grafana", ["RDNA inference dashboard", "18 alert rules"], accent=GREEN, sub=":3000")
    s.card(1320, 800, 220, "ntfy", ["Self-hosted push", "→ phone app"], accent=GREEN, sub=":8095")
    s.arrow([(1000, 860), (1060, 860)], GREEN)
    s.arrow([(175, 562), (175, 730), (1170, 730), (1170, 800)], PURPLE, dash="6 5", label="dashboards", lx=600, ly=724)
    s.arrow([(1280, 860), (1320, 860)], GREEN, label="alerts", lx=1300, ly=852)
    s.text(700, 1000, "Alerts: GPU hotspot and memory (both cards + Jetson), fan stalled, CPU / VRM heat,",
           13, MUTED)
    s.text(700, 1022, "disk under 10%, worker or exporter down, Jetson memory, sensor script stalled.", 13, MUTED)
    s.text(700, 1060, "Dashboard rows: pool · agent tools · energy and cost · health (server, desktop, Jetson)",
           13, FG)
    s.write("homelab-services.svg")


def harness():
    s = Svg(1600, 760, "Hive offline harness: one agent turn",
            "Branch offline-harness. Every model call goes through the gateway; tools run locally or through rdnamcp.")
    s.card(40, 140, 220, "User message", ["Chat or workflow step", "in a workspace"], accent=PURPLE)
    s.card(300, 140, 300, "Context planner", [
        "System prompt byte-stable across turns",
        "Summary sent as the first turn",
        "Compact at 85% of window → 50%",
        "Boundary kept across restarts",
        "Summaries back off after failures",
    ], accent=TEAL, sub="prefix-cache friendly")
    s.card(640, 140, 300, "Vault retrieval", [
        "Embed query (EmbeddingGemma)",
        "Top 20 vectors → rerank (bge)",
        "Top 5 passages added to the",
        "latest user turn, not the system",
    ], accent=GREEN, sub="optional")
    s.card(980, 140, 300, "OpenAI-wire tool loop", [
        "Round budget 24 · repeat guard",
        "Last round: tool_choice none",
        "Reasoning passed back (gpt-oss)",
        "Context guard stops tools near full",
    ], accent=CORAL, sub="gemma4:12b / gpt-oss:20b")
    s.card(1320, 140, 240, "Reply", ["Text answer", "File edits → one proposal", "for human review"],
           accent=PURPLE)
    for x1, x2 in ((260, 300), (600, 640), (940, 980), (1280, 1320)):
        s.arrow([(x1, 200), (x2, 200)], MUTED)
    s.card(760, 420, 380, "Built-in tools", [
        ("read_file · list_dir · search", "mono"),
        ("edit_file · write_file", "mono"),
        ("run_command (off unless enabled)", "mono"),
        "Per-turn git worktree, made on the first write",
        "Confined to the root; .git writes refused",
        "Offered only to local / private endpoints",
    ], accent=CORAL)
    s.card(1180, 420, 380, "MCP tools", [
        ("rdnamcp: web_search, fetch_url, search_docs,", "mono"),
        ("remember, recall …", "mono"),
        "Static bearer tokens stored on the device",
        "Exfiltration guard: no MCP calls after",
        "a turn has read files",
    ], accent=YELLOW)
    s.arrow([(1130, 330), (1130, 380), (950, 380), (950, 420)], CORAL, both=True, label="tool calls / results",
            lx=1040, ly=372)
    s.arrow([(1130, 330), (1130, 380), (1370, 380), (1370, 420)], YELLOW, both=True)
    s.card(40, 420, 300, "Quick tasks model", ["lfm2:8b on CPU", "Chat titles in ~0.6 s", "Falls back to the chat model"],
           accent=BLUE, sub="utility runtime")
    s.card(380, 420, 340, "Model profiles", ["gpt-oss: agent loops (reasoning low)", "gemma4: chat, summaries",
                                             "lfm2: quick jobs only", "Context from /props or 32k table"],
           accent=BLUE)
    s.rect(40, 640, 1520, 70, fill=PANEL2, stroke=EDGE)
    s.text(800, 682, "All model calls → rdnagw gateway (desktop :8080) → gemma4 · gpt-oss · lfm2 · embeddings · rerank",
           15, FG, anchor="middle")
    s.write("hive-harness.svg")


if __name__ == "__main__":
    topology()
    services()
    harness()
