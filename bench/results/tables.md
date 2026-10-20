### Prefill / decode, flash attention off vs on (tok/s)

| card · backend | pp512 FA off | pp512 FA on | tg128 FA off | tg128 FA on |
|---|---|---|---|---|
| RX 6900 XT · ROCm | 1199 | 1424 | 52 | 54 |
| RX 6900 XT · Vulkan | 1017 | 896 | 48 | 49 |
| RX 6700 XT · ROCm | 769 | 849 | 40 | 42 |
| M4 Air · Metal | 131 | 125 | 12 | 12 |

### Fresh-prompt prefill: tok/s (seconds to first token)

| card · backend | 512 | 2048 | 8192 | 16384 |
|---|---|---|---|---|
| RX 6900 XT · ROCm | 1420 (0.4s) | 1360 (1.5s) | 1258 (6.5s) | 1156 (14.2s) |
| RX 6900 XT · Vulkan | 896 (0.6s) | 508 (4.0s) | 304 (26.9s) | 69 (237.7s) |
| RX 6700 XT · ROCm | 843 (0.6s) | 800 (2.6s) | 736 (11.1s) | 672 (24.4s) |
| M4 Air · Metal | 116 (4.4s) | 107 (19.1s) | 84 (97.9s) | 69 (239.1s) |

### Single-stream decode at depth (tok/s)

| card · backend | @0 | @4096 | @16384 | @32768 |
|---|---|---|---|---|
| RX 6900 XT · ROCm | 54.4 | 49.9 | 47.4 | 44.6 |
| RX 6900 XT · Vulkan | 49.1 | 25.9 | 29.3 | 25.0 |
| RX 6700 XT · ROCm | 42.3 | 38.6 | 37.2 | 35.1 |
| M4 Air · Metal | 9.8 | 8.8 | 7.7 | 6.7 |

### KV cache type at 16k depth: prefill / decode (tok/s)

| card · backend | f16 | q8_0 | q4_0 |
|---|---|---|---|
| RX 6900 XT · ROCm | 990 / 47.8 | 986 / 46.8 | 988 / 47.2 |
| RX 6900 XT · Vulkan | 121 / 29.6 | 120 / 19.4 | 123 / 21.0 |
| RX 6700 XT · ROCm | 573 / 36.7 | 569 / 35.5 | 569 / 36.0 |
| M4 Air · Metal | 52 / 7.6 | 51 / 6.9 | 53 / 6.8 |

### Concurrency sweep (single worker, 8k-byte system prompt, 4 turns)

| card | slots | agents | agg tok/s | per-stream tok/s | follow-up TTFT p50 (s) | cache reuse | failed |
|---|---|---|---|---|---|---|---|
| RX 6900 XT | 2×32k | 1 | 36.7 | 48.8 | 0.68 | 68% | 0 |
| RX 6900 XT | 2×32k | 2 | 52.9 | 44.8 | 1.56 | 68% | 0 |
| RX 6900 XT | 2×32k | 4 | 50.6 | 41.9 | 6.48 | 62% | 0 |
| RX 6900 XT | 2×32k | 8 | 53.4 | 41.8 | 15.68 | 67% | 0 |
| RX 6900 XT | 4×8k | 1 | 36.8 | 48.8 | 0.77 | 68% | 0 |
| RX 6900 XT | 4×8k | 2 | 52.8 | 44.7 | 1.50 | 68% | 0 |
| RX 6900 XT | 4×8k | 4 | 61.4 | 31.2 | 2.88 | 68% | 0 |
| RX 6900 XT | 4×8k | 8 | 61.5 | 26.9 | 10.98 | 62% | 0 |
| RX 6900 XT | 4×32k | 1 | 36.7 | 48.8 | 0.69 | 68% | 0 |
| RX 6900 XT | 4×32k | 2 | 52.8 | 44.6 | 1.49 | 68% | 0 |
| RX 6900 XT | 4×32k | 4 | 62.6 | 32.6 | 3.48 | 68% | 0 |
| RX 6900 XT | 4×32k | 8 | 61.9 | 26.8 | 10.95 | 62% | 0 |
| RX 6700 XT | 2×32k | 1 | 26.3 | 37.2 | 1.13 | 68% | 0 |
| RX 6700 XT | 2×32k | 2 | 35.8 | 33.5 | 2.59 | 68% | 0 |
| RX 6700 XT | 2×32k | 4 | 33.7 | 30.8 | 9.97 | 62% | 0 |
| RX 6700 XT | 2×32k | 8 | 35.7 | 30.6 | 23.76 | 67% | 0 |
| RX 6700 XT | 4×8k | 1 | 25.9 | 37.2 | 1.12 | 68% | 0 |
| RX 6700 XT | 4×8k | 2 | 35.0 | 33.5 | 2.59 | 68% | 0 |
| RX 6700 XT | 4×8k | 4 | 41.3 | 20.0 | 4.97 | 68% | 0 |
| RX 6700 XT | 4×8k | 8 | 38.6 | 17.1 | 18.04 | 62% | 0 |
| RX 6700 XT | 4×32k | 1 | 26.4 | 37.3 | 1.12 | 68% | 0 |
| RX 6700 XT | 4×32k | 2 | 35.2 | 33.5 | 2.59 | 68% | 0 |
| RX 6700 XT | 4×32k | 4 | 39.4 | 18.9 | 4.38 | 68% | 0 |
| RX 6700 XT | 4×32k | 8 | 37.9 | 17.1 | 17.99 | 62% | 0 |

