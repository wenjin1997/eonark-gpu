#!/usr/bin/env bash
set -euo pipefail

# Compare performance of mimchasher with and without the `icicle` build tag.
# It runs both commands:
#   go run -tags icicle ./examples/mimchasher/main.go -count=1 -v
#   go run ./examples/mimchasher/main.go -count=1 -v
# and saves outputs and timing info into the benchmarks/ directory.

# Ensure we operate from repo root (directory of this script)
cd "$(dirname "$0")"

OUT_DIR="benchmarks"
mkdir -p "$OUT_DIR"

TIMESTAMP="$(date +"%Y%m%d_%H%M%S")"

# Common args to the program
PROGRAM_ARGS=("-count=1" "-v")

# Verify /usr/bin/time exists; fallback to plain time (less detailed)
TIME_BIN="/usr/bin/time"
TIME_FMT="elapsed_sec=%e\nuser_sec=%U\nsys_sec=%S\nmax_kb=%M"
if [[ ! -x "$TIME_BIN" ]]; then
	TIME_BIN="time"
	# POSIX time has no -f; we will just append plain timing to logs
	TIME_FMT=""
fi

echo "Running mimchasher with -tags icicle ..."
ICICLE_LOG="${OUT_DIR}/mimchasher_icicle_${TIMESTAMP}.log"
ICICLE_TIME="${OUT_DIR}/mimchasher_icicle_${TIMESTAMP}.time"
if [[ -n "$TIME_FMT" && "$TIME_BIN" == "/usr/bin/time" ]]; then
	$TIME_BIN -f "$TIME_FMT" -o "$ICICLE_TIME" \
		bash -c "go run -tags icicle ./examples/mimchasher/main.go ${PROGRAM_ARGS[*]}" \
		2>&1 | tee "$ICICLE_LOG"
else
	# Fallback timing appended to log
	({ time bash -c "go run -tags icicle ./examples/mimchasher/main.go ${PROGRAM_ARGS[*]}"; } 2>&1) | tee "$ICICLE_LOG"
	grep -E "^real|^user|^sys" "$ICICLE_LOG" || true > "$ICICLE_TIME"
fi

echo "Running mimchasher without build tags ..."
DEFAULT_LOG="${OUT_DIR}/mimchasher_default_${TIMESTAMP}.log"
DEFAULT_TIME="${OUT_DIR}/mimchasher_default_${TIMESTAMP}.time"
if [[ -n "$TIME_FMT" && "$TIME_BIN" == "/usr/bin/time" ]]; then
	$TIME_BIN -f "$TIME_FMT" -o "$DEFAULT_TIME" \
		bash -c "go run ./examples/mimchasher/main.go ${PROGRAM_ARGS[*]}" \
		2>&1 | tee "$DEFAULT_LOG"
else
	({ time bash -c "go run ./examples/mimchasher/main.go ${PROGRAM_ARGS[*]}"; } 2>&1) | tee "$DEFAULT_LOG"
	grep -E "^real|^user|^sys" "$DEFAULT_LOG" || true > "$DEFAULT_TIME"
fi

echo
echo "Summary of timing (lower is better):"
if [[ -f "$ICICLE_TIME" ]]; then
	echo "icicle:" && cat "$ICICLE_TIME"
else
	echo "icicle: (timing not available)"
fi
echo
if [[ -f "$DEFAULT_TIME" ]]; then
	echo "default:" && cat "$DEFAULT_TIME"
else
	echo "default: (timing not available)"
fi

echo
echo "Logs saved to:"
echo " - $ICICLE_LOG"
echo " - $DEFAULT_LOG"
echo "Timing saved to:"
echo " - $ICICLE_TIME"
echo " - $DEFAULT_TIME"


