#!/usr/bin/env bash
# Drives the benchmark lab: reconfigures workers over SSH and runs the matrices.
#
#   bench/lab.sh start desktop 2 65536 q8_0 "--cache-ram 8192"   # slots ctx kv extra
#   bench/lab.sh stop server
#   bench/lab.sh micro            # llama-bench suites on every card (stops workers)
#   bench/lab.sh serve            # single-worker serving matrix (desktop)
#   bench/lab.sh capacity         # concurrency sweep on each card
#   bench/lab.sh route            # routing-policy matrix across both cards
#   bench/lab.sh mac              # MacBook as a third worker / fallback
#   bench/lab.sh memory           # VRAM / host RAM per configuration
#   bench/lab.sh compaction       # history compaction: cost, cache impact, recall
#   bench/lab.sh models           # MoE vs dense: speed, CPU expert offload, recall
#
# MODEL_ALIAS and the *_MODEL paths can be overridden per call (see lab.env).
# Results land in bench/results/*.jsonl; bench/plot.py turns them into graphs.
set -euo pipefail
cd "$(dirname "$0")/.."
source bench/lab.env
mkdir -p bench/results
BENCH=./bin/rdnabench
GW=./bin/rdnagw
GW_URL=http://127.0.0.1:8090

log() { printf '\n### %s\n' "$*" >&2; }

start() { # host slots ctx kv extra
  start_ "$@" || return 1
  # Never benchmark a survivor: the worker must report the model we asked for.
  local got
  got=$(curl -fsS -m 10 "$(url_of "$1")/v1/models" | python3 -c 'import json,sys; print(json.load(sys.stdin)["data"][0]["id"])' 2>/dev/null || true)
  if [[ "$got" != "$MODEL_ALIAS" ]]; then
    echo "!! $1 is serving '$got', expected '$MODEL_ALIAS'" >&2
    return 1
  fi
}

start_() {
  local host="$1" slots="$2" ctx="$3" kv="$4" extra="${5:-}"
  log "start $host: ${slots}x$((ctx / slots)) kv=$kv $extra"
  case "$host" in
    desktop)
      ssh -n -o BatchMode=yes "$DESKTOP_SSH" "powershell -NoProfile -ExecutionPolicy Bypass -File $DESKTOP_ROOT\\worker.ps1 -Model $DESKTOP_MODEL -Alias $MODEL_ALIAS -Start -Parallel $slots -Ctx $ctx -Kv $kv -Extra \"${extra:- }\"" ;;
    server)
      ssh -n -o BatchMode=yes "$SERVER_SSH" "MODEL=$SERVER_MODEL ALIAS=$MODEL_ALIAS PARALLEL=$slots CTX=$ctx KV=$kv EXTRA='$extra' ./install.sh service" ;;
    mac)
      stop mac
      # Loopback only: the gateway runs on this Mac, nothing else needs it.
      nohup "$MAC_LLAMA/llama-server" -m "$MAC_MODEL" --alias "$MODEL_ALIAS" -ngl 999 -fa on \
        -c "$ctx" --parallel "$slots" -ctk "$kv" -ctv "$kv" --cache-reuse 256 --metrics --slots \
        --host 127.0.0.1 --port "${MAC_URL##*:}" $extra >bench/results/mac-server.log 2>&1 &
      for _ in $(seq 90); do curl -fs "$MAC_URL/health" >/dev/null && return; sleep 2; done
      echo "mac worker did not come up" >&2; return 1 ;;
  esac
}

stop() {
  case "$1" in
    desktop) ssh -n -o BatchMode=yes "$DESKTOP_SSH" "powershell -NoProfile -ExecutionPolicy Bypass -File $DESKTOP_ROOT\\worker.ps1 -Stop" ;;
    server) ssh -n -o BatchMode=yes "$SERVER_SSH" "systemctl --user stop llama-worker" ;;
    mac) pkill -f "llama-server .*--port ${MAC_URL##*:}" || true; sleep 1 ;;
  esac
}

url_of() { case "$1" in desktop) echo "$DESKTOP_URL" ;; server) echo "$SERVER_URL" ;; mac) echo "$MAC_URL" ;; esac; }

bench() { # file label tags rdnabench-args...
  local file="$1" label="$2" tags="$3"; shift 3
  log "$label"
  "$BENCH" -model "$MODEL_ALIAS" -json "bench/results/$file" -label "$label" -tags "$tags" "$@" ||
    echo "!! $label failed" >&2
}