### Cached vs fresh prompts (2 agents × 6 turns, ~7k-token system prompt, RX 6900 XT 2×32k)

| mode | follow-up TTFT p50 (s) | TTFT p95 (s) | wall (s) | agg tok/s | cache reuse | mean prompt tok |
|---|---|---|---|---|---|---|
| cached | 1.83 | 9.49 | 37.0 | 41.6 | 79% | 7461 |
| no-cache | 12.76 | 15.16 | 100.0 | 15.4 | 0% | 7755 |

### TTFT vs context (1 agent × 4 turns)

| mean prompt tok | first turn (s) | follow-ups cached (s) | follow-ups cache off (s) | cache reuse |
|---|---|---|---|---|
| 1659 | 1.75 | 0.68 | 1.84 | 64% |
| 4489 | 3.64 | 0.70 | 4.10 | 71% |
| 10174 | 8.32 | 0.76 | 9.10 | 73% |
| 15889 | 13.57 | 0.84 | 14.40 | 74% |

### 6 agents on 2 slots (slot eviction), unique ~5k-token system prompts

| config | cache reuse | follow-up TTFT p50 (s) | p95 (s) | agg tok/s | failed |
|---|---|---|---|---|---|
| cache-ram 0 | 0% | 28.64 | 32.55 | 23.4 | 0 |
| cache-ram 8192 | 58% | 15.30 | 20.37 | 35.4 | 0 |
| cache-ram 32768 | 71% | 12.42 | 18.82 | 40.5 | 0 |
| swa-full | 71% | 19.36 | 50.77 | 18.7 | 0 |
| kv-unified | 58% | 45.75 | 58.19 | 15.5 | 0 |

### Routing policies, RX 6900 XT + RX 6700 XT (2×32k each), mean of runs

| scenario | policy | runs | agg tok/s | wall (s) | follow-up TTFT p50 (s) | TTFT p95 (s) | cache reuse | failed |
|---|---|---|---|---|---|---|---|---|
| short | round-robin | 2 | 60.2 | 85 | 5.53 | 25.77 | 62% | 0 |
| short | least-loaded | 2 | 75.5 | 68 | 8.05 | 12.67 | 59% | 0 |
| short | cache-aware | 2 | 75.6 | 68 | 7.85 | 13.02 | 61% | 0 |
| short | cost-aware | 2 | 75.3 | 68 | 8.26 | 12.88 | 61% | 0 |
| long | round-robin | 4 | 22.7 | 169 | 10.08 | 64.81 | 57% | 0 |
| long | least-loaded | 4 | 49.7 | 81 | 6.52 | 18.70 | 82% | 0 |
| long | cache-aware | 4 | 39.0 | 101 | 6.76 | 25.94 | 73% | 0 |
| long | cost-aware | 4 | 39.2 | 101 | 6.28 | 29.14 | 73% | 0 |

### MacBook Air M4 as a worker

| pool | agg tok/s | per-stream tok/s | follow-up TTFT p50 (s) | TTFT p95 (s) | wall (s) | requests per worker |
|---|---|---|---|---|---|---|
| mac alone | 6.8 | 11.5 | 5.08 | 6.32 | 75 | – |
| 2 gpus | 41.8 | 27.2 | 8.22 | 29.35 | 92 | desktop-6900xt:20, server-6700xt:10 |
| 2 gpus + mac | 40.9 | 27.1 | 8.38 | 36.07 | 94 | desktop-6900xt:20, server-6700xt:10 |
| fallback, gpus up | 47.8 | 44.8 | 1.49 | 4.95 | 16 | desktop-6900xt:6 |
| fallback, gpus down | 6.1 | 11.6 | 4.76 | 5.15 | 63 | mac-m4:3 |

