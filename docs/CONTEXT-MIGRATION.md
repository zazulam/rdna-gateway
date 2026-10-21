# Plan: moving context between workers

Status: **proposal, not implemented.** Nothing here is measured yet; phase 0 exists to
find out whether the idea pays off on RDNA2.

## The problem

A conversation's context lives in one worker's KV cache. Today, when the gateway sends
the next turn somewhere else (the first worker is busy, down, being restarted, or another
card has more room), the new worker has to process the whole prompt again. That's
expensive at long contexts: on the 6900 XT, a 16k-token follow-up takes about 14 s
uncached against 0.8 s cached ([BENCHMARKS](BENCHMARKS.md#caching)), and the 6700 XT
prefills about half as fast.

So the gateway avoids moving sessions, which works against it: load can't be rebalanced,
a worker can't be drained without making its users wait, and a session that outgrows its
slot can't be promoted to a bigger one.

**Goal:** move a session's KV cache to another worker in less time than re-processing it,
and fall back to re-processing whenever moving would be slower or unsafe.

## What llama-server already offers

The workers' build (llama.cpp b11719) has what's needed on each end:

| Feature | What it does |
|---|---|
| `--slot-save-path DIR` | enables `POST /slots/{id}?action=save` and `?action=restore` with a `filename`: the slot's KV cache is written to, or read from, a file in `DIR` |
| `id_slot` in a request | pins the request to one slot, so the next turn lands where the cache was restored |
| `GET /slots` | each slot's state: idle or busy, and how many tokens it holds |
| `--cache-ram` | the host-RAM prompt cache; `--cache-idle-slots` parks idle slots there |
| `--ctx-checkpoints` | checkpoints for sliding-window (SWA) models such as gemma, whose cache can't simply be truncated |

What's missing is everything between two workers: knowing which session lives where,
copying the file across machines, and deciding when moving beats re-processing.

## Design

```
            ┌──────────── gateway (rdnagw) ─────────────┐
 client ──▶ │ session table · migration planner · routing│
            └───────┬───────────────────────────┬───────┘
          save, GET │                           │ PUT, restore
            ┌───────▼───────┐   KV file   ┌─────▼─────────┐
            │ rdnaslot      │ ──────────▶ │ rdnaslot      │
            │ llama-server  │             │ llama-server  │
            │ (source)      │             │ (target)      │
            └───────────────┘             └───────────────┘
```

### 1. Sessions in the gateway

The gateway already cuts every prompt into 512-byte blocks and chains their hashes
(`internal/prefix`). A **session** is identified by the hash of its first few full blocks
(the system prompt and opening turn, which stay fixed for a conversation). The gateway
keeps a bounded table:

```
session → { worker, slot, tokens, last_used, model, kv_type }
```

It learns `slot` by matching the session against each worker's `GET /slots` (which lists
what every slot holds), or by pinning `id_slot` itself when it routes a session's first
turn; phase 1 checks which is reliable. This table is also what
the router needs to count *resident sessions per slot*, the improvement the routing
benchmarks already point to.

### 2. A slot agent on each worker (`rdnaslot`)

llama-server writes slot files to its own disk, and the boxes run different OSes, so each
worker gets a small sidecar (Go, the same binary on Windows and Linux) listening next to
llama-server on the private network:

| Endpoint | Does |
|---|---|
| `POST /save {slot, name}` | calls llama-server's save, then returns the file's size and SHA-256 |
| `GET /kv/{name}` | streams the file out |
| `PUT /kv/{name}` | receives a file, checks its SHA-256 |
| `POST /restore {slot, name}` | calls llama-server's restore and confirms the slot's token count |
| `DELETE /kv/{name}` | removes it; files are also deleted after restore and on a timer |

Every file travels with a manifest: model file SHA-256, llama.cpp build, KV cache type,
context per slot, token count. It needs the same bearer token as the gateway, because a KV
file is the conversation itself.

### 3. The migration planner

To move session S from worker A to worker B:

1. **Check compatibility.** Same model file (by SHA-256), same KV type, a slot on B big
   enough for S plus room to grow, and a build pair proven compatible in phase 0.
   Otherwise: don't move.
2. **Decide whether it's worth it.** Estimate both costs:

   ```
   move    = save_time + bytes / link_bandwidth + restore_time
   recompute = tokens / prefill_rate(B)
   ```

   `bytes ≈ tokens × 2 × full_attention_layers × kv_heads × head_dim × bytes_per_value`
   (sliding-window layers only add their window). The gateway already learns each
   worker's prefill rate for cost-aware routing; save, restore and bandwidth get learned
   the same way from real moves. Move only if `move` is clearly smaller.
3. **Move.** Wait until S's slot on A is idle; save; stream the file A → B (through the
   gateway or directly between sidecars); restore into an idle slot on B; confirm the token
   count.
