#!/usr/bin/env bash
# run_all.sh — runs both benchmark harnesses (hyperfine-based
# bench_harness.sh and perf-based perf_bench.sh) against a real,
# user-supplied filesystem tree, no symlinks followed.
#
# Output layout (in --out-dir, default ./bench_results/):
#   hyperfine.csv
#   perf.csv
#
# A run_manifest.txt is written recording exactly which tree (path +
# actual dir/file/symlink counts at run time), which binaries, and which
# workers list produced each file — so six months from now you can tell
# which run is which without guessing from file mtimes, and can tell if
# the tree itself changed (e.g. a git pull) between two runs.
#
# Usage (run from walkman/walkman/test/, same directory bench_harness.sh
# and perf_bench.sh already live in):
#   ./run_all.sh \
#       --walkman          ../build/walkman \
#       --ignore-parallel  ../build/ignore-parallel-cli \
#       --tree             /path/to/linux-7.2.2 \
#       --workers          "1,2,4,$(nproc)" \
#       --runs             10 \
#       --out-dir          bench_results
#
# Either of --skip-hyperfine / --skip-perf can narrow what runs.
#
# Pass --quick for a fast iteration pass (fewer workers, fewer runs) —
# see the QUICK block below for exactly what it changes and why. Full
# default sweep is still the more rigorous option; --quick trades some
# precision for speed intentionally.

set -euo pipefail

WALKMAN_BIN=""
IGNORE_PARALLEL_BIN=""
TREE=""
WORKERS="1,2,4,$(nproc 2>/dev/null || echo 4)"
RUNS=10
WARMUP=5
OUT_DIR="bench_results"
SKIP_HYPERFINE=0
SKIP_PERF=0
QUICK=0

while [[ $# -gt 0 ]]; do
  case "$1" in
    --walkman) WALKMAN_BIN="$2"; shift 2 ;;
    --ignore-parallel) IGNORE_PARALLEL_BIN="$2"; shift 2 ;;
    --tree)    TREE="$2"; shift 2 ;;
    --workers) WORKERS="$2"; shift 2 ;;
    --runs)    RUNS="$2"; shift 2 ;;
    --warmup)  WARMUP="$2"; shift 2 ;;
    --out-dir) OUT_DIR="$2"; shift 2 ;;
    --skip-hyperfine) SKIP_HYPERFINE=1; shift 1 ;;
    --skip-perf)      SKIP_PERF=1; shift 1 ;;
    --quick)   QUICK=1; shift 1 ;;
    -h|--help) grep '^#' "$0" | sed 's/^#//'; exit 0 ;;
    *) echo "unknown arg: $1" >&2; exit 1 ;;
  esac
done

# --quick: sane fast-iteration defaults, only applied to values the user
# didn't already override above (an explicit --workers/--runs/--warmup
# always wins). Keeps 1 worker (the serial baseline is genuinely useful —
# it's the only point that shows raw single-thread cost, everything else
# is a scaling claim relative to it) but drops the sweep to two points
# (1 and nproc) instead of four, and lowers run/warmup counts.
if [[ "$QUICK" -eq 1 ]]; then
  [[ "$WORKERS" == "1,2,4,$(nproc 2>/dev/null || echo 4)" ]] && WORKERS="1,$(nproc 2>/dev/null || echo 4)"
  [[ "$RUNS" == 10 ]] && RUNS=6
  [[ "$WARMUP" == 5 ]] && WARMUP=2
fi

if [[ -z "$TREE" ]]; then
  echo "error: --tree $TREE is required" >&2
  exit 1
fi

if [[ -z "$WALKMAN_BIN" && -z "$IGNORE_PARALLEL_BIN" ]]; then
  echo "missing at least one binary: specify --walkman or --ignore-parallel (see --help)" >&2
  exit 1
fi

if [[ ! -d "$TREE" ]]; then
  echo "error: --tree $TREE is not a directory" >&2
  exit 1
fi

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
BENCH_HARNESS="$SCRIPT_DIR/bench_harness.sh"
PERF_BENCH="$SCRIPT_DIR/perf_bench.sh"

for req_script in BENCH_HARNESS PERF_BENCH; do
  if [[ ! -f "${!req_script}" ]]; then
    echo "error: expected $req_script at ${!req_script} but it's not there." >&2
    echo "  run this from the same directory bench_harness.sh / perf_bench.sh live in." >&2
    exit 1
  fi
done

if [[ $SKIP_HYPERFINE -eq 0 ]] && ! command -v hyperfine >/dev/null 2>&1; then
  echo "error: hyperfine not found (needed for bench_harness.sh). Install it, or pass --skip-hyperfine." >&2
  exit 1
fi
if [[ $SKIP_PERF -eq 0 ]] && ! command -v perf >/dev/null 2>&1; then
  echo "error: perf not found (needed for perf_bench.sh). Install linux-tools, or pass --skip-perf." >&2
  exit 1
fi