### Memory per configuration (gemma4 12B)

| card | config | VRAM GiB | host RSS GiB after load | agg tok/s (4 agents) | cache reuse |
|---|---|---|---|---|---|
| 6900 XT | 4x8k q8_0 | 8.8 | 14.7 | 50.8 | 63% |
| 6900 XT | 4x32k q8_0 | 9.8 | 14.7 | 50.6 | 63% |
| 6900 XT | 2x32k cache-ram 0 | 8.6 | 8.2 | 24.3 | 0% |
| 6900 XT | 2x32k cache-ram 32G | 8.6 | 13.3 | 39.3 | 63% |
| 6900 XT | 2x32k q8_0 | 8.6 | 13.3 | 38.9 | 63% |
| 6900 XT | 2x32k f16 | 9.5 | 17.3 | 32.2 | 55% |
| 6900 XT | 2x32k q4_0 | 8.1 | 10.3 | 41.5 | 63% |
| 6900 XT | 2x32k swa-full | 15.7 | 10.2 | 16.7 | 63% |
| 6700 XT | 4x8k q8_0 | 8.3 | 7.3 | 30.9 | 63% |
| 6700 XT | 4x32k q8_0 | 9.3 | 13.0 | 30.0 | 63% |
| 6700 XT | 2x32k cache-ram 0 | 8.1 | 2.7 | 15.2 | 0% |
| 6700 XT | 2x32k cache-ram 32G | 8.1 | 7.7 | 24.9 | 63% |
| 6700 XT | 2x32k q8_0 | 8.1 | 7.7 | 24.9 | 63% |
| 6700 XT | 2x32k f16 | 9.0 | 11.8 | 20.1 | 55% |
| 6700 XT | 2x32k q4_0 | 7.6 | 4.7 | 27.1 | 63% |

### Compaction (2 agents × 12 turns, gemma4 12B on RX 6900 XT)

| strategy | wall (s) | compactions | summary call (s) | next-turn TTFT (s) | next-turn cached | follow-up TTFT p50 (s) | largest prompt | recall (compacted / recent) |
|---|---|---|---|---|---|---|---|---|
| no compaction | 108 | 0 | 0.0 | 0.00 | 0% | 3.01 | 14489 | 100% (0 / 100) |
| append @8k | 119 | 4 | 20.3 | 1.84 | 75% | 1.88 | 8933 | 100% (100 / 100) |
| append @12k | 121 | 2 | 33.8 | 1.99 | 37% | 2.72 | 12668 | 100% (100 / 100) |
| rewrite @8k | 135 | 4 | 25.4 | 5.62 | 0% | 2.52 | 8821 | 100% (100 / 100) |

### Dense vs MoE, best placement per card (llama-bench, FA on)

| model | card | expert layers on CPU | pp4096 tok/s | tg128 tok/s |
|---|---|---|---|---|
| gemma4 12B (dense) | RX 6900 XT | 0 | 1318 | 53.8 |
| LFM2.5 8B-A1B | RX 6900 XT | 0 | 5399 | 212.6 |
| gpt-oss 20B-A3.6B | RX 6900 XT | 0 | 2866 | 118.7 |
| Qwen3.6 35B-A3B | RX 6900 XT | 16 | 548 | 36.9 |
| gemma4 12B (dense) | RX 6700 XT | 0 | 767 | 41.6 |
| LFM2.5 8B-A1B | RX 6700 XT | 0 | 3224 | 197.1 |
| gpt-oss 20B-A3.6B | RX 6700 XT | 0 | 1696 | 99.6 |
| Qwen3.6 35B-A3B | RX 6700 XT | 20 | 149 | 31.5 |
| gemma4 12B (dense) | M4 Air | 0 | 46 | 7.0 |
| LFM2.5 8B-A1B | M4 Air | 0 | 311 | 37.6 |
| gpt-oss 20B-A3.6B | M4 Air | 0 | 350 | 31.2 |

### Serving per model (RX 6900 XT, 2×32k)

