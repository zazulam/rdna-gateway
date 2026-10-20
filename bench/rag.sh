#!/usr/bin/env bash
# Retrieval and agent-tool benchmarks, run on the server box (where Qdrant is).
#
#   bench/rag.sh up          # start candidate embedders/rerankers (temporary containers)
#   bench/rag.sh corpus      # llama.cpp docs -> chunks, then generate questions
#   bench/rag.sh retrieval   # recall@k per embedder x reranker (CPU servers)
#   bench/rag.sh retrieval-gpu  # the same, with the models on the GPU (much faster)
#   bench/rag.sh e2e         # closed-book vs file vs long-context vs retrieval answering
#   bench/rag.sh speed       # embedding/rerank throughput, CPU vs the 6700 XT
#   bench/rag.sh web         # version questions with and without web tools
#   bench/rag.sh contention  # does CPU indexing slow the GPU worker or the CPU model?
#   bench/rag.sh down
#
# Results are appended to bench/results/rag.jsonl.
set -euo pipefail
cd "$(dirname "$0")/.."
source bench/lab.env

R=rag-bench # working dir on the server
IMG=ghcr.io/ggml-org/llama.cpp:server@sha256:143926008cbbbab8fae8fd87809875af3e42d3d676c30c11c55c0f3f683a5a07
LLAMA_TAG=${LLAMA_TAG:-b11723}
GW=${GW:-http://100.64.0.10:8080} # set GW in lab.env
OUT=bench/results/rag.jsonl

# name|port|file|extra llama-server args
EMBEDDERS=(
  "qwen3-embed-0.6b|8183|Qwen3-Embedding-0.6B-Q8_0.gguf|--embedding --pooling last"
  "embeddinggemma-300m|8184|embeddinggemma-300M-Q8_0.gguf|--embedding"
  "nomic-embed-1.5|8185|nomic-embed-text-v1.5.Q8_0.gguf|--embedding"
)
RERANKERS=(
  "bge-reranker-v2-m3|8186|bge-reranker-v2-m3-Q8_0.gguf|--reranking"
  "qwen3-reranker-0.6b|8187|qwen3-reranker-0.6b-q8_0.gguf|--reranking"
)

srv() { ssh -o BatchMode=yes "$SERVER_SSH" "$@"; }
dk() { echo "docker --context default"; }

flags() { # kind table-rows...: -embed/-rerank flags for ragbench
  local kind=$1 out="" e name port file args
  shift
  for e in "$@"; do
    IFS='|' read -r name port file args <<<"$e"
    out+=" -$kind $name=http://127.0.0.1:$port=$name"
  done
  echo "$out"
}

pull() { mkdir -p bench/results; scp -q "$SERVER_SSH:$R/rag.jsonl" /tmp/rag.$$ 2>/dev/null && cat /tmp/rag.$$ >>"$OUT" && rm /tmp/rag.$$ && srv "rm -f $R/rag.jsonl"; }

case "${1:-}" in
up)
  srv "mkdir -p $R"
  for e in "${EMBEDDERS[@]}" "${RERANKERS[@]}"; do
    IFS='|' read -r name port file args <<<"$e"
    srv "$(dk) rm -f rb-$name >/dev/null 2>&1; $(dk) run -d --name rb-$name --network host -v \$HOME/models:/models:ro $IMG \
      -m /models/$file --alias $name $args --host 127.0.0.1 --port $port -t ${THREADS:-4} -c 8192 -b 2048 -ub 2048 --parallel 4 >/dev/null"
  done
  for e in "${EMBEDDERS[@]}" "${RERANKERS[@]}"; do
    IFS='|' read -r name port _ <<<"$e"
    srv "for i in \$(seq 60); do curl -sf 127.0.0.1:$port/health >/dev/null && exit 0; sleep 1; done; echo $name not up; exit 1"
  done
  scp -q deploy/sff/bin/ragbench bench/rag-gpu.sh "$SERVER_SSH:$R/"
  echo "candidates up" ;;
down)
  for e in "${EMBEDDERS[@]}" "${RERANKERS[@]}"; do
    IFS='|' read -r name _ <<<"$e"
    srv "$(dk) rm -f rb-$name >/dev/null 2>&1 || true"
  done ;;
corpus)
  srv "cd $R && if [ ! -d llama.cpp ]; then mkdir llama.cpp && curl -fsSL https://codeload.github.com/ggml-org/llama.cpp/tar.gz/refs/tags/$LLAMA_TAG | tar xz -C llama.cpp --strip-components=1; fi && \
    ./ragbench corpus -dir llama.cpp -chunks chunks.jsonl && ./ragbench qa -chunks chunks.jsonl -qa qa.jsonl -n ${N:-150} -llm $GW -model gemma4:12b"
  srv "wc -l $R/chunks.jsonl $R/qa.jsonl" ;;
