#!/usr/bin/env bash
# Retrieval node on a Jetson (Orin Nano Super, JetPack 6.2): llama.cpp built for
# CUDA sm_87, the embedder and reranker as systemd user units, and node_exporter
# plus a temperature script for monitoring.
#
#   HOST=100.x.y.z ./setup.sh build      # llama.cpp b11723 with CUDA (~20 min)
#   HOST=100.x.y.z ./setup.sh services   # embed :8083, rerank :8084, node_exporter :9100
#
# Models go in MODELS (default /ssd/models, i.e. NVMe; the SD card is slow).
set -euo pipefail
cd "$(dirname "$0")"
: "${HOST:?set HOST to the Tailscale address of this box}"
LLAMA_TAG=${LLAMA_TAG:-b11723}
SRC=$HOME/llama/src MODELS=${MODELS:-/ssd/models} UNITS=$HOME/.config/systemd/user

unit() { # name description execstart
  mkdir -p "$UNITS"
  cat >"$UNITS/$1.service" <<EOF
[Unit]
Description=$2
After=network-online.target

[Service]
ExecStart=$3
# HOST may be a Tailscale address that isn't up yet at boot; keep retrying.
Restart=always
RestartSec=5

[Install]
WantedBy=default.target
EOF
}

case "${1:-}" in
build)
  mkdir -p "$HOME/llama"
  [[ -d $SRC ]] || { curl -fsSL "https://codeload.github.com/ggml-org/llama.cpp/tar.gz/refs/tags/$LLAMA_TAG" | tar xz -C "$HOME/llama" && mv "$HOME/llama/llama.cpp-$LLAMA_TAG" "$SRC"; }
  cd "$SRC"
  PATH=/usr/local/cuda/bin:$PATH cmake -B build -DGGML_CUDA=ON -DCMAKE_CUDA_ARCHITECTURES=87 -DLLAMA_CURL=OFF -DCMAKE_BUILD_TYPE=Release
  cmake --build build -j "$(nproc)" --target llama-server llama-bench ;;
services)
  bin=$SRC/build/bin/llama-server
  unit llama-embed "llama.cpp embedder" "$bin -m $MODELS/embeddinggemma-300M-Q8_0.gguf --alias embeddinggemma-300m --embedding -ngl 999 -c 4096 -b 1024 -ub 1024 --parallel 2 --metrics --host $HOST --port 8083"
  unit llama-rerank "llama.cpp reranker" "$bin -m $MODELS/bge-reranker-v2-m3-Q8_0.gguf --alias bge-reranker-v2-m3 --reranking -ngl 999 -c 8192 -b 2048 -ub 2048 --parallel 8 --metrics --host $HOST --port 8084"
  # node_exporter's thermal_zone collector hangs on the powered-off cv0-2 zones;
  # jetson-sensors.sh writes the readable ones for the textfile collector.
  install -m 755 jetson-sensors.sh "$HOME/bin/jetson-sensors.sh"
  unit jetson-sensors "Jetson sensors for node_exporter" "$HOME/bin/jetson-sensors.sh"
  [[ -x $HOME/bin/node_exporter ]] || echo "install the linux-arm64 node_exporter to ~/bin first" >&2
  unit node-exporter "Prometheus node_exporter" "$HOME/bin/node_exporter --web.listen-address=$HOST:9100 --no-collector.thermal_zone --collector.textfile.directory=$HOME/.node-textfile"
  systemctl --user daemon-reload
  systemctl --user enable --now llama-embed llama-rerank jetson-sensors node-exporter
  loginctl enable-linger "$USER" || true ;;
*) sed -n '2,10p' "$0"; exit 2 ;;
esac
