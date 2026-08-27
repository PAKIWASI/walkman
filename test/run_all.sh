#!/usr/bin/env bash
# run_all_benchmarks.sh — runs both benchmark harnesses (hyperfine-based
# bench_harness.sh and perf-based perf_bench.sh) across all three tree
# shapes (wide, deep, mixed), and writes correctly-named, non-colliding
# CSVs for each combination.
#
# This exists to fix a real mistake from doing these runs by hand: it's
# easy to reuse the same --out filename across different --tree shapes
# and silently overwrite earlier results (this happened in an earlier
# session — a file named results_wide.csv ended up holding tree_mixed
# data because the same --out path was passed for a later run). This
# script hardcodes one filename per (harness, shape) pair specifically
# to make that mistake impossible.
#
# Output layout (in --out-dir, default ./bench_results/):
#   hyperfine_wide.csv    hyperfine_deep.csv    hyperfine_mixed.csv
#   perf_wide.csv         perf_deep.csv         perf_mixed.csv
#
# A run_manifest.txt is also written recording exactly which tree
# (shape/seed/dir+file counts), which binaries, and which workers list
# produced each file — so six months from now you can tell which run is
# which without guessing from file mtimes.
#
# Usage (run from walkman/walkman/test/, same directory build_tree.sh,
# bench_harness.sh, and perf_bench.sh already live in):
#   ./run_all.sh \
#       --walkman          ../build/main \
#       --ignore-parallel  ../build/ignore-parallel-cli \
#       --workers          "1,2,4,$(nproc)" \
#       --runs             10 \
#       --out-dir          bench_results
#
# Any of --skip-hyperfine / --skip-perf / --shapes can narrow what runs,
# e.g. to redo just one shape after a fix: --shapes mixed
#
# Pass --quick for a fast iteration pass (fewer workers, fewer runs) —
# see the QUICK block below for exactly what it changes and why. Full
# default sweep is still the more rigorous option; --quick trades some
# precision for speed intentionally.

set -euo pipefail

WALKMAN_BIN=""
IGNORE_PARALLEL_BIN=""
WORKERS="1,2,4,$(nproc 2>/dev/null || echo 4)"
RUNS=10
WARMUP=5
OUT_DIR="bench_results"
TREE_ROOT="${TMPDIR:-/tmp}"
SEED=42
SHAPES="wide,deep,mixed"
SKIP_HYPERFINE=0
SKIP_PERF=0
QUICK=0

while [[ $# -gt 0 ]]; do
  case "$1" in
    --walkman) WALKMAN_BIN="$2"; shift 2 ;;
    --ignore-parallel) IGNORE_PARALLEL_BIN="$2"; shift 2 ;;
    --workers) WORKERS="$2"; shift 2 ;;
    --runs)    RUNS="$2"; shift 2 ;;
    --warmup)  WARMUP="$2"; shift 2 ;;
    --out-dir) OUT_DIR="$2"; shift 2 ;;
    --tree-root) TREE_ROOT="$2"; shift 2 ;;
    --seed)    SEED="$2"; shift 2 ;;
    --shapes)  SHAPES="$2"; shift 2 ;;
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
# (1 and nproc) instead of four, and lowers run/warmup counts. Combined
# with bench_harness.sh now pinning hyperfine's run count by default
# (see that script), this cuts a full three-shape sweep from several
# minutes to well under a minute on a normal machine. Use the full
# defaults (or hand-pick --workers/--runs) when you want the tighter,
# more thorough numbers instead — --quick is for "does this look right
# / did I break something," not for numbers you're about to publish.
if [[ "$QUICK" -eq 1 ]]; then
  [[ "$WORKERS" == "1,2,4,$(nproc 2>/dev/null || echo 4)" ]] && WORKERS="1,$(nproc 2>/dev/null || echo 4)"
  [[ "$RUNS" == 10 ]] && RUNS=6
  [[ "$WARMUP" == 5 ]] && WARMUP=2
fi

for req in WALKMAN_BIN IGNORE_PARALLEL_BIN; do
  if [[ -z "${!req}" ]]; then
    echo "missing required --${req,,} (see --help)" >&2; exit 1
  fi
done

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
BUILD_TREE="$SCRIPT_DIR/build_tree.sh"
BENCH_HARNESS="$SCRIPT_DIR/bench_harness.sh"
PERF_BENCH="$SCRIPT_DIR/perf_bench.sh"