| model | agents | agg tok/s | per-stream tok/s | follow-up TTFT p50 (s) | cache reuse |
|---|---|---|---|---|---|
| gemma4 12B (dense) | 1 | 37.0 | 48.9 | 0.68 | 68% |
| gemma4 12B (dense) | 4 | 53.2 | 41.4 | 6.50 | 66% |
| LFM2.5 8B-A1B | 1 | 137.4 | 200.0 | 0.13 | 68% |
| LFM2.5 8B-A1B | 4 | 208.5 | 147.5 | 1.48 | 59% |
| gpt-oss 20B-A3.6B | 1 | 76.7 | 108.1 | 0.27 | 68% |
| gpt-oss 20B-A3.6B | 4 | 110.8 | 74.5 | 3.06 | 67% |
| Qwen3.6 35B-A3B | 1 | 20.4 | 34.2 | 1.16 | 68% |
| Qwen3.6 35B-A3B | 4 | 26.9 | 23.0 | 13.21 | 66% |

### Needle in a haystack (found / probes per context size)

| model | overall | 4k | 8k | 16k | 28k |
|---|---|---|---|---|---|
| gemma4 12B (dense) | 100% | 5/5 | 5/5 | 5/5 | 5/5 |
| LFM2.5 8B-A1B | 95% | 5/5 | 5/5 | 5/5 | 4/5 |
| gpt-oss 20B-A3.6B | 100% | 5/5 | 5/5 | 5/5 | 5/5 |
| Qwen3.6 35B-A3B | 100% | 5/5 | 5/5 | 5/5 | 5/5 |

### Fact recall at session end (2 agents × 10 turns)

| model | history | probes | recalled | of compacted-away facts | compactions |
|---|---|---|---|---|---|
| gemma4 12B (dense) | full history | 6 | 100% | – | 0 |
| LFM2.5 8B-A1B | full history | 6 | 100% | – | 0 |
| gpt-oss 20B-A3.6B | full history | 6 | 100% | – | 0 |
| Qwen3.6 35B-A3B | full history | 6 | 100% | – | 0 |
| gemma4 12B (dense) | compacted | 6 | 100% | 100% | 2 |
| LFM2.5 8B-A1B | compacted | 6 | 67% | 50% | 2 |
| gpt-oss 20B-A3.6B | compacted | 6 | 100% | 100% | 2 |
| Qwen3.6 35B-A3B | compacted | 6 | 100% | 100% | 2 |

### Retrieval quality: 150 questions over llama.cpp's docs (1,732 passages); rerankers rescore the top 30

| embedder | reranker | questions | recall@1 | recall@5 | recall@10 | MRR@10 | search p50 ms (6700 XT) |
|---|---|---|---|---|---|---|---|
| Qwen3-Embedding 0.6B | vector search only | 150 | 27% | 52% | 65% | 0.38 | 22 |
| Qwen3-Embedding 0.6B | + bge-reranker-v2-m3 | 150 | 47% | 75% | 77% | 0.60 | 1924 |
| Qwen3-Embedding 0.6B | + Qwen3-Reranker 0.6B | 150 | 48% | 75% | 76% | 0.60 | 3644 |
| EmbeddingGemma 300M | vector search only | 150 | 41% | 77% | 83% | 0.57 | 19 |
| EmbeddingGemma 300M | + bge-reranker-v2-m3 | 150 | 59% | 87% | 91% | 0.72 | 1802 |
| EmbeddingGemma 300M | + Qwen3-Reranker 0.6B | 150 | 56% | 87% | 89% | 0.70 | 2319 |
| nomic-embed v1.5 | vector search only | 150 | 43% | 71% | 83% | 0.56 | 14 |
| nomic-embed v1.5 | + bge-reranker-v2-m3 | 150 | 57% | 87% | 89% | 0.71 | 1743 |
| nomic-embed v1.5 | + Qwen3-Reranker 0.6B | 150 | 56% | 87% | 89% | 0.70 | 2304 |

### Rerank depth: how many vector hits the reranker rescores

