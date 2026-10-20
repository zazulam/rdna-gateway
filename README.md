# rdna-gateway

One OpenAI-compatible endpoint in front of a pool of llama.cpp workers on AMD
RDNA2 cards (RX 6000 series: gfx1030/1031/1032). vLLM and SGLang don't support
these GPUs.

- **Workers** run `llama-server` from a ROCm gfx103X build with flash attention,
  4×32k slots, q8_0 KV, prefix cache reuse and a host-RAM prompt cache. They run on
  Windows (logon task) or Linux (systemd user unit).
- **The gateway** (`rdnagw`) streams, fails over, retries dropped keep-alive
  connections, aborts stalled streams, and exposes `/metrics`. It routes by
  `least-loaded` (the default), `cache-aware` (an SGLang-style chained-block prefix
  index), `cost-aware` (estimated queue wait plus uncached prefill, from speeds
  learned off each worker's own timings) or `round-robin`. Workers marked
  `fallback: true` get traffic only when every primary is down.
- **The bench** (`rdnabench`) replays agent-shaped traffic. It can turn caching off,
  run history compaction, plant facts for recall checks, and run
  needle-in-a-haystack probes. `bench/lab.sh` drives whole matrices over SSH.

**Architecture:** [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) · **Results:** [docs/BENCHMARKS.md](docs/BENCHMARKS.md) · **Slides:**
[inference](docs/deck/rdna2-benchmarks.pdf) and [agent tools](docs/deck/agent-tools.pdf) (Marp sources alongside).

## Headline numbers (RX 6900 XT, unless noted)

| | |
|---|---|
| ROCm + FA prefill vs Ollama on the same card | **5.8×** (1370 vs 236 tok/s) |
| upstream GGUF vs Ollama's copy of the same model, decode | **2×** (54 vs 28 tok/s) |
| Vulkan prefill at a 16k-token prompt | 69 tok/s, vs **1156** on ROCm |
| agent session with prefix caching vs cache off | **2.7×** faster |
| 32 GB host-RAM prompt cache, 6 agents on 2 slots | **−42%** wall time |
| two cards, long contexts: least-loaded vs round-robin | **2.1×** aggregate |
| gpt-oss 20B (MoE), prefill / decode | **2866 / 119** tok/s |
| Qwen3.6 35B-A3B on 16 GB via `--n-cpu-moe 16` | **37** tok/s |

## Quick start

Worker on Windows (PowerShell):

```powershell
.\workers\windows\worker.ps1 -Install                        # ROCm gfx103X build
.\workers\windows\worker.ps1 -Model C:\models\gemma-4-12b-it-qat-q4_0.gguf -Alias gemma4:12b `
  -Extra '--cache-ram 32768' -Start
```

Worker on Linux:

```sh
./workers/linux/install.sh build
MODEL=~/models/gemma-4-12b-it-qat-q4_0.gguf ALIAS=gemma4:12b EXTRA="--cache-ram 32768" \
  ./workers/linux/install.sh service
```

Use the same alias on every worker that serves a model, and prefer upstream
GGUFs over Ollama's blobs. Models bigger than VRAM: add `--n-cpu-moe N` (MoE) and
sweep `N` with `bench/micro.sh`.

Gateway:

```sh
go build -o bin/ ./cmd/...
cp gateway.example.json gateway.json   # list your workers
./bin/rdnagw -config gateway.json
# or on Windows, as a logon task:
.\workers\windows\gateway.ps1 -Install -Exe .\rdnagw.exe -Config .\gateway.json
```

Point any OpenAI-compatible client at `http://<gateway>:8080/v1`. Set `api_key`
(or `RDNAGW_API_KEY`) to require a bearer token. Tool calls pass through,
streaming or not.

## Routing reasons

Responses carry `X-Gateway-Worker`, `X-Gateway-Route` and `X-Gateway-Matched-Blocks`:

| reason | when |
|---|---|
| `least-loaded` | lowest in-flight ÷ (slots × weight); ties rotate |
| `prefix` / `overflow` | cache-aware: the longest cached prefix, unless that card is full and another isn't |
| `cost` | cost-aware: the lowest estimated seconds to the first token |
| `retry` | the worker dropped or refused the connection before responding |
| `fallback` | no primary worker is up |

## Agent services on a Linux worker box

`deploy/sff/` is a Docker Compose stack for an always-on Linux worker (the GPU worker and
two small retrieval models stay systemd units on its GPU):

- **Monitoring:** Prometheus and a Grafana "RDNA inference" dashboard: workers, tok/s,
  prompt cache hit rate, slots and queue, per-stream speed, routing, failures, agent tool
  calls and latency, and hardware health (GPU edge/junction/memory temperature, power,
  fan, CPU/VRM/RAM/NVMe temperature). Workers that listen only on the gateway's host are
  read through the gateway's `/metrics/workers/<name>` relay.
- **Alerts** for heat, a stalled fan, low disk and workers going down, pushed to your phone
  by a self-hosted ntfy. On Windows, run LibreHardwareMonitor with its web server on and set
  `"host_metrics": "http://127.0.0.1:8085/metrics"` in gateway.json: the gateway relays its
  sensors at `/metrics/host`, regrouped into valid Prometheus text (LibreHardwareMonitor
  repeats TYPE lines, which Prometheus rejects).
- **A CPU model for quick jobs** (titles, classification, picking an agent): LFM2 8B-A1B
  answers in 0.2–0.4 s. Add it to the gateway as another worker. Use LFM2, not LFM2.5,
  which always reasons first and took about 6 s for the same jobs.
- **Agent tools over MCP** (`rdnamcp`, Streamable HTTP, bearer token):

  | tool | backed by |
  |---|---|
  | `web_search`, `fetch_url` | SearXNG; fetches refuse LAN, tailnet and local addresses |
  | `search_docs`, `index_document`, `list_collections` | EmbeddingGemma 300M → Qdrant → bge-reranker-v2-m3, on the GPU |
  | `remember`, `recall` | the same, in a shared `memory` collection |

  On llama.cpp's docs, `search_docs` found the source passage in the top 5 for 87% of
  questions, and answering from its top 5 was as accurate as handing gemma4 the right file,
  at a fifth of the tokens (docs/BENCHMARKS.md). Index a folder from any machine with
  `rdnamcp index -url http://<box>:8090/mcp -collection docs <paths>`.

```sh
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o deploy/sff/bin/rdnamcp ./cmd/rdnamcp
cd deploy/sff && cp .env.example .env && $EDITOR .env   # addresses, Grafana password
./up.sh                                                 # generates secrets, renders prometheus.yml, starts
# retrieval models on the GPU, next to the worker:
NAME=embed MODEL=~/models/embeddinggemma-300M-Q8_0.gguf ALIAS=embeddinggemma-300m PORT=8083 HOST=<tailscale-ip> \
  ARGS=--embedding ./workers/linux/install.sh aux
NAME=rerank MODEL=~/models/bge-reranker-v2-m3-Q8_0.gguf ALIAS=bge-reranker-v2-m3 PORT=8084 HOST=<tailscale-ip> \
  ARGS=--reranking ./workers/linux/install.sh aux
```

Grafana, ntfy, the model servers and the MCP endpoint listen only on `BIND_IP` (a
Tailscale address); Prometheus, node_exporter, Qdrant and SearXNG only on loopback.

## Reproducing the benchmarks

```sh
cp bench/lab.env.example bench/lab.env; $EDITOR bench/lab.env   # your hosts and paths
bench/models.sh desktop gemma4-12b     # fetch and verify models per host
bench/lab.sh micro                     # kernels (stops workers)
bench/lab.sh capacity | serve | memory | compaction | route | models | quality | mac
bench/rag.sh up | corpus | retrieval-gpu | e2e | speed | web | contention   # agent tools
python3 bench/plot.py                  # docs/img/*.svg + bench/results/tables.md
PLOT_THEME=dark python3 bench/plot.py  # docs/img/dark/*.svg, used by the deck
npx @marp-team/marp-cli docs/deck/rdna2-benchmarks.md --pdf --allow-local-files
```

## License

MIT, see [LICENSE](LICENSE).
