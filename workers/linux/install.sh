#!/usr/bin/env bash
# rdna-gateway worker for Linux + RDNA2 (gfx1030/1031/1032).
#
#   ./install.sh                         # install llama.cpp ROCm gfx103X build
#   MODEL=/path/model.gguf ALIAS=gemma4:12b ./install.sh service
#   ./install.sh ollama-blob gemma4:12b-it-qat   # print the GGUF path of an Ollama model
#   NAME=embed MODEL=... ALIAS=... PORT=8083 HOST=100.x.y.z ARGS="--embedding --pooling last" ./install.sh aux
#
# Runs llama-server under a systemd user unit (llama-worker.service). "aux"
# adds a small model next to it on the same GPU (llama-NAME.service), e.g. an
# embedder or reranker for retrieval; they fit in the VRAM the worker leaves.
set -euo pipefail

ROOT="${ROOT:-$HOME/llama}"
PORT="${PORT:-8081}"
PARALLEL="${PARALLEL:-4}"
CTX="${CTX:-131072}" # 4 slots x 32k
KV="${KV:-q8_0}"
CACHE_REUSE="${CACHE_REUSE:-256}"
EXTRA="${EXTRA:-}" # extra llama-server flags, e.g. "--cache-ram 16384 --swa-full"

ollama_blob() {
  local name="${1%%:*}" tag="${1#*:}"
  [[ "$tag" == "$1" ]] && tag=latest
  local models="${OLLAMA_MODELS:-$HOME/.ollama/models}"
  [[ -d "$models" ]] || models=/usr/share/ollama/.ollama/models
  local digest
  digest=$(python3 -c 'import json,sys; m=json.load(open(sys.argv[1])); print(next(l["digest"] for l in m["layers"] if l["mediaType"]=="application/vnd.ollama.image.model"))' \
    "$models/manifests/registry.ollama.ai/library/$name/$tag")
  echo "$models/blobs/${digest/:/-}"
}

install_build() {
  mkdir -p "$ROOT"
  local url
  url=$(curl -fsSL https://api.github.com/repos/lemonade-sdk/llamacpp-rocm/releases/latest |
    python3 -c 'import json,sys; r=json.load(sys.stdin); print(next(a["browser_download_url"] for a in r["assets"] if "ubuntu-rocm-gfx103X" in a["name"]))')
  echo "downloading $url"
  curl -fL --progress-bar "$url" -o "$ROOT/rocm.zip"
  rm -rf "$ROOT/rocm" && mkdir -p "$ROOT/rocm"
  (cd "$ROOT/rocm" && unzip -q ../rocm.zip) && rm "$ROOT/rocm.zip"
  chmod +x "$ROOT"/rocm/llama-* || true
  # The build bundles its ROCm runtime; the user just needs /dev/kfd access.
  if ! id -nG | grep -qw render; then
    echo "note: add yourself to the render+video groups: sudo usermod -aG render,video $USER (then re-login)"
  fi
  LD_LIBRARY_PATH="$ROOT/rocm" "$ROOT/rocm/llama-server" --list-devices
}

install_service() {
  : "${MODEL:?set MODEL=/path/to/model.gguf}"
  local alias="${ALIAS:-$(basename "$MODEL" .gguf)}"
  # The gfx103X build ships gfx1031/1032 kernels, so no HSA_OVERRIDE_GFX_VERSION
  # by default — it would also re-target a Ryzen iGPU. Set HSA_OVERRIDE=10.3.0
  # only for a card the build doesn't list. DEVICE pins the discrete GPU
  # (see `llama-server --list-devices`); iGPUs show up as extra ROCm devices.
  local device="${DEVICE:-ROCm0}" override=""
  [[ -n "${HSA_OVERRIDE:-}" ]] && override="Environment=HSA_OVERRIDE_GFX_VERSION=$HSA_OVERRIDE"
  mkdir -p "$HOME/.config/systemd/user"
  cat >"$HOME/.config/systemd/user/llama-worker.service" <<EOF
[Unit]
Description=llama.cpp worker (rdna-gateway)
After=network-online.target

[Service]
$override
Environment=LD_LIBRARY_PATH=$ROOT/rocm
ExecStart=$ROOT/rocm/llama-server -m $MODEL --alias $alias --device $device -ngl 999 -fa on -c $CTX --parallel $PARALLEL -ctk $KV -ctv $KV --cache-reuse $CACHE_REUSE --metrics --slots --host 0.0.0.0 --port $PORT $EXTRA
Restart=on-failure
RestartSec=5

[Install]
WantedBy=default.target
EOF
  systemctl --user daemon-reload
  systemctl --user enable llama-worker.service
  systemctl --user restart llama-worker.service
  loginctl enable-linger "$USER" 2>/dev/null || true # keep running after logout
  sleep 2
  for _ in $(seq 90); do curl -fs "http://127.0.0.1:$PORT/health" && echo && return; sleep 2; done
  echo "worker did not come up; journalctl --user -u llama-worker" >&2
  return 1
}

install_aux() {
  : "${NAME:?set NAME, e.g. embed}" "${MODEL:?set MODEL=/path/to/model.gguf}" "${PORT:?set PORT}"
  local alias="${ALIAS:-$(basename "$MODEL" .gguf)}" unit="llama-$NAME.service"
  mkdir -p "$HOME/.config/systemd/user"
  cat >"$HOME/.config/systemd/user/$unit" <<EOF
[Unit]
Description=llama.cpp $NAME (rdna-gateway)
After=network-online.target llama-worker.service

[Service]
Environment=LD_LIBRARY_PATH=$ROOT/rocm
ExecStart=$ROOT/rocm/llama-server -m $MODEL --alias $alias --device ${DEVICE:-ROCm0} -ngl 999 -c ${CTX_AUX:-4096} -b ${UB_AUX:-1024} -ub ${UB_AUX:-1024} --parallel ${SLOTS_AUX:-2} --metrics --host ${HOST:-127.0.0.1} --port $PORT ${ARGS:-}
# HOST may be a Tailscale address that isn't up yet at boot; keep retrying.
Restart=always
RestartSec=5

[Install]
WantedBy=default.target
EOF
  systemctl --user daemon-reload
  systemctl --user enable "$unit"
  systemctl --user restart "$unit"
  for _ in $(seq 60); do curl -fs "http://${HOST:-127.0.0.1}:$PORT/health" && echo && return; sleep 2; done
  echo "$NAME did not come up; journalctl --user -u $unit" >&2
  return 1
}

case "${1:-build}" in
  build) install_build ;;
  service) install_service ;;
  aux) install_aux ;;
  ollama-blob) ollama_blob "$2" ;;
  *) echo "usage: $0 [build|service|aux|ollama-blob NAME]" >&2; exit 2 ;;
esac