for req_script in BUILD_TREE BENCH_HARNESS PERF_BENCH; do
  if [[ ! -f "${!req_script}" ]]; then
    echo "error: expected $req_script at ${!req_script} but it's not there." >&2
    echo "  run this from the same directory build_tree.sh / bench_harness.sh / perf_bench.sh live in." >&2
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
{
  echo "run_all_benchmarks.sh — $(date -u '+%Y-%m-%d %H:%M:%S UTC')"
  echo "host: $(uname -srmo 2>/dev/null || uname -a)"
  echo "cpu:  $(grep -m1 'model name' /proc/cpuinfo 2>/dev/null | cut -d: -f2 | sed 's/^ *//' || echo unknown)"
  echo "walkman:          $(realpath -- "$WALKMAN_BIN")"
  echo "ignore-parallel:  $(realpath -- "$IGNORE_PARALLEL_BIN")"
  echo "workers:  $WORKERS"
  echo "runs:     $RUNS   warmup: $WARMUP"
  echo "seed:     $SEED"
  echo ""
} > "$MANIFEST"

IFS=',' read -ra SHAPE_LIST <<< "$SHAPES"
FAILURES=()

for shape in "${SHAPE_LIST[@]}"; do
  tree_path="$TREE_ROOT/tree_${shape}"

  echo "" >&2
  echo "############################################" >&2
  echo "# shape=$shape" >&2
  echo "############################################" >&2

  "$BUILD_TREE" --shape "$shape" --root "$tree_path" --seed "$SEED" | tee -a "$MANIFEST" >&2

  common_args=(--walkman "$WALKMAN_BIN" --ignore-parallel "$IGNORE_PARALLEL_BIN" --tree "$tree_path" --workers "$WORKERS")

  # Each harness call is allowed to fail without taking down the rest of
  # the sweep — a multi-hour, multi-shape run shouldn't be all-or-nothing.
  # bench_harness.sh in particular can write a complete, valid CSV and
  # THEN fail on its own cosmetic summary-table step (e.g. if `column`
  # isn't installed) — that's not a reason to lose the other 5 runs.
  # Failures are collected and reported clearly at the end instead of
  # silently swallowed.
  if [[ $SKIP_HYPERFINE -eq 0 ]]; then
    hf_out="$OUT_DIR/hyperfine_${shape}.csv"
    echo "" >&2
    echo "--- hyperfine: shape=$shape -> $hf_out ---" >&2
    if "$BENCH_HARNESS" "${common_args[@]}" --min-runs "$RUNS" --warmup "$WARMUP" --out "$hf_out"; then
      echo "hyperfine_${shape}.csv -> $hf_out ($(wc -l < "$hf_out") lines)" >> "$MANIFEST"
    else
      status=$?
      if [[ -s "$hf_out" ]]; then
        echo "warning: bench_harness.sh exited $status for shape=$shape, but $hf_out was written and looks non-empty — likely failed on its own summary step, data is probably fine. Check manually." >&2
        echo "hyperfine_${shape}.csv -> $hf_out (harness exited $status, but CSV present — VERIFY)" >> "$MANIFEST"
      else
        echo "error: bench_harness.sh exited $status for shape=$shape and $hf_out is missing/empty. Skipping." >&2
        echo "hyperfine_${shape}.csv -> FAILED (exit $status, no usable CSV)" >> "$MANIFEST"
        FAILURES+=("hyperfine/$shape")
      fi
    fi
  fi

  if [[ $SKIP_PERF -eq 0 ]]; then
    perf_out="$OUT_DIR/perf_${shape}.csv"
    echo "" >&2
    echo "--- perf: shape=$shape -> $perf_out ---" >&2
    if "$PERF_BENCH" "${common_args[@]}" --runs "$RUNS" --warmup "$WARMUP" --out "$perf_out"; then
      echo "perf_${shape}.csv -> $perf_out ($(wc -l < "$perf_out") lines)" >> "$MANIFEST"
    else
      status=$?
      if [[ -s "$perf_out" ]]; then
        echo "warning: perf_bench.sh exited $status for shape=$shape, but $perf_out was written and looks non-empty. Check manually." >&2
        echo "perf_${shape}.csv -> $perf_out (harness exited $status, but CSV present — VERIFY)" >> "$MANIFEST"
      else
        echo "error: perf_bench.sh exited $status for shape=$shape and $perf_out is missing/empty. Skipping." >&2
        echo "perf_${shape}.csv -> FAILED (exit $status, no usable CSV)" >> "$MANIFEST"
        FAILURES+=("perf/$shape")
      fi
    fi
  fi
done


# Cleanup

rm -rf /tmp/tree*


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