# mem <host> <label> <file> <phase>: snapshot the worker's VRAM and host-RAM use.
mem() {
  local host="$1" label="$2" file="${3:-memory.jsonl}" phase="${4:-idle}" out
  case "$host" in
    desktop) out=$(ssh -n -o BatchMode=yes "$DESKTOP_SSH" "powershell -NoProfile -Command \"\$v=((Get-Counter '\GPU Adapter Memory(*)\Dedicated Usage').CounterSamples | Measure-Object CookedValue -Sum).Sum; \$r=(Get-Process llama-server -ErrorAction SilentlyContinue | Measure-Object WorkingSet64 -Sum).Sum; '{0} {1}' -f [int](\$v/1MB), [int](\$r/1MB)\"" | tr -d '\r') ;;
    server) out=$(ssh -n -o BatchMode=yes "$SERVER_SSH" 'v=0; for c in /sys/class/drm/card*/device; do t=$(cat $c/mem_info_vram_total 2>/dev/null || echo 0); [ "$t" -gt 4000000000 ] && v=$(( $(cat $c/mem_info_vram_used) / 1048576 )); done; r=$(ps -o rss= -C llama-server | awk "{s+=\$1} END {print int(s/1024)}"); echo "$v $r"') ;;
    *) return 0 ;;
  esac
  python3 -c 'import json,sys; v,r=sys.argv[4].split(); print(json.dumps({"host":sys.argv[1],"label":sys.argv[2],"phase":sys.argv[3],"vram_mib":int(v),"rss_mib":int(r)}))' \
    "$host" "$label" "$phase" "$out" >>"bench/results/$file" || true
  echo "   mem $host $label $phase: $out (VRAM MiB, host RSS MiB)" >&2
}

# gateway <config-json>: (re)start the gateway on loopback with this worker list.
gateway() {
  pkill -f "$GW -config" 2>/dev/null || true
  local cfg=bench/results/.gateway.json
  printf '%s\n' "$1" >"$cfg"
  "$GW" -config "$cfg" >>bench/results/gateway.log 2>&1 &
  for _ in $(seq 30); do curl -fs "$GW_URL/health" >/dev/null && return; sleep 1; done
  echo "gateway did not come up" >&2; return 1
}

micro() {
  stop desktop; stop server
  bench/micro.sh desktop rocm
  bench/micro.sh desktop vulkan
  bench/micro.sh server rocm
  bench/micro.sh mac metal
}

# Concurrency sweep per card, at the two slot layouts.
capacity() {
  for host in ${@:-desktop server}; do
    for layout in "4 32768" "2 65536" "4 131072"; do
      set -- $layout
      start "$host" "$1" "$2" q8_0 ""
      for a in 1 2 4 8; do
        bench capacity.jsonl "$host ${1}x$(($2 / $1)) agents=$a" "host=$host,slots=$1,ctx=$2,agents=$a" \
          -url "$(url_of "$host")" -agents "$a" -turns 4 -system-chars 8000 -seed $((a * 7))
      done
    done
  done
}

# Cache matrix on one card (desktop, 2x32k): fresh vs cached, context growth,
# host-RAM prompt cache under slot pressure, SWA cache mode.
serve() {
  local u="$DESKTOP_URL"
  start desktop 2 65536 q8_0 ""
  # Fresh prompts vs cached, at an agent-sized context.
  bench cache.jsonl "cached" "mode=cached" -url "$u" -agents 2 -turns 6 -system-chars 24000 -tool-chars 3000 -seed 101
  bench cache.jsonl "no-cache" "mode=no-cache" -url "$u" -agents 2 -turns 6 -system-chars 24000 -tool-chars 3000 -seed 102 -no-cache -unique-system
  # TTFT as the system prompt grows: first turn (cold) vs follow-ups (warm).
  for sc in 4000 16000 40000 64000; do
    bench context.jsonl "context $sc" "system_chars=$sc,mode=cached" -url "$u" -agents 1 -turns 4 -system-chars "$sc" -seed $((sc + 1))
    bench context.jsonl "context $sc no-cache" "system_chars=$sc,mode=no-cache" -url "$u" -agents 1 -turns 4 -system-chars "$sc" -seed $((sc + 2)) -no-cache
  done
  # 6 agents on 2 slots: slots get evicted, so reuse depends on the host-RAM cache.
  for cram in 0 8192 32768; do
    start desktop 2 65536 q8_0 "--cache-ram $cram"
    bench hostcache.jsonl "cache-ram $cram" "cache_ram=$cram" -url "$u" -agents 6 -turns 4 -system-chars 16000 -unique-system -seed $((cram + 5))
  done
  start desktop 2 65536 q8_0 "--swa-full"
  bench hostcache.jsonl "swa-full" "cache_ram=8192,swa=full" -url "$u" -agents 6 -turns 4 -system-chars 16000 -unique-system -seed 9
  start desktop 2 65536 q8_0 "--kv-unified"
  bench hostcache.jsonl "kv-unified" "cache_ram=8192,kv=unified" -url "$u" -agents 6 -turns 4 -system-chars 16000 -unique-system -seed 10
}