retrieval-gpu)
  # VRAM next to the worker fits one embedder plus one reranker at a time.
  for r in "${RERANKERS[@]}"; do
    IFS='|' read -r rname _ rfile rargs <<<"$r"
    srv "\$HOME/$R/rag-gpu.sh start 8291 $rfile $rname $rargs"
    for e in "${EMBEDDERS[@]}"; do
      IFS='|' read -r name _ file args <<<"$e"
      srv "\$HOME/$R/rag-gpu.sh start 8290 $file $name $args && cd $R && ./ragbench retrieval -chunks chunks.jsonl -qa qa.jsonl -n ${N:-150} -c 2 \
        -out rag.jsonl -label retrieval -embed $name=http://127.0.0.1:8290=$name -rerank $rname=http://127.0.0.1:8291=$rname; \$HOME/$R/rag-gpu.sh stop 8290"
    done
    srv "\$HOME/$R/rag-gpu.sh stop 8291"
  done
  pull ;;
retrieval)
  srv "cd $R && ./ragbench retrieval -chunks chunks.jsonl -qa qa.jsonl -n ${N:-150} -c 2 -out rag.jsonl -label retrieval \
    $(flags embed "${EMBEDDERS[@]}") $(flags rerank "${RERANKERS[@]}")"
  pull ;;
e2e)
  E=${E:-qwen3-embed-0.6b} RR=${RR:-bge-reranker-v2-m3}
  ep=$(printf '%s\n' "${EMBEDDERS[@]}" | grep "^$E|" | cut -d'|' -f2)
  rp=$(printf '%s\n' "${RERANKERS[@]}" | grep "^$RR|" | cut -d'|' -f2)
  srv "cd $R && ./ragbench e2e -chunks chunks.jsonl -qa qa.jsonl -n ${N:-40} -llm $GW -model ${MODEL:-gemma4:12b} -out rag.jsonl -label e2e \
    -embed $E=http://127.0.0.1:$ep=$E -rerank $RR=http://127.0.0.1:$rp=$RR"
  pull ;;
speed)
  srv "cd $R && ./ragbench speed -chunks chunks.jsonl -c 2 -out rag.jsonl -label cpu $(flags embed "${EMBEDDERS[@]}") $(flags rerank "${RERANKERS[@]}")"
  # The same models on the 6700 XT, next to the resident gemma4 worker.
  for e in "${EMBEDDERS[@]}" "${RERANKERS[@]}"; do
    IFS='|' read -r name port file args <<<"$e"
    k=rerank; [[ $args == *embedding* ]] && k=embed
    srv "\$HOME/$R/rag-gpu.sh start 8290 $file $name $args && cd $R && ./ragbench speed -chunks chunks.jsonl -c 2 -out rag.jsonl -label gpu -$k $name=http://127.0.0.1:8290=$name; \$HOME/$R/rag-gpu.sh stop 8290"
  done
  pull ;;
web)
  tok=$(srv 'sed -n "s/^RDNAMCP_TOKEN=//p" ~/rdna-sff/.env')
  for m in ${MODELS:-gemma4:12b lfm2:8b}; do
    srv "cd $R && RDNAMCP_TOKEN=$tok ./ragbench web -mcp http://$SERVER_IP:8090/mcp -llm $GW -model $m -out rag.jsonl -label web"
  done
  pull ;;
contention)
  # GPU worker throughput and CPU-model latency, idle vs. while the CPU embeds the corpus.
  go build -o bin/ ./cmd/rdnabench
  probe() { # label
    local lab=$1
    ./bin/rdnabench -url "$SERVER_URL" -model gemma4:12b -agents 4 -turns 3 -label "contention-gpu-$lab" -json bench/results/rag-contention.jsonl -tags "load=$lab" >/dev/null
    for i in $(seq 10); do
      curl -s -m 60 "http://$SERVER_IP:8082/v1/chat/completions" -H 'Content-Type: application/json' \
        -d '{"model":"lfm2:8b","messages":[{"role":"user","content":"Title for a chat about GPU routing, 5 words."}],"max_tokens":16}' |
        python3 -c "import sys,json;t=json.load(sys.stdin)['timings'];print(json.dumps({'kind':'contention-cpu','load':'$lab','seconds':(t['prompt_ms']+t['predicted_ms'])/1000,'tg':t['predicted_per_second']}))" >>bench/results/rag-contention.jsonl
    done
  }
  load_on() { # background loop on the server; the probe runs meanwhile
    srv "cd $R && while true; do ./ragbench speed -chunks chunks.jsonl -c 4 -out /dev/null $1 >/dev/null 2>&1; done" &
    loader=$!
    sleep 5
  }
  load_off() { kill $loader 2>/dev/null || true; srv "pkill -f '[r]agbench speed' || true"; sleep 2; }
  probe idle
  load_on "-embed qwen3-embed-0.6b=http://127.0.0.1:8183=qwen3-embed-0.6b" # CPU container
  probe indexing
  load_off
  srv "\$HOME/$R/rag-gpu.sh start 8291 bge-reranker-v2-m3-Q8_0.gguf bge-reranker-v2-m3 --reranking"
  load_on "-rerank bge-reranker-v2-m3=http://127.0.0.1:8291=bge-reranker-v2-m3"
  probe gpu-rerank
  load_off
  srv "\$HOME/$R/rag-gpu.sh stop 8291"
  echo "contention results in bench/results/rag-contention.jsonl" ;;
*)
  sed -n '2,14p' "$0"; exit 2 ;;
esac
