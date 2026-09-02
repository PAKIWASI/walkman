#!/usr/bin/env bash
# run_all_sym.sh — symlink-focused counterpart to run_all.sh.
#
# run_all.sh benchmarks plain traversal, never passing --follow-links, so
# it never exercises either CLI's symlink-resolution / cycle-detection
# path. This script runs both CLIs with --follow-links ALWAYS on, against
# the same kind of real, user-supplied tree as run_all.sh — relying on
# whatever symlinks already exist in that tree (a real checkout like the
# Linux kernel source has a natural handful — e.g. arch/header symlinks —
# without needing to fabricate any).
#
# Output layout (in --out-dir, default ./bench_results_symlinks/):
#   hyperfine.csv
#   perf.csv
#
# A run_manifest.txt is written recording exactly which tree (path +
# actual dir/file/symlink counts at run time), which binaries, and which
# workers list produced each file.
#
# Usage (run from walkman/walkman/test/, same directory bench_harness.sh
# and perf_bench.sh already live in):
#   ./run_all_sym.sh \
#       --walkman          ../build/walkman \
#       --fastwalk         ../build/fastwalk \
#       --ignore-parallel  ../build/ignore-parallel-cli \
#       --tree             /path/to/linux-7.2.2 \
#       --workers          "1,2,4,$(nproc)" \
#       --runs             10 \
#       --out-dir          bench_results_symlinks
#
# Either of --skip-hyperfine / --skip-perf narrows what runs.
#
# Pass --quick for a fast iteration pass (fewer workers, fewer runs) —
# see the QUICK block below.

set -euo pipefail

WALKMAN_BIN=""
IGNORE_PARALLEL_BIN=""
FASTWALK_BIN=""
TREE=""
WORKERS="1,2,4,$(nproc 2>/dev/null || echo 4)"
RUNS=10
WARMUP=5
OUT_DIR="bench_results_symlinks"
SKIP_HYPERFINE=0
SKIP_PERF=0
QUICK=0

while [[ $# -gt 0 ]]; do
  case "$1" in
    --walkman) WALKMAN_BIN="$2"; shift 2 ;;
    --ignore-parallel) IGNORE_PARALLEL_BIN="$2"; shift 2 ;;
    --fastwalk) FASTWALK_BIN="$2"; shift 2 ;;
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

# --quick: same fast-iteration defaults as run_all.sh's --quick (see that
# script for the reasoning) — narrows the worker sweep to 1,nproc and
# lowers run/warmup counts. Any explicit --workers/--runs/--warmup you
# passed above still wins.
if [[ "$QUICK" -eq 1 ]]; then
  [[ "$WORKERS" == "1,2,4,$(nproc 2>/dev/null || echo 4)" ]] && WORKERS="1,$(nproc 2>/dev/null || echo 4)"
  [[ "$RUNS" == 10 ]] && RUNS=6
  [[ "$WARMUP" == 5 ]] && WARMUP=2
fi

if [[ -z "$TREE" ]]; then
  echo "error: --tree $TREE is required" >&2
  exit 1
fi

if [[ -z "$WALKMAN_BIN" && -z "$IGNORE_PARALLEL_BIN" && -z "$FASTWALK_BIN" ]]; then
  echo "missing at least one binary: specify --walkman, --ignore-parallel, or --fastwalk (see --help)" >&2
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

echo "counting tree contents (this can take a moment on a large tree)..." >&2
TREE_DIRS=$(find "$TREE" -mindepth 1 -type d | wc -l)
TREE_FILES=$(find "$TREE" -type f | wc -l)
TREE_LINKS=$(find "$TREE" -type l | wc -l)

# This script exists specifically to exercise --follow-links; a tree with
# zero symlinks in it wouldn't tell you anything that plain run_all.sh
# doesn't already cover.
if [[ "$TREE_LINKS" -eq 0 ]]; then
  echo "error: --tree $TREE contains zero symlinks — nothing for --follow-links to exercise." >&2
  echo "  point this at a tree that has some (a real checkout usually does), or use run_all.sh instead." >&2
  exit 1
fi

{
  echo "run_all_sym.sh — $(date -u '+%Y-%m-%d %H:%M:%S UTC')"
  echo "host: $(uname -srmo 2>/dev/null || uname -a)"
  echo "cpu:  $(grep -m1 'model name' /proc/cpuinfo 2>/dev/null | cut -d: -f2 | sed 's/^ *//' || echo unknown)"
  [[ -n "$WALKMAN_BIN" ]] && echo "walkman:          $(realpath -- "$WALKMAN_BIN")"
  [[ -n "$IGNORE_PARALLEL_BIN" ]] && echo "ignore-parallel:  $(realpath -- "$IGNORE_PARALLEL_BIN")"
  [[ -n "$FASTWALK_BIN" ]] && echo "fastwalk:         $(realpath -- "$FASTWALK_BIN")"
  echo "tree:             $(realpath -- "$TREE")"
  echo "tree dirs=$TREE_DIRS files=$TREE_FILES symlinks=$TREE_LINKS"
  echo "workers:  $WORKERS"
  echo "runs:     $RUNS   warmup: $WARMUP"
  echo "mode:     follow-links only"
  echo ""
} > "$MANIFEST"

FAILURES=()

common_args=(--tree "$TREE" --workers "$WORKERS" --follow-links)
[[ -n "$WALKMAN_BIN" ]] && common_args+=(--walkman "$WALKMAN_BIN")
[[ -n "$IGNORE_PARALLEL_BIN" ]] && common_args+=(--ignore-parallel "$IGNORE_PARALLEL_BIN")
[[ -n "$FASTWALK_BIN" ]] && common_args+=(--fastwalk "$FASTWALK_BIN")

# Each harness call is allowed to fail without taking down the other —
# same reasoning as run_all.sh: bench_harness.sh can write a complete,
# valid CSV and then fail on its own cosmetic summary-table step, which
# isn't a reason to lose the other run's output.
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