# Routing policies across both cards at the deployment layout (2x32k each).
route() {
  start desktop 2 65536 q8_0 ""
  start server 2 65536 q8_0 ""
  gateway "{\"listen\":\"127.0.0.1:8090\",\"workers\":[
    {\"name\":\"desktop-6900xt\",\"url\":\"$DESKTOP_URL\",\"slots\":2},
    {\"name\":\"server-6700xt\",\"url\":\"$SERVER_URL\",\"slots\":2,\"weight\":0.8}]}"
  local rep
  for rep in 1 2; do
    for pol in round-robin least-loaded cache-aware cost-aware; do
      bench route.jsonl "short $pol #$rep" "scenario=short,policy=$pol,rep=$rep" -url "$GW_URL" -policy "$pol" \
        -agents 8 -turns 5 -system-chars 8000 -seed $((rep * 100 + 1))
      bench route.jsonl "long $pol #$rep" "scenario=long,policy=$pol,rep=$rep" -url "$GW_URL" -policy "$pol" \
        -agents 6 -turns 5 -system-chars 40000 -tool-chars 4000 -unique-system -seed $((rep * 100 + 2))
    done
  done
}

# Quality per model with a reasoning-sized answer budget: needle and recall.
quality() {
  local name file dx sx macok
  while IFS='|' read -r -u 3 name file dx sx macok; do
    MODEL_ALIAS="$name" DESKTOP_MODEL="$DESKTOP_MODELS\\$file"
    start desktop 2 65536 q8_0 "$dx" || { echo "!! $name did not start" >&2; continue; }
    bench needle.jsonl "$name needle" "model=$name,probe_tokens=1024" -url "$DESKTOP_URL" -probe-tokens 1024 \
      -needle 4000,8000,16000,28000 -needle-depths 0,25,50,75,100 -seed 602
    bench recall.jsonl "$name full history" "model=$name,style=none,probe_tokens=1024" -url "$DESKTOP_URL" -probe-tokens 1024 \
      -agents 2 -turns 10 -system-chars 8000 -tool-chars 4000 -facts -seed 603
    bench recall.jsonl "$name compacted" "model=$name,style=append,probe_tokens=1024" -url "$DESKTOP_URL" -probe-tokens 1024 \
      -agents 2 -turns 10 -system-chars 8000 -tool-chars 4000 -facts -compact-at 9000 -seed 603
  done 3<<<"$MODEL_SET"
  unset MODEL_ALIAS DESKTOP_MODEL; source bench/lab.env
  start desktop 2 65536 q8_0 ""
}

# Extra repetitions of the long routing scenario (it has the most run-to-run variance).
route_more() {
  start desktop 2 65536 q8_0 ""
  start server 2 65536 q8_0 ""
  gateway "{\"listen\":\"127.0.0.1:8090\",\"workers\":[
    {\"name\":\"desktop-6900xt\",\"url\":\"$DESKTOP_URL\",\"slots\":2},
    {\"name\":\"server-6700xt\",\"url\":\"$SERVER_URL\",\"slots\":2,\"weight\":0.8}]}"
  local rep pol
  for rep in ${@:-3 4}; do
    for pol in round-robin least-loaded cache-aware cost-aware; do
      bench route.jsonl "long $pol #$rep" "scenario=long,policy=$pol,rep=$rep" -url "$GW_URL" -policy "$pol" \
        -agents 6 -turns 5 -system-chars 40000 -tool-chars 4000 -unique-system -seed $((rep * 100 + 2))
    done
  done
}

# Does a MacBook help? As a weighted third worker, and as fallback-only.
mac() {
  start desktop 2 65536 q8_0 ""
  start server 2 65536 q8_0 ""
  start mac 1 32768 q8_0 ""
  bench mac.jsonl "mac alone" "pool=mac-alone" -url "$MAC_URL" -agents 1 -turns 4 -system-chars 8000 -seed 301
  local two="{\"name\":\"desktop-6900xt\",\"url\":\"$DESKTOP_URL\",\"slots\":2},{\"name\":\"server-6700xt\",\"url\":\"$SERVER_URL\",\"slots\":2,\"weight\":0.8}"
  gateway "{\"listen\":\"127.0.0.1:8090\",\"workers\":[$two]}"
  bench mac.jsonl "2 gpus" "pool=2gpu" -url "$GW_URL" -policy cost-aware -agents 6 -turns 5 -system-chars 40000 -tool-chars 4000 -unique-system -seed 302
  gateway "{\"listen\":\"127.0.0.1:8090\",\"workers\":[$two,{\"name\":\"mac-m4\",\"url\":\"$MAC_URL\",\"slots\":1,\"weight\":0.3}]}"
  bench mac.jsonl "2 gpus + mac" "pool=2gpu+mac" -url "$GW_URL" -policy cost-aware -agents 6 -turns 5 -system-chars 40000 -tool-chars 4000 -unique-system -seed 302
  # Fallback-only: the Mac takes traffic only when no GPU worker is up.
  gateway "{\"listen\":\"127.0.0.1:8090\",\"health_interval\":\"2s\",\"workers\":[$two,{\"name\":\"mac-m4\",\"url\":\"$MAC_URL\",\"slots\":1,\"fallback\":true}]}"
  bench mac.jsonl "fallback, gpus up" "pool=fallback-gpus-up" -url "$GW_URL" -policy cost-aware -agents 2 -turns 3 -system-chars 8000 -seed 303
  stop desktop; stop server; sleep 5
  bench mac.jsonl "fallback, gpus down" "pool=fallback-gpus-down" -url "$GW_URL" -policy cost-aware -agents 1 -turns 3 -system-chars 8000 -seed 304
  start desktop 2 65536 q8_0 ""; start server 2 65536 q8_0 ""
  stop mac
}

# VRAM and host RAM per configuration, idle (after load) and after a burst.
memory() {
  local host label slots ctx kv extra
  for host in ${@:-desktop server}; do
    while IFS='|' read -r -u 3 label slots ctx kv extra; do
      start "$host" "$slots" "$ctx" "$kv" "$extra" || { echo "!! $host $label did not start" >&2; continue; }
      mem "$host" "$label" memory.jsonl idle
      bench memload.jsonl "$host $label" "host=$host,config=$label" -url "$(url_of "$host")" \
        -agents 4 -turns 3 -system-chars 16000 -unique-system -seed 77
      mem "$host" "$label" memory.jsonl loaded
    done 3<<'CONFIGS'
4x8k q8_0|4|32768|q8_0|
2x32k q8_0|2|65536|q8_0|
4x32k q8_0|4|131072|q8_0|
2x32k f16|2|65536|f16|
2x32k q4_0|2|65536|q4_0|
2x32k swa-full|2|65536|q8_0|--swa-full
2x32k cache-ram 0|2|65536|q8_0|--cache-ram 0
2x32k cache-ram 32G|2|65536|q8_0|--cache-ram 32768
CONFIGS
  done
}

# Compaction on the desktop: none vs summary-after-system (prefix kept) vs
# summary-in-system (prefix rewritten), with planted facts quizzed at the end.
compaction() {
  local u="$DESKTOP_URL"
  start desktop 2 65536 q8_0 ""
  local common=(-url "$u" -agents 2 -turns 12 -system-chars 16000 -tool-chars 4000 -facts -compact-tokens 512)
  bench compaction.jsonl "no compaction" "style=none" "${common[@]}" -seed 501
  bench compaction.jsonl "append @8k" "style=append,at=8000" "${common[@]}" -seed 501 -compact-at 8000 -compact-style append
  bench compaction.jsonl "rewrite @8k" "style=rewrite,at=8000" "${common[@]}" -seed 501 -compact-at 8000 -compact-style rewrite
  bench compaction.jsonl "append @12k" "style=append,at=12000" "${common[@]}" -seed 501 -compact-at 12000 -compact-style append
}

# Small MoE models vs the dense baseline. name|file|desktop extra|server extra|mac?
MODEL_SET='gemma4-12b|gemma-4-12b-it-qat-q4_0.gguf|||yes
lfm2.5-8b-a1b|LFM2.5-8B-A1B-Q4_K_M.gguf|||yes
gpt-oss-20b|gpt-oss-20b-MXFP4.gguf||--n-cpu-moe 6|yes
qwen3.6-35b-a3b|Qwen3.6-35B-A3B-UD-Q4_K_M.gguf|--n-cpu-moe 16|--n-cpu-moe 24|no'

models() {
  local name file dx sx macok n hosts=" ${*:-desktop server mac} "
  on() { [[ "$hosts" == *" $1 "* ]]; }
  on desktop && stop desktop
  on server && stop server
  # Kernel speed per model and host; for models that overflow VRAM, sweep how
  # many layers' experts live in system RAM (-ncmoe).
  while IFS='|' read -r -u 3 name file dx sx macok; do
    local dm="$DESKTOP_MODELS\\$file" sm="~/models/$file" mm
    mm="$(dirname "$MAC_MODEL")/$file"
    case "$name" in
      qwen3.6-35b-a3b)
        on desktop && for n in 12 16 20 28 40; do DESKTOP_MODEL="$dm" REPS=1 bench/micro.sh desktop rocm "moe-$name" -fa 1 -p 512,4096 -n 128 -ncmoe $n || true; done
        on server && for n in 20 24 28 40; do SERVER_MODEL="$sm" REPS=1 bench/micro.sh server rocm "moe-$name" -fa 1 -p 512,4096 -n 128 -ncmoe $n || true; done ;;
      gpt-oss-20b)
        on desktop && { DESKTOP_MODEL="$dm" bench/micro.sh desktop rocm "moe-$name" -fa 1 -p 512,4096 -n 128 || true; }
        on server && for n in 0 4 8 12; do SERVER_MODEL="$sm" REPS=1 bench/micro.sh server rocm "moe-$name" -fa 1 -p 512,4096 -n 128 -ncmoe $n || true; done ;;
      *)
        on desktop && { DESKTOP_MODEL="$dm" bench/micro.sh desktop rocm "moe-$name" -fa 1 -p 512,4096 -n 128 || true; }
        on server && { SERVER_MODEL="$sm" bench/micro.sh server rocm "moe-$name" -fa 1 -p 512,4096 -n 128 || true; } ;;
    esac
    if on mac && [[ "$macok" == yes ]]; then MAC_MODEL="$mm" bench/micro.sh mac metal "moe-$name" -fa 1 -p 512,4096 -n 128 || true; fi
  done 3<<<"$MODEL_SET"

  # Serving + recall per model on the desktop (2x32k).
  on desktop || return 0
  while IFS='|' read -r -u 3 name file dx sx macok; do
    MODEL_ALIAS="$name" DESKTOP_MODEL="$DESKTOP_MODELS\\$file"
    start desktop 2 65536 q8_0 "$dx" || { echo "!! $name did not start" >&2; continue; }
    mem desktop "$name" memory-models.jsonl loaded
    for a in 1 4; do
      bench models.jsonl "$name agents=$a" "model=$name,agents=$a" -url "$DESKTOP_URL" -agents "$a" -turns 4 -system-chars 8000 -seed 601
    done
    bench needle.jsonl "$name needle" "model=$name" -url "$DESKTOP_URL" -needle 4000,8000,16000,28000 -needle-depths 0,25,50,75,100 -seed 602
    bench recall.jsonl "$name full history" "model=$name,style=none" -url "$DESKTOP_URL" -agents 2 -turns 10 -system-chars 8000 -tool-chars 4000 -facts -seed 603
    bench recall.jsonl "$name compacted" "model=$name,style=append" -url "$DESKTOP_URL" -agents 2 -turns 10 -system-chars 8000 -tool-chars 4000 -facts -compact-at 9000 -seed 603
  done 3<<<"$MODEL_SET"
  unset MODEL_ALIAS DESKTOP_MODEL; source bench/lab.env # back to the default model
  start desktop 2 65536 q8_0 ""
}

cmd="${1:?usage: $0 start|stop|micro|capacity|serve|route|mac|memory|compaction|models}"
shift
"$cmd" "$@"