| embedder | reranker | candidates | recall@1 | recall@5 |
|---|---|---|---|---|
| Qwen3-Embedding 0.6B | bge-reranker-v2-m3 | 10 | 43% | 64% |
| Qwen3-Embedding 0.6B | bge-reranker-v2-m3 | 20 | 46% | 71% |
| Qwen3-Embedding 0.6B | bge-reranker-v2-m3 | 30 | 47% | 75% |
| Qwen3-Embedding 0.6B | Qwen3-Reranker 0.6B | 10 | 45% | 65% |
| Qwen3-Embedding 0.6B | Qwen3-Reranker 0.6B | 20 | 46% | 71% |
| Qwen3-Embedding 0.6B | Qwen3-Reranker 0.6B | 30 | 48% | 75% |
| EmbeddingGemma 300M | bge-reranker-v2-m3 | 10 | 56% | 81% |
| EmbeddingGemma 300M | bge-reranker-v2-m3 | 20 | 58% | 85% |
| EmbeddingGemma 300M | bge-reranker-v2-m3 | 30 | 59% | 87% |
| EmbeddingGemma 300M | Qwen3-Reranker 0.6B | 10 | 55% | 81% |
| EmbeddingGemma 300M | Qwen3-Reranker 0.6B | 20 | 55% | 85% |
| EmbeddingGemma 300M | Qwen3-Reranker 0.6B | 30 | 56% | 87% |
| nomic-embed v1.5 | bge-reranker-v2-m3 | 10 | 53% | 81% |
| nomic-embed v1.5 | bge-reranker-v2-m3 | 20 | 56% | 85% |
| nomic-embed v1.5 | bge-reranker-v2-m3 | 30 | 57% | 87% |
| nomic-embed v1.5 | Qwen3-Reranker 0.6B | 10 | 53% | 80% |
| nomic-embed v1.5 | Qwen3-Reranker 0.6B | 20 | 55% | 85% |
| nomic-embed v1.5 | Qwen3-Reranker 0.6B | 30 | 56% | 87% |

### Embedding and rerank speed

| model | device | throughput | query latency |
|---|---|---|---|
| Qwen3-Embedding 0.6B | CPU | 1.7 passages/s | 107 ms |
| EmbeddingGemma 300M | CPU | 5.4 passages/s | 30 ms |
| nomic-embed v1.5 | CPU | 5.4 passages/s | 28 ms |
| Qwen3-Embedding 0.6B | GPU | 28.8 passages/s | 15 ms |
| EmbeddingGemma 300M | GPU | 76.1 passages/s | 9 ms |
| nomic-embed v1.5 | GPU | 77.1 passages/s | 6 ms |
| bge-reranker-v2-m3 | CPU | 30 candidates: 14.37 s p50 | 20.12 s p95 |
| Qwen3-Reranker 0.6B | CPU | 30 candidates: 19.81 s p50 | 42.32 s p95 |
| bge-reranker-v2-m3 | GPU | 30 candidates: 0.95 s p50 | 1.31 s p95 |
| Qwen3-Reranker 0.6B | GPU | 30 candidates: 1.20 s p50 | 2.35 s p95 |

### Answering llama.cpp questions with gemma4 12B (40 questions, cache off)

| context | correct (overflow = wrong) | overflowed 32k slot | correct when it fit | prompt tokens | prefill s | total s (mean) | total s (p95) |
|---|---|---|---|---|---|---|---|
| no docs | 20.0% | 0 | 20% | 73 | 0.3 | 2.3 | 3.9 |
| the right file (if you knew it) | 82.5% | 2 | 87% | 10106 | 23.1 | 24.8 | 70.8 |
| ~26k tokens of docs pasted in | 75.0% | 9 | 97% | 25684 | 67.8 | 69.8 | 113.8 |
| top 5 passages (search_docs) | 82.5% | 0 | 82% | 2034 | 4.3 | 8.5 | 17.2 |

### Web search: 'what is the latest version of X' (truth fetched live)

| model | mode | questions | correct | tool calls (mean) | s (mean) | s (p95) |
|---|---|---|---|---|---|---|
| gemma4:12b | closed-book | 11 | 0% | 0.0 | 1.0 | 1.4 |
| gemma4:12b | web-tools | 11 | 82% | 1.5 | 5.8 | 13.2 |
| lfm2:8b | closed-book | 11 | 0% | 0.0 | 0.4 | 0.7 |
| lfm2:8b | web-tools | 11 | 36% | 0.9 | 5.3 | 6.5 |

### Contention: retrieval work vs. the GPU worker and the CPU model

| background load | gemma4 on 6700 XT, 4 agents (agg tok/s) | lfm2 title job (median s) |
|---|---|---|
| idle | 49.7 | 0.23 |
| indexing | 46.3 | 0.28 |
| gpu-rerank | 24.9 | 0.22 |
