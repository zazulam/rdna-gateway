---
marp: true
theme: default
paginate: true
size: 16:9
title: RDNA2 local inference benchmarks
style: |
  section { font-family: "Inter", "Helvetica Neue", Arial, sans-serif; font-size: 24px; padding: 48px 64px;
            background: #12151c; color: #d7dce4; }
  section::after { color: #6c7586; }
  h1 { color: #ff6b7f; font-size: 44px; }
  h2 { color: #2ec4d6; font-size: 34px; margin-bottom: 12px; }
  h3 { color: #9aa3b2; }
  strong { color: #ffffff; }
  em { color: #f6c35f; }
  a { color: #2ec4d6; }
  code { background: #232834; color: #f6c35f; border-radius: 4px; padding: 0 4px; }
  img { display: block; margin: 0 auto; background: transparent; }
  table { font-size: 19px; margin: 0 auto; border-collapse: collapse; }
  table th { background: #232834; color: #ffffff; border-color: #343b4a; }
  table td { background: #171b23; border-color: #343b4a; }
  table tr:nth-child(2n) td { background: #1c212b; }
  .kpis { display: grid; grid-template-columns: repeat(4, 1fr); gap: 14px; margin-top: 10px; }
  .kpi { background: #1c212b; border: 1px solid #2c3240; border-radius: 12px; padding: 14px 16px; }
  .kpi b { display: block; font-size: 40px; color: #ff6b7f; line-height: 1.1; }
  .kpi span { font-size: 17px; color: #aab2c0; }
  .small { font-size: 18px; color: #8b93a1; }
  .cols { display: grid; grid-template-columns: 1fr 1fr; gap: 28px; }
---

# RDNA2 as a first-class local inference target

### rdna-gateway: benchmarks, findings, recommendations

RX 6900 XT (Windows) · RX 6700 XT (Ubuntu) · M4 MacBook Air
llama.cpp ROCm gfx103X · cache-aware OpenAI-compatible gateway

<span class="small">October 2026 · every number is reproducible with bench/lab.sh</span>

---

## Key indicators

<div class="kpis">
<div class="kpi"><b>5.8×</b><span>prefill vs Ollama on the same card (ROCm + flash-attn)</span></div>
<div class="kpi"><b>2×</b><span>decode from the upstream GGUF vs Ollama's copy of the same model</span></div>
<div class="kpi"><b>2.7×</b><span>faster agent sessions with prefix caching (37 s vs 100 s)</span></div>
<div class="kpi"><b>−42%</b><span>wall time from a 32 GB host-RAM prompt cache under slot pressure</span></div>
<div class="kpi"><b>2.1×</b><span>least-loaded vs round-robin on long contexts, two cards</span></div>
<div class="kpi"><b>119</b><span>tok/s decode, gpt-oss 20B MoE on the 6900 XT (2.2× gemma4 12B)</span></div>
<div class="kpi"><b>37</b><span>tok/s, Qwen3.6 35B MoE on a 16 GB card via expert offload</span></div>
<div class="kpi"><b>83</b><span>tok/s aggregate, 8 agents on the deployed two-card pool, 0 failures</span></div>
</div>

---

## The lab

| | RX 6900 XT | RX 6700 XT | MacBook Air |
|---|---|---|---|
| GPU | gfx1030 · 16 GB · 512 GB/s | gfx1031 · 12 GB · 384 GB/s | M4 · 24 GB unified · 120 GB/s |
| Host | Win 11 · 5950X · 64 GB | Ubuntu 24.04 · Ryzen 7000 · 62 GB | macOS, fanless |
| Runtime | llama.cpp ROCm gfx103X (+ Vulkan for A/B) | llama.cpp ROCm gfx103X | llama.cpp Metal |

**Models:** gemma4 12B Q4_0 (dense) · LFM2.5 8B-A1B · gpt-oss 20B MXFP4 · Qwen3.6 35B-A3B Q4_K_M

**Workload:** agent loops with a long system prompt, tool results each turn, and fixed 128-token replies.
vLLM and SGLang don't support gfx1030, so llama.cpp + a router *is* the RDNA2 serving stack.

---

<!-- _paginate: true -->

![bg contain](../img/homelab-topology.svg)

---

![bg contain](../img/homelab-services.svg)

---

## Backend: ROCm, never Vulkan, for long prompts

![h:470](../img/dark/micro-prefill.svg)

<span class="small">Vulkan prefill collapses to 69 tok/s at 16k tokens: a fresh 16k prompt takes 238 s, versus 14 s on ROCm.</span>

---

## Flash attention: on for ROCm

![h:440](../img/dark/micro-flash-attn.svg)

**+19% prefill** on ROCm. It *hurts* on Vulkan. The M4 Air is shown for scale.

---

## KV-cache type is free on ROCm

![h:440](../img/dark/micro-kv.svg)

f16, q8_0 and q4_0 run at **identical speed** at 16k depth, so compress the KV and spend the VRAM on slots and context.

---

## Decode holds up deep into context

![h:430](../img/dark/micro-depth.svg)

The 6900 XT goes from 54 to 45 tok/s at 32k tokens of history, and the 6700 XT from 42 to 35. Vulkan halves after 4k.

---

## Slots: 4 × 32k is the layout

![h:400](../img/dark/capacity.svg)

4 slots raise aggregate throughput (62 vs 53 tok/s) at the cost of per-stream speed. Under 10 GB of VRAM on both cards.

---

## Memory per configuration

![h:420](../img/dark/memory-vram.svg)

<span class="small">The sliding-window cache keeps 4×32k cheap. `--swa-full` doesn't fit 2×32k on 12 GB. Host RSS grows with the RAM prompt cache.</span>

---

## Caching: fresh vs cached prompts

![h:400](../img/dark/cache-vs-fresh.svg)

2 agents × 6 turns at about 7k tokens: **37 s cached vs 100 s fresh** (2.7×).

---

## Time to first token as context grows

![h:430](../img/dark/context-ttft.svg)

A follow-up turn at 16k tokens starts in **0.8 s cached** versus **14 s fresh**.

---

## Host-RAM cache, SWA and unified KV

![h:330](../img/dark/host-cache.svg)

6 agents on 2 slots: evicted conversations restore from RAM. **Off 131 s → 8 GB 87 s → 32 GB 76 s.**
`--swa-full` (1.9×) and `--kv-unified` (2.3×) made it **slower**: keep the defaults.

---

## Compaction: where you put the summary matters

![h:330](../img/dark/compaction.svg)

- **Summary after the system prompt:** the next turn is 75% cached and starts in 1.8 s. **Summary inside it:** 0% cached, 5.6 s.
- Each summary costs 20–34 s, so compact late. At about 16k tokens, never compacting was fastest.
- Recall survived compaction for gemma4, gpt-oss and Qwen3.6 (100%). LFM2.5 (1B active) kept half.

---

## Routing across two cards

![h:400](../img/dark/routing.svg)

Round-robin loses 25–50%. On long contexts **least-loaded beats prefix affinity**: affinity overfills a warm card's slots and forces evictions.

---

## Dense vs small MoE

![h:420](../img/dark/moe-speed.svg)

gpt-oss 20B: **2,866 / 119 tok/s** (prefill / decode) on the 6900 XT, 2.2× gemma4 12B. <span class="small">The Mac was thermally throttled by this point.</span>

---

## MoE bigger than VRAM: expert offload

![h:420](../img/dark/moe-offload.svg)

`--n-cpu-moe` keeps the experts of N layers in system RAM. Qwen3.6 35B peaks at **N = 16 → 37 tok/s** on 16 GB; fewer overflows VRAM.

---

## Quality: needle in a haystack

![h:380](../img/dark/needle.svg)

One planted fact, 4k–28k tokens, 5 depths, cache off. gemma4, gpt-oss and Qwen3.6: **20/20**; LFM2.5: 19/20.
<span class="small">Reasoning models need an answer budget: at 160 tokens gpt-oss and LFM2.5 "failed" by running out mid-thought.</span>

---

## Quality: agent memory across compaction

![h:420](../img/dark/recall.svg)

Facts planted in tool results, quizzed at session end: gemma4, gpt-oss and Qwen3.6 **100%** after compaction; LFM2.5 **67%**: the smallest model's summaries drop facts.

---

## The MacBook: fallback, not a worker

![h:300](../img/dark/mac-third-worker.svg)

- 11× slower prefill, 4–8× slower decode, and it throttles (12.5 → 7 tok/s under sustained load).
- As a pool member the cost-aware router gave it **0 of 30** requests.
- As `fallback: true` it served everything, without errors, while both GPU boxes were down.

---

## Recommended deployment (live: 8 agents → 83 tok/s)

| setting | value | why |
|---|---|---|
| runtime | llama.cpp ROCm gfx103X, `-fa on` | 5.8× Ollama's prefill |
| model file | upstream GGUF | 2× Ollama-blob decode |
| slots | `--parallel 4 -c 131072` (4×32k) | best aggregate, under 10 GB |
| KV | `-ctk q8_0 -ctv q8_0` | free on ROCm |
| prompt cache | `--cache-ram 32768`, `--cache-reuse 256` | −42% under slot pressure |
| leave off | `--swa-full`, `--kv-unified` | 1.9× / 2.3× slower |
| routing | `least-loaded`, 6700 XT `weight` 0.65 | best on long contexts |
| Mac | `fallback: true` | availability, not speed |

---

## Improving utilization (no new hardware)

1. **Pick the model per job:** gpt-oss 20B for fast agent loops (2.2× gemma4, and it keeps facts through compaction); Qwen3.6 35B when quality matters; LFM2.5 only for short, uncompacted tasks.
2. **Specialize the cards:** gpt-oss fits the 6700 XT entirely (100 tok/s, faster than gemma4 on the 6900 XT), and the gateway already routes by model.
3. **Router v2:** count resident sessions per slot, not just in-flight requests, so affinity stops before a warm card overflows.
4. **Compaction policy in Hive:** summary after a stable system prompt, triggered near the slot limit.
5. **Speculative decoding:** an EAGLE-3 draft for gpt-oss exists (0.9 GB). Next experiment.
6. **Keep the 6900 XT's VRAM clear:** the desktop compositor already holds 2.5 GB, and a game would evict the model.

---

## Upgrades: what extra hardware would buy

Decode is memory-bandwidth-bound and MoE offload is VRAM-bound. *Estimates are from bandwidth ratios, not measured.*

| option | effect | stays on RDNA2? |
|---|---|---|
| **Radeon Pro W6800 32 GB** (gfx1030) | Qwen3.6 35B entirely in VRAM: est. 2–3× its offloaded 37 tok/s; two models resident | ✅ same stack |
| **2nd 16 GB RDNA2 card** in the desktop | a third worker: est. +50–60 tok/s aggregate; or layer split for 32 GB models | ✅ |
| **RX 7900 XTX 24 GB** (RDNA3, 960 GB/s) | est. 1.8× decode; also unlocks vLLM / SGLang (gfx1100) | ❌ new gen |
| faster system RAM | helps only MoE expert offload | – |
| **skip:** Mac as a worker, faster LAN, more CPU cores | not the bottleneck | – |

---

## Methodology

- `bench/micro.sh`: llama-bench suites (FA, prefill length, decode depth, KV type, ubatch, MoE offload sweeps).
- `bench/lab.sh`: reconfigures workers over SSH, verifies the served model, and runs serving, cache, compaction, routing, memory and quality matrices.
- `rdnabench`: agent traffic, `-no-cache`, `-compact-at`, `-facts` recall, `-needle`.
- Long routing runs were repeated 4×; the error bars show the min/max spread.
- Caveat: the 6900 XT is someone's everyday desktop, so background load adds variance.

<span class="small">github.com/zazulam/rdna-gateway · docs/BENCHMARKS.md · bench/results/tables.md</span>
