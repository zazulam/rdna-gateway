#!/usr/bin/env bash
# Runs on the server box: start/stop a llama-server for an embedder or reranker
# on the GPU, next to the resident worker. Copied there by bench/rag.sh.
#
#   rag-gpu.sh start PORT MODEL_FILE ALIAS llama-server-args...
#   rag-gpu.sh stop PORT
set -euo pipefail
ROOT=${ROOT:-$HOME/llama/rocm} DEVICE=${DEVICE:-ROCm0}
cmd=$1 port=$2
pid=/tmp/rag-gpu-$port.pid
case $cmd in
start)
  file=$3 alias=$4; shift 4
  LD_LIBRARY_PATH=$ROOT setsid nohup "$ROOT/llama-server" -m "$HOME/models/$file" --alias "$alias" "$@" --device "$DEVICE" -ngl 999 \
    --host 127.0.0.1 --port "$port" -c ${CTX:-4096} -b ${UB:-1024} -ub ${UB:-1024} --parallel ${SLOTS:-2} >"/tmp/rag-gpu-$port.log" 2>&1 </dev/null &
  echo $! >"$pid"
  for _ in $(seq 90); do curl -sf "127.0.0.1:$port/health" >/dev/null && exit 0; sleep 1; done
  echo "$alias on :$port didn't come up:" >&2; tail -5 "/tmp/rag-gpu-$port.log" >&2; exit 1 ;;
stop)
  [[ -f $pid ]] && kill "$(cat "$pid")" 2>/dev/null || true
  rm -f "$pid"; sleep 2 ;;
esac
