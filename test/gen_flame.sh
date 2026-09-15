#!/usr/bin/env bash
# gen_flamegraph.sh — record a perf CPU profile and turn it into a flamegraph SVG.
#
# Usage:
#   ./gen_flamegraph.sh [--flamegraph-dir DIR] [--freq HZ] [--out FILE] -- CMD [ARGS...]
#
# Example (walkman):
#   ./gen_flamegraph.sh --flamegraph-dir ./FlameGraph --freq 999 --out walkman_flame.svg \
#       -- ../build/walkman --quiet ../../../foss/linux-7.2.2
#
# Requires: perf, and a clone of https://github.com/brendangregg/FlameGraph
# (stackcollapse-perf.pl + flamegraph.pl must be in --flamegraph-dir).

set -euo pipefail

FLAMEGRAPH_DIR="${FLAMEGRAPH_DIR:-$HOME/FlameGraph}"
FREQ=999
OUT="flamegraph.svg"
TITLE=""

while [[ $# -gt 0 ]]; do
  case "$1" in
    --flamegraph-dir) FLAMEGRAPH_DIR="$2"; shift 2 ;;
    --freq)           FREQ="$2"; shift 2 ;;
    --out)            OUT="$2"; shift 2 ;;
    --title)          TITLE="$2"; shift 2 ;;
    --) shift; break ;;
    -h|--help) grep '^#' "$0" | sed 's/^#//'; exit 0 ;;
    *) echo "unknown arg: $1 (did you forget --?)" >&2; exit 1 ;;
  esac
done

if [[ $# -eq 0 ]]; then
  echo "error: no command given to profile. Usage: $0 [opts] -- CMD [ARGS...]" >&2
  exit 1
fi

# --- sanity checks -----------------------------------------------------

if ! command -v perf >/dev/null 2>&1; then
  echo "error: perf not found. Install it (e.g. 'sudo pacman -S perf' on Artix)." >&2
  exit 1
fi

STACKCOLLAPSE="$FLAMEGRAPH_DIR/stackcollapse-perf.pl"
FLAMEGRAPH="$FLAMEGRAPH_DIR/flamegraph.pl"
for f in "$STACKCOLLAPSE" "$FLAMEGRAPH"; do
  if [[ ! -x "$f" && ! -f "$f" ]]; then
    echo "error: expected script not found: $f" >&2
    echo "  (pass the right path with --flamegraph-dir, or clone https://github.com/brendangregg/FlameGraph)" >&2
    exit 1
  fi
done

# perf_event_paranoid check — same fix your perf_bench.sh already knows about.
PARANOID="$(cat /proc/sys/kernel/perf_event_paranoid 2>/dev/null || echo unknown)"
PERF_PREFIX=()
if [[ "$PARANOID" != "unknown" && "$PARANOID" -gt 1 && "$EUID" -ne 0 ]]; then
  if command -v sudo >/dev/null 2>&1; then
    echo "note: perf_event_paranoid=$PARANOID, need root — using sudo for perf record" >&2
    PERF_PREFIX=(sudo)
  else
    echo "error: perf_event_paranoid=$PARANOID and no sudo available." >&2
    echo "  Fix with: sudo sysctl kernel.perf_event_paranoid=-1" >&2
    echo "  or:       sudo setcap cap_perfmon=ep \$(command -v perf)" >&2
    exit 1
  fi
fi

# --- pipeline ------------------------------------------------------------

WORKDIR="$(mktemp -d)"
trap 'rm -rf "$WORKDIR"' EXIT

PERF_DATA="$WORKDIR/perf.data"
PERF_SCRIPT_OUT="$WORKDIR/out.perf"
FOLDED="$WORKDIR/out.folded"

echo "==> recording (freq=${FREQ}Hz): $*" >&2
"${PERF_PREFIX[@]}" perf record -F "$FREQ" -g --call-graph fp -o "$PERF_DATA" -- "$@"

# perf.data may be root-owned if we used sudo; make sure we can read it back.
if [[ ${#PERF_PREFIX[@]} -gt 0 ]]; then
  sudo chmod a+r "$PERF_DATA"
fi

echo "==> perf script" >&2
perf script -i "$PERF_DATA" > "$PERF_SCRIPT_OUT"

echo "==> collapsing stacks" >&2
"$STACKCOLLAPSE" "$PERF_SCRIPT_OUT" > "$FOLDED"

if [[ ! -s "$FOLDED" ]]; then
  echo "error: no stacks collapsed — the profiled command probably ran too briefly to get samples." >&2
  exit 1
fi

echo "==> rendering SVG -> $OUT" >&2
FLAME_ARGS=(--width 1600)
[[ -n "$TITLE" ]] && FLAME_ARGS+=(--title "$TITLE")
"$FLAMEGRAPH" "${FLAME_ARGS[@]}" "$FOLDED" > "$OUT"

echo "done: $OUT" >&2