4. **Switch.** Update the session table and send S's next request to B with `id_slot`
   set. If any step fails, the request simply goes to B (or back to A) and is re-processed,
   which is exactly today's behaviour.

### When to move

| Trigger | What happens |
|---|---|
| **Drain** (`rdnagw drain <worker>`) | move every movable session off a worker before a restart or model swap |
| **Rebalance** | one card's slots are all held by sessions while another sits empty |
| **Promote** | a session nears its 32k slot limit and a long-context slot exists elsewhere |
| **Park** | a session idles past N minutes: save it to disk, free the slot, restore on return |
| **Failover** | a dead worker can't save, so this only works for sessions parked or checkpointed beforehand |

## Phases

| Phase | Work | Done when |
|---|---|---|
| **0 · Measure** | Script `bench/` runs: save and restore time and file size at 4k / 16k / 32k on both cards; restore a slot saved on the 6900 XT into the 6700 XT (and Windows → Linux); greedy-decode the same continuation with and without a move; repeat on gemma4 (SWA) and gpt-oss | We know `move` vs `recompute` at each size, and whether cross-card and cross-OS restore produce identical output. If moving never wins, stop here. |
| **1 · Manual move** | `rdnaslot`; `rdnagw migrate --session S --to B` | A session moves between cards and its next turn starts at cached speed |
| **2 · Sessions** | session table, `id_slot` pinning, resident-session counting in the router, `rdnagw drain` | Draining a worker under agent load causes no re-processing for movable sessions |
| **3 · Automatic** | planner with the cost rule; rebalance and promote triggers | `rdnabench` scenario: mixed long and short sessions on two cards, with and without migration; aggregate tok/s and p50 time to first token both no worse |
| **4 · Parking** | idle sessions saved to NVMe and restored on demand | Long idle sessions stop taking slots, and coming back is faster than re-processing |

Metrics throughout: `rdnagw_migrations_total{trigger,result}`, bytes moved, save /
transfer / restore seconds, and re-processing avoided.

## Risks

- **Cross-card or cross-OS restore may not be exact.** The file holds raw cache values;
  different kernels on gfx1030 and gfx1031 could still be compatible, or not. Phase 0
  answers it; incompatible pairs are simply never chosen.
- **Files may be big.** By the formula above, a 32k-token cache can run from hundreds of MB
  to over a GB depending on the model, which is seconds on gigabit Ethernet. Moves would
  then pay off mainly for long contexts headed to the slower card. Phase 0 measures the
  real sizes.
- **Sliding-window models** (gemma) keep only part of their cache per layer. Restoring it
  should work, but how that interacts with checkpoints and cache reuse needs testing.
- **Saving blocks the slot**, so moves happen between turns, never mid-generation.
- **Privacy.** A KV file is the conversation. Keep it on the private network, authenticate
  the sidecar, delete files after restore.
- **API drift.** Slot save/restore is part of llama-server's documented API, but its file
  format follows the build. Pin builds in the manifest.
