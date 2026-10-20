#!/usr/bin/env bash
# Kernel-level benchmarks (llama-bench) for one host. The host's llama-server
# must be stopped first (it holds the VRAM); see bench/lab.sh.
#
#   bench/micro.sh desktop rocm     # Windows box, ROCm build
#   bench/micro.sh desktop vulkan   # Windows box, Vulkan build
#   bench/micro.sh server rocm      # Linux box
#   bench/micro.sh mac metal        # this Mac
#   bench/micro.sh server rocm moe-x -fa 1 -p 512 -n 128 -ncmoe 8   # one custom suite
#
# Appends one JSON object per measurement to bench/results/micro.jsonl, tagged
# with host, backend and suite.
set -euo pipefail
cd "$(dirname "$0")/.."
source bench/lab.env

host="$1" backend="$2"
shift 2
out=bench/results/micro.jsonl
mkdir -p bench/results
reps="${REPS:-2}"

run() { # suite, llama-bench args...
  local suite="$1"; shift
  echo ">> $host/$backend $suite: $*" >&2
  local raw
  case "$host" in
    desktop) raw=$(ssh -n -o BatchMode=yes "$DESKTOP_SSH" "$DESKTOP_ROOT\\$backend\\llama-bench.exe -m $DESKTOP_MODEL -o jsonl -r $reps $*") ;;
    server) raw=$(ssh -n -o BatchMode=yes "$SERVER_SSH" "LD_LIBRARY_PATH=$SERVER_ROOT/rocm $SERVER_ROOT/rocm/llama-bench -m $SERVER_MODEL -dev $SERVER_DEVICE -o jsonl -r $reps $*") ;;
    mac) raw=$("$MAC_LLAMA/llama-bench" -m "$MAC_MODEL" -o jsonl -r "$reps" "$@") ;;
    *) echo "unknown host $host" >&2; exit 2 ;;
  esac
  printf '%s\n' "$raw" | tr -d '\r' | grep '^{' | python3 -c '
import json, sys
host, backend, suite = sys.argv[1:4]
for line in sys.stdin:
    d = json.loads(line)
    d.update(host=host, backend_label=backend, suite=suite)
    print(json.dumps(d))' "$host" "$backend" "$suite" >>"$out"
}

if [[ $# -gt 0 ]]; then # a single named suite with explicit llama-bench args
  run "$@"
  exit 0
fi

# 1. Flash attention on/off, the standard pp512/tg128 pair.
run fa -fa 0,1 -p 512 -n 128
# 2. Fresh-prompt prefill at growing prompt lengths (what an uncached TTFT costs).
run prefill -fa 1 -p 512,2048,8192,16384 -n 0
# 3. Decode speed deep into a context (long agent histories).
run depth -fa 1 -p 0 -n 128 -d 0,4096,16384,32768
# 4. KV-cache type: speed at a 16k-token depth (VRAM is the other half of the trade).
for kv in f16 q8_0 q4_0; do
  run "kv-$kv" -fa 1 -ctk "$kv" -ctv "$kv" -p 2048 -n 128 -d 16384
done
# 5. Physical batch size for prefill.
run ubatch -fa 1 -b 2048 -ub 256,512,1024,2048 -p 4096 -n 0
