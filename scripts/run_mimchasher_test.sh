#!/usr/bin/env bash
set -euo pipefail

# cd to repo root
SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" >/dev/null 2>&1 && pwd)"
cd "$SCRIPT_DIR/.."

export EON_PROFILE=1

TS=$(date +%Y%m%d_%H%M%S)
LOG_DIR="logs"
mkdir -p "$LOG_DIR"
BASE="$LOG_DIR/mimchasher_icicle_${TS}"
LOG_FILE="${BASE}.log"
NCU_REP="${BASE}.ncu-rep"
CSV_FILE="${BASE}_SpeedOfLight.csv"

echo "[run] nsys profile -> go run -tags icicle ./examples/mimchasher/main.go -count=1 -v"
STATS_TXT="${BASE}_gpukernsum.txt"
echo "[out] log=$LOG_FILE  rep=${BASE}.(qdrep|nsys-rep)  stats=$STATS_TXT"

# Build once to avoid go run child process; then profile the binary
BIN="bin/mimchasher"
mkdir -p bin
echo "[build] go build -tags icicle -o $BIN ./examples/mimchasher/main.go"
go build -tags icicle -o "$BIN" ./examples/mimchasher/main.go

# Collect timeline and kernel list via Nsight Systems (direct binary run)
nsys profile --trace=cuda,nvtx,osrt --force-overwrite true --show-output true \
  -o "$BASE" -- \
  "$BIN" -count=1 -v 2>&1 | tee "$LOG_FILE"

# Export kernel summary (top kernels, durations, counts)
# Detect report extension produced by current nsys version (.qdrep or .nsys-rep)
REP=""
if [ -f "${BASE}.qdrep" ]; then
  REP="${BASE}.qdrep"
elif [ -f "${BASE}.nsys-rep" ]; then
  REP="${BASE}.nsys-rep"
else
  echo "[error] Nsight Systems report not found: ${BASE}.qdrep or ${BASE}.nsys-rep" >&2
  exit 1
fi

nsys stats --report gpukernsum "$REP" | tee "$STATS_TXT"

echo "[done] See: $LOG_FILE | $REP | $STATS_TXT"


