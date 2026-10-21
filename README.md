# rdna-gateway

**Fast local LLMs on AMD RX 6000 (RDNA2) cards.** An OpenAI-compatible gateway in front of
llama.cpp workers, plus the benchmarks, monitoring and agent tools from a two-GPU home lab.

vLLM and SGLang don't support RDNA2, and the usual defaults (Ollama, Vulkan) leave a lot
of speed on the table. This repo is what came out of measuring the alternatives on real
hardware: an RX 6900 XT, an RX 6700 XT, a Jetson Orin Nano and a MacBook Air.

You can use it three ways:

- **Read the findings.** No setup needed: [what we learned](#what-we-learned) below, the
  [full results](docs/BENCHMARKS.md), and two slide decks
  ([inference](docs/deck/rdna2-benchmarks.pdf), [agent tools](docs/deck/agent-tools.pdf)).
- **Run a faster worker on one card.** Scripts install a ROCm build of llama.cpp and run it
  as a service on Windows or Linux.
- **Pool several machines** behind one endpoint, with failover, routing, monitoring and
  optional agent tools (web search, document retrieval, memory) over MCP.

![Service architecture: clients call one gateway, which routes to llama.cpp workers on two GPUs, a CPU and a Jetson; Prometheus and Grafana watch everything](docs/img/homelab-services.svg)

## What we learned

The numbers are from this repo's own harness ([docs/BENCHMARKS.md](docs/BENCHMARKS.md)).

### Running models on RDNA2

| Do this | Because |
|---|---|
| **Use llama.cpp's ROCm build, not Vulkan** | Vulkan prefill collapses on long prompts: a fresh 16k-token prompt took 238 s vs 14 s on ROCm. [More](docs/BENCHMARKS.md#kernels-llama-bench) |
| **Use the upstream GGUF, not Ollama's copy** | Ollama's file of the same model decoded at half the speed (28 vs 54 tok/s); ROCm + flash attention prefilled 5.8× faster than Ollama. |
| **Turn on flash attention; use q8_0 KV** | +19% prefill on ROCm. KV cache type costs no speed, so q8_0 is free VRAM. |
| **Run 4 slots × 32k context per card** | Best total throughput (62 vs 53 tok/s with 2 slots), under 10 GB of VRAM. [More](docs/BENCHMARKS.md#serving-one-card) |
| **Prefer small MoE models** | gpt-oss 20B ran 2.2× faster than dense gemma4 12B (2866 / 119 tok/s prefill / decode) with perfect needle recall. A 35B MoE fits a 16 GB card with `--n-cpu-moe 16` (37 tok/s). [More](docs/BENCHMARKS.md#dense-vs-moe) |

<picture>
  <source media="(prefers-color-scheme: dark)" srcset="docs/img/dark/micro-prefill.svg">
  <img alt="Prefill throughput by prompt length: ROCm holds 1,100 to 1,400 tokens/s on the 6900 XT while Vulkan falls from about 900 to 69" src="docs/img/micro-prefill.svg">
</picture>

<picture>
  <source media="(prefers-color-scheme: dark)" srcset="docs/img/dark/moe-speed.svg">
  <img alt="Decode and prefill speed per model on each card: the MoE models (LFM2.5, gpt-oss) are several times faster than dense gemma4 12B" src="docs/img/moe-speed.svg">
</picture>

### Serving agents

| Do this | Because |
|---|---|
| **Keep prefix caching on** | Agent sessions ran 2.7× faster; a cached follow-up at 16k context starts in 0.8 s instead of 14 s. [More](docs/BENCHMARKS.md#caching) |
| **Give spare RAM to `--cache-ram`** | With 6 agents on 2 slots, a 32 GB host prompt cache cut wall time by 42%. |
| **Never rewrite the system prompt to compact history** | It throws away the whole cache (0% reuse, 5.6 s to first token vs 1.8 s). Put the summary after it. [More](docs/BENCHMARKS.md#compaction) |
| **Route by load, not round-robin** | With two different cards, round-robin was up to 2× slower; least-loaded won on long contexts. [More](docs/BENCHMARKS.md#routing-across-two-cards) |
| **Use a slow machine only as a fallback** | The MacBook added nothing to the pool, but served everything when both GPU boxes were down. |

<picture>
  <source media="(prefers-color-scheme: dark)" srcset="docs/img/dark/context-ttft.svg">
  <img alt="Time to first token as context grows: with the prefix cached it stays under a second up to 16k tokens; without it, it climbs to 14 seconds" src="docs/img/context-ttft.svg">
</picture>

<picture>
  <source media="(prefers-color-scheme: dark)" srcset="docs/img/dark/routing.svg">
  <img alt="Routing policies across two cards: round-robin has the lowest throughput, especially with long contexts; least-loaded is best" src="docs/img/routing.svg">
</picture>

### Agent tools and retrieval

| Finding | Detail |
|---|---|
| **5 retrieved passages ≈ the right file** | Same accuracy (82.5%) as handing the model the exact file, with a fifth of the tokens. Pasting all the docs was 8× slower and often overflowed. [More](docs/BENCHMARKS.md#retrieval-vs-long-context) |
| **The smallest embedder won** | EmbeddingGemma 300M beat Qwen3-Embedding 0.6B by 25 points of recall@5. |
| **Always rerank** | A cross-encoder over the top 20 hits added 15 points of first-place accuracy. |
| **A Jetson makes a good retrieval node** | At 16 W it runs the embedder and reranker, freeing the GPU for a bigger chat model. [More](docs/BENCHMARKS.md#jetson-orin-nano-super-as-the-retrieval-node) |
| **Tiny CPU models handle quick jobs** | LFM2 8B-A1B writes titles and routes requests in 0.2–0.4 s on a desktop CPU. |

<picture>
  <source media="(prefers-color-scheme: dark)" srcset="docs/img/dark/rag-e2e.svg">
  <img alt="Answer accuracy, prompt tokens and time: the top 5 retrieved passages match the right file's accuracy with far fewer tokens and much less time than pasting the docs" src="docs/img/rag-e2e.svg">
</picture>

## What's here

| Part | What it does | Where |
|---|---|---|
| **Workers** | Install a llama.cpp ROCm build for gfx103X and run `llama-server` as a service | `workers/windows`, `workers/linux`, `workers/jetson` |
| **Gateway** (`rdnagw`) | One OpenAI-compatible endpoint: routing, failover, retries, streaming, `/metrics` | `cmd/rdnagw`, `internal/gateway` |
| **Agent tools** (`rdnamcp`) | MCP server: web search, page fetch, document search, memory, lab metrics | `cmd/rdnamcp`, `internal/{mcp,rag,tools}` |
| **Monitoring stack** | Docker Compose: Prometheus, Grafana dashboard, alerts to your phone, Qdrant, SearXNG | `deploy/sff` |
| **Energy exporter** | Reads an Enphase solar gateway for the power, cost and solar panels | `cmd/envoyexporter` |
| **Benchmarks** | Agent-shaped load generator, retrieval bench, scripts that run whole test matrices | `cmd/rdnabench`, `cmd/ragbench`, `bench/` |
| **Docs** | Results, architecture diagrams, slide decks (Marp sources included) | `docs/` |

## Requirements

- **GPU:** an RDNA2 card. gfx1030 is the RX 6800 / 6800 XT / 6900 XT / 6950 XT; gfx1031 the
  RX 6700 / 6700 XT / 6750 XT; gfx1032 the RX 6600 / 6600 XT / 6650 XT. The workers use the
  [lemonade-sdk llama.cpp ROCm builds](https://github.com/lemonade-sdk/llamacpp-rocm).
- **OS:** Windows 10/11 or Linux (tested on Ubuntu 24.04). The optional retrieval node is a
  Jetson Orin Nano on JetPack 6.2.
- **To build:** Go 1.25 or newer. **For the stack:** Docker with Compose.
- **Networking:** anything that lets the boxes reach each other. The lab uses
  [Tailscale](https://tailscale.com), and the examples use Tailscale-style `100.64.0.x`
  placeholder addresses: replace them with your own.

## Quick start

### 1 · One card: a faster worker

Windows (PowerShell):

```powershell
.\workers\windows\worker.ps1 -Install                        # download the ROCm gfx103X build
.\workers\windows\worker.ps1 -Model C:\models\gemma-4-12b-it-qat-q4_0.gguf -Alias gemma4:12b `
  -Extra '--cache-ram 32768' -Start                         # runs as a logon task
```

Linux:

```sh
./workers/linux/install.sh build
MODEL=~/models/gemma-4-12b-it-qat-q4_0.gguf ALIAS=gemma4:12b EXTRA="--cache-ram 32768" \
  ./workers/linux/install.sh service                         # systemd user unit
```

The worker is an OpenAI-compatible server on port 8081, set up as recommended above: flash
attention, 4 slots × 32k context, q8_0 KV and prefix cache reuse. Already have the model in
Ollama? `worker.ps1 -OllamaBlob <name>` or `install.sh ollama-blob <name>` prints its file
path, though the upstream GGUF is faster. For a model bigger than VRAM, add
`--n-cpu-moe N` (MoE models) and find the best `N` with `bench/micro.sh`.

Check it:

```sh
curl http://localhost:8081/v1/chat/completions -H 'Content-Type: application/json' \
  -d '{"model":"gemma4:12b","messages":[{"role":"user","content":"Say hi"}]}'
```

### 2 · Several machines: the gateway

Run a worker on each box (use the **same alias** everywhere a model is served), then:

```sh
go build -o bin/ ./cmd/...
cp gateway.example.json gateway.json      # list your workers
./bin/rdnagw -config gateway.json
```

A minimal `gateway.json`:

```json
{
  "listen": ":8080",
  "policy": "least-loaded",
  "workers": [
    { "name": "big-card",   "url": "http://100.64.0.10:8081", "slots": 4 },
    { "name": "small-card", "url": "http://100.64.0.20:8081", "slots": 4, "weight": 0.65 },
    { "name": "laptop",     "url": "http://100.64.0.40:8082", "slots": 1, "fallback": true }
  ]
}
```

`weight` scales a slower card's share of the load; `fallback` workers get traffic only when
every other worker is down.

Point any OpenAI-compatible client at `http://<gateway>:8080/v1`. It serves chat
completions, completions, embeddings and rerank, passes tool calls through (streaming or
not), and `/v1/models` lists the models of every healthy worker. Set `api_key` (or
`RDNAGW_API_KEY`) to require a bearer token. On Windows it can run as a logon task:
`.\workers\windows\gateway.ps1 -Install -Exe .\rdnagw.exe -Config .\gateway.json`.

### 3 · The full lab: monitoring and agent tools

`deploy/sff` turns an always-on Linux box into the lab's service host. Details in
[Agent services](#agent-services) below.

```sh
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o deploy/sff/bin/rdnamcp ./cmd/rdnamcp
cd deploy/sff && cp .env.example .env && $EDITOR .env   # addresses, models, Grafana password
./up.sh                                                 # generates secrets, renders configs, starts
```

## How routing works

Each response says where it went, in `X-Gateway-Worker`, `X-Gateway-Route` and
`X-Gateway-Matched-Blocks`.

| Policy / reason | When |
|---|---|
| `least-loaded` (default) | lowest in-flight ÷ (slots × weight); ties rotate |
| `prefix` / `overflow` | `cache-aware`: the worker with the longest cached prefix, unless it's full and another isn't |
| `cost` | `cost-aware`: the lowest estimated time to first token, from speeds learned off each worker's timings |
| `round-robin` | in turn, ignoring load |
| `retry` | the worker dropped or refused the connection before responding |
| `fallback` | no primary worker is up, so a `fallback: true` worker serves |

The gateway also retries dropped keep-alive connections, aborts stalled streams, and
exposes Prometheus metrics at `/metrics`.

## Agent services

The `deploy/sff` stack adds, on one Linux box:

- **Monitoring:** Prometheus and a Grafana dashboard covering workers, tokens/s, prompt
  cache hit rate, slots and queue, routing, agent tool calls, hardware health (GPU, CPU,
  VRM, RAM and NVMe temperatures, power, fans) and energy cost.

  ![The Grafana dashboard's pool row: workers up, tokens per second, prompt cache hit rate, gateway requests, slots and routing decisions](docs/img/grafana-pool.png)

- **Alerts** for heat, stalled fans, low disk and workers going down, pushed to your phone
  through a self-hosted [ntfy](https://ntfy.sh). On Windows, run LibreHardwareMonitor with its
  web server on and set `host_metrics` in `gateway.json`; the gateway relays those sensors
  at `/metrics/host` in a form Prometheus accepts.
- **Energy and solar (optional):** power per box (measured where the hardware reports it,
  estimated otherwise), cost at your electricity rate, and with an Enphase gateway, how much
  of the lab's energy your panels covered. Set the rates and `ENVOY_URL` in `.env`.
- **Agent tools over MCP** (`rdnamcp`, Streamable HTTP with a bearer token):

  | Tool | Backed by |
  |---|---|
  | `web_search`, `fetch_url` | SearXNG; fetches refuse LAN, tailnet and loopback addresses |
  | `search_docs`, `index_document`, `list_collections` | an embedder → Qdrant → a reranker |
  | `remember`, `recall` | the same, in a shared `memory` collection |
  | `metrics_query`, `metrics_range`, `metrics_names`, `lab_dashboard` | read-only Prometheus, plus the dashboard's own queries |

  Index a folder from any machine:
  `rdnamcp index -url http://<box>:8090/mcp -collection docs <paths>`.

The embedder and reranker are small llama.cpp servers. Run them next to the GPU worker:

```sh
NAME=embed MODEL=~/models/embeddinggemma-300M-Q8_0.gguf ALIAS=embeddinggemma-300m PORT=8083 HOST=<box-ip> \
  ARGS=--embedding ./workers/linux/install.sh aux
NAME=rerank MODEL=~/models/bge-reranker-v2-m3-Q8_0.gguf ALIAS=bge-reranker-v2-m3 PORT=8084 HOST=<box-ip> \
  ARGS=--reranking ./workers/linux/install.sh aux
```

or on a Jetson with `workers/jetson/setup.sh`, then set `EMBED_URL` and `RERANK_URL` in
`.env`. Grafana, ntfy, the model servers and the MCP endpoint listen only on `BIND_IP` (a
private address); Prometheus, node_exporter, Qdrant and SearXNG only on loopback.

How the pieces fit together: [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md).

## Reproducing the benchmarks

```sh
cp bench/lab.env.example bench/lab.env; $EDITOR bench/lab.env   # your hosts and paths
bench/models.sh desktop gemma4-12b     # fetch and verify models per host
bench/lab.sh micro                     # kernels (stops workers)
bench/lab.sh capacity | serve | memory | compaction | route | models | quality | mac
bench/rag.sh up | corpus | retrieval-gpu | e2e | speed | web | contention   # agent tools
python3 bench/plot.py                  # docs/img/*.svg + bench/results/tables.md
PLOT_THEME=dark python3 bench/plot.py  # docs/img/dark/*.svg, used by the decks
npx @marp-team/marp-cli docs/deck/rdna2-benchmarks.md --pdf --allow-local-files
```

Raw results are in `bench/results/*.jsonl`; every table is in
[bench/results/tables.md](bench/results/tables.md).

## Future work

Ideas for **longer contexts without slowing everything else down**. None of these are
measured yet; each would get a benchmark before becoming a default.

| Idea | Why it could help |
|---|---|
| **Length-aware routing to mixed slot sizes** | Most requests are short. Run most slots at 32k and one long-context slot per card (a second llama-server instance, or `--kv-unified-per-slot` on newer builds), and have the gateway route by estimated prompt length, so only long requests pay long-context costs. `--kv-unified` measured 2.3× slower on older builds, so that needs re-testing. |
| **q4_0 KV for the long slots** | KV cache type cost no speed on ROCm, and q4_0 roughly halves the cache's VRAM compared with q8_0, so about twice the context fits. The needle and recall benchmarks would show whether quality holds. |
| **Count resident sessions per slot** | The routing benchmarks showed prefix affinity overloading a warm card. Routing by sessions held, not just requests in flight, keeps long conversations cached instead of evicted. |
| **Speculative decoding** | The build has n-gram modes that need no second model and suit agents that repeat code and file contents, plus draft-model modes. Either could raise decode speed, which is what drops as context grows. |
| **Favour sliding-window and hybrid-attention models** | gemma4 and gpt-oss keep most of their cache to a fixed window, which is why 4×32k fits under 10 GB. Their cache grows far more slowly than a fully dense model's. |
| **Move context between workers** | Instead of re-processing a long conversation when it moves to another card, copy its KV cache across. That allows draining, rebalancing, promoting a session to a bigger slot, and parking idle sessions on disk. [The plan](docs/CONTEXT-MIGRATION.md). |
| **Retrieve and compact rather than stretch the window** | Already measured: five retrieved passages matched handing over the right file at a fifth of the tokens, and compaction kept recall at 100% for the larger models. Often the best "long context" is a shorter one. |

## Caveats

This is one person's home lab, published in case it helps others with the same hardware.
It's tested on the machines listed in [docs/BENCHMARKS.md](docs/BENCHMARKS.md), the 6900 XT
box doubles as an everyday desktop (so runs vary), and nothing here is hardened for the
public internet: keep it on a private network. Issues and pull requests are welcome.

## License

MIT, see [LICENSE](LICENSE).
