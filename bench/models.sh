#!/usr/bin/env bash
# Fetch the benchmark models onto a host (from Hugging Face, ungated repos) and
# verify their sha256 against the published LFS hashes.
#
#   bench/models.sh desktop|server|mac [model...]
set -euo pipefail
cd "$(dirname "$0")/.."
source bench/lab.env

# name|repo|file|sha256
MODELS=(
  "gemma4-12b|google/gemma-4-12B-it-qat-q4_0-gguf|gemma-4-12b-it-qat-q4_0.gguf|93567e57a8fe10b23569b9d9ec38cd005deedf71e29477c421a4b83f418a538b"
  "lfm2.5-8b-a1b|LiquidAI/LFM2.5-8B-A1B-GGUF|LFM2.5-8B-A1B-Q4_K_M.gguf|"
  "lfm2-8b-a1b|LiquidAI/LFM2-8B-A1B-GGUF|LFM2-8B-A1B-Q4_K_M.gguf|d2185b22630fc68043dac7182f12e86e5ad14990229a90b6c9ad3f4421ddaf82"
  "qwen3-embed-0.6b|Qwen/Qwen3-Embedding-0.6B-GGUF|Qwen3-Embedding-0.6B-Q8_0.gguf|"
  "embeddinggemma-300m|ggml-org/embeddinggemma-300M-GGUF|embeddinggemma-300M-Q8_0.gguf|"
  "nomic-embed-1.5|nomic-ai/nomic-embed-text-v1.5-GGUF|nomic-embed-text-v1.5.Q8_0.gguf|"
  "bge-reranker-v2-m3|gpustack/bge-reranker-v2-m3-GGUF|bge-reranker-v2-m3-Q8_0.gguf|"
  "qwen3-reranker-0.6b|ggml-org/Qwen3-Reranker-0.6B-Q8_0-GGUF|qwen3-reranker-0.6b-q8_0.gguf|"
  "gpt-oss-20b|ggml-org/gpt-oss-20b-GGUF|gpt-oss-20b-MXFP4.gguf|"
  "qwen3.6-35b-a3b|unsloth/Qwen3.6-35B-A3B-GGUF|Qwen3.6-35B-A3B-UD-Q4_K_M.gguf|"
)

host="$1"; shift
want=("$@")

for m in "${MODELS[@]}"; do
  IFS='|' read -r name repo file sha <<<"$m"
  if [[ ${#want[@]} -gt 0 && ! " ${want[*]} " =~ " $name " ]]; then continue; fi
  if [[ -z "$sha" ]]; then
    sha=$(curl -fsSL "https://huggingface.co/api/models/$repo?blobs=true" | python3 -c '
import json, sys
f = sys.argv[1]
print(next(s["lfs"]["sha256"] for s in json.load(sys.stdin)["siblings"] if s["rfilename"] == f))' "$file")
  fi
  url="https://huggingface.co/$repo/resolve/main/$file"
  echo ">> $host: $name ($file)"
  case "$host" in
    desktop)
      ssh -o BatchMode=yes "$DESKTOP_SSH" "powershell -NoProfile -Command \"\$ProgressPreference='SilentlyContinue'; \$f='$DESKTOP_MODELS\\$file'; if (-not (Test-Path \$f) -or (Get-FileHash \$f).Hash -ne '$sha'.ToUpper()) { Invoke-WebRequest '$url' -OutFile \$f }; if ((Get-FileHash \$f).Hash -eq '$sha'.ToUpper()) { 'ok $file' } else { 'HASH MISMATCH $file' }\"" ;;
    server)
      ssh -o BatchMode=yes "$SERVER_SSH" "cd ~/models && { echo '$sha  $file' | sha256sum -c --quiet 2>/dev/null || curl -fsSL -o '$file' '$url'; } && echo '$sha  $file' | sha256sum -c" ;;
    mac)
      dir=$(dirname "$MAC_MODEL")
      (cd "$dir" && { echo "$sha  $file" | shasum -a 256 -c --quiet 2>/dev/null || curl -fsSL -o "$file" "$url"; } && echo "$sha  $file" | shasum -a 256 -c) ;;
  esac
done