mkdir -p "$OUT_DIR"
MANIFEST="$OUT_DIR/run_manifest.txt"

# Snapshot the tree's actual shape at run time. Unlike the old generated
# trees, a real tree can change out from under you (a `git pull` between
# two runs, a stray build artifact left in the checkout) — recording the
# counts here means a later comparison can at least tell you if the tree
# wasn't the same one.
echo "counting tree contents (this can take a moment on a large tree)..." >&2
TREE_DIRS=$(find "$TREE" -mindepth 1 -type d | wc -l)
TREE_FILES=$(find "$TREE" -type f | wc -l)
TREE_LINKS=$(find "$TREE" -type l | wc -l)

{
  echo "run_all.sh — $(date -u '+%Y-%m-%d %H:%M:%S UTC')"
  echo "host: $(uname -srmo 2>/dev/null || uname -a)"
  echo "cpu:  $(grep -m1 'model name' /proc/cpuinfo 2>/dev/null | cut -d: -f2 | sed 's/^ *//' || echo unknown)"
  [[ -n "$WALKMAN_BIN" ]] && echo "walkman:          $(realpath -- "$WALKMAN_BIN")"
  [[ -n "$IGNORE_PARALLEL_BIN" ]] && echo "ignore-parallel:  $(realpath -- "$IGNORE_PARALLEL_BIN")"
  echo "tree:             $(realpath -- "$TREE")"
  echo "tree dirs=$TREE_DIRS files=$TREE_FILES symlinks=$TREE_LINKS"
  echo "workers:  $WORKERS"
  echo "runs:     $RUNS   warmup: $WARMUP"
  echo ""
} > "$MANIFEST"

FAILURES=()

common_args=(--tree "$TREE" --workers "$WORKERS")
[[ -n "$WALKMAN_BIN" ]] && common_args+=(--walkman "$WALKMAN_BIN")
[[ -n "$IGNORE_PARALLEL_BIN" ]] && common_args+=(--ignore-parallel "$IGNORE_PARALLEL_BIN")

# Each harness call is allowed to fail without taking down the other —
# bench_harness.sh in particular can write a complete, valid CSV and THEN
# fail on its own cosmetic summary-table step (e.g. if `column` isn't
# installed) — that's not a reason to lose the other run's output.
# Failures are collected and reported clearly at the end instead of
# silently swallowed.
if [[ $SKIP_HYPERFINE -eq 0 ]]; then
  hf_out="$OUT_DIR/hyperfine.csv"
  echo "" >&2
  echo "--- hyperfine -> $hf_out ---" >&2
  if "$BENCH_HARNESS" "${common_args[@]}" --min-runs "$RUNS" --warmup "$WARMUP" --out "$hf_out"; then
    echo "hyperfine.csv -> $hf_out ($(wc -l < "$hf_out") lines)" >> "$MANIFEST"
  else
    status=$?
    if [[ -s "$hf_out" ]]; then
      echo "warning: bench_harness.sh exited $status, but $hf_out was written and looks non-empty — likely failed on its own summary step, data is probably fine. Check manually." >&2
      echo "hyperfine.csv -> $hf_out (harness exited $status, but CSV present — VERIFY)" >> "$MANIFEST"
    else
      echo "error: bench_harness.sh exited $status and $hf_out is missing/empty. Skipping." >&2
      echo "hyperfine.csv -> FAILED (exit $status, no usable CSV)" >> "$MANIFEST"
      FAILURES+=("hyperfine")
    fi
  fi
fi

if [[ $SKIP_PERF -eq 0 ]]; then
  perf_out="$OUT_DIR/perf.csv"
  echo "" >&2
  echo "--- perf -> $perf_out ---" >&2
  if "$PERF_BENCH" "${common_args[@]}" --runs "$RUNS" --warmup "$WARMUP" --out "$perf_out"; then
    echo "perf.csv -> $perf_out ($(wc -l < "$perf_out") lines)" >> "$MANIFEST"
  else
    status=$?
    if [[ -s "$perf_out" ]]; then
      echo "warning: perf_bench.sh exited $status, but $perf_out was written and looks non-empty. Check manually." >&2
      echo "perf.csv -> $perf_out (harness exited $status, but CSV present — VERIFY)" >> "$MANIFEST"
    else
      echo "error: perf_bench.sh exited $status and $perf_out is missing/empty. Skipping." >&2
      echo "perf.csv -> FAILED (exit $status, no usable CSV)" >> "$MANIFEST"
      FAILURES+=("perf")
    fi
  fi
fi

echo "" >&2
echo "all done. results in $OUT_DIR/:" >&2
ls -la "$OUT_DIR" >&2
echo "" >&2
echo "manifest: $MANIFEST" >&2

if [[ ${#FAILURES[@]} -gt 0 ]]; then
  echo "" >&2
  echo "WARNING: ${#FAILURES[@]} run(s) failed with no usable output: ${FAILURES[*]}" >&2
  echo "See $MANIFEST and the output above for details." >&2
  exit 1
fi
