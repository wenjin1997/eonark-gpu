#!/usr/bin/env bash
set -euo pipefail

# Compare performance of mimchasher with and without the `icicle` build tag.
# It runs both commands:
#   go run -tags icicle ./examples/mimchasher/main.go -count=1 -v
#   go run ./examples/mimchasher/main.go -count=1 -v
# and saves outputs and timing info into the benchmarks/ directory.

# Ensure we operate from repo root
SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" >/dev/null 2>&1 && pwd)"
cd "$SCRIPT_DIR/.."

# Select output directory based on GPU model
GPU_NAME="$(nvidia-smi --query-gpu=name --format=csv,noheader 2>/dev/null | head -n 1 || echo "")"
if [[ "$GPU_NAME" == *"5070"* ]]; then
  OUT_DIR="logs/msm-fft-gpu-compare/gpu-5070"
elif [[ "$GPU_NAME" == *"4090"* ]]; then
  OUT_DIR="logs/msm-fft-gpu-compare/gpu-4090"
else
  OUT_DIR="logs/msm-fft-gpu-compare/others"
fi
mkdir -p "$OUT_DIR"

TIMESTAMP="$(date +"%Y%m%d_%H%M%S")"

# Common args to the program
PROGRAM_ARGS=("-count=1" "-v")

echo "Running mimchasher with -tags icicle ..."
ICICLE_LOG="${OUT_DIR}/mimchasher_icicle_${TIMESTAMP}.log"
go run -tags icicle ./examples/mimchasher/main.go "${PROGRAM_ARGS[@]}" 2>&1 | tee "$ICICLE_LOG"

echo "Running mimchasher without build tags ..."
DEFAULT_LOG="${OUT_DIR}/mimchasher_default_${TIMESTAMP}.log"
go run ./examples/mimchasher/main.go "${PROGRAM_ARGS[@]}" 2>&1 | tee "$DEFAULT_LOG"

echo "Logs saved to:"
echo " - $ICICLE_LOG"
echo " - $DEFAULT_LOG"
