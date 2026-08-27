#!/usr/bin/env bash
# symlink-focused counterpart to run_all.sh.
#
# run_all.sh's trees are always built with --links 0 (see build_tree.sh's
# default), so it never exercises either CLI's --follow-links path. This
# script builds trees that DO contain symlinks and benchmarks both CLIs
# with --follow-links ALWAYS on, so the walkman vs ignore::WalkParallel
# comparison actually covers the code path that resolves a symlink,
# stats it, and (for followed dirs) recurses into it with cycle
# detection — not just the plain no-symlinks walk.
#
# Deliberately no nofollow mode/baseline here — that's close to what plain
# run_all.sh already measures (same tree minus a few Lstat calls on the
# symlinks themselves), so it would double the sweep for marginal new
# information. This script exists specifically to measure --follow-links,
# so that's the only thing it runs.
#
# Output layout (in --out-dir, default ./bench_results_symlinks/):
#   hyperfine_wide.csv    hyperfine_deep.csv    hyperfine_mixed.csv
#   perf_wide.csv         perf_deep.csv         perf_mixed.csv
# (one hardcoded filename per (harness, shape) pair, same reasoning as
# run_all.sh: never reuse an --out path across runs that produced
# different data.)
#
# A run_manifest.txt is written recording exactly which tree (shape/seed/
# dir+file+LINK counts), which binaries, and which workers list produced
# each file.
#
# Usage (run from walkman/walkman/test/, same directory build_tree.sh,
# bench_harness.sh, and perf_bench.sh already live in):
#   ./run_all_sym.sh \
#       --walkman          ../build/main \
#       --ignore-parallel  ../build/ignore-parallel-cli \
#       --workers          "1,2,4,$(nproc)" \
#       --runs             10 \
#       --links            200 \
#       --out-dir          bench_results_symlinks
#
# --links N sets how many symlinks build_tree.sh scatters into each tree
# (default 200). Targets are chosen uniformly at random from every
# directory in the tree, including descendants of the link's own parent,
# so this intentionally includes some symlink cycles — both walkman
# (ancestor dirKey chain) and ignore::WalkParallel (same_file-based) are
# expected to detect and report those rather than hang, and a sweep that
# never creates a cycle wouldn't tell you anything about that path.
#
# Any of --skip-hyperfine / --skip-perf / --shapes narrow what runs, e.g.
# to redo just one shape after a fix: --shapes mixed
#
# Pass --quick for a fast iteration pass (fewer workers, fewer runs) —
# see the QUICK block below.

set -euo pipefail

WALKMAN_BIN=""
IGNORE_PARALLEL_BIN=""
WORKERS="1,2,4,$(nproc 2>/dev/null || echo 4)"
RUNS=10
WARMUP=5
OUT_DIR="bench_results_symlinks"
TREE_ROOT="${TMPDIR:-/tmp}"
SEED=42
SHAPES="wide,deep,mixed"
LINKS=200
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
    --links)   LINKS="$2"; shift 2 ;;
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

for req in WALKMAN_BIN IGNORE_PARALLEL_BIN; do
  if [[ -z "${!req}" ]]; then
    echo "missing required --${req,,} (see --help)" >&2; exit 1
  fi
done

if [[ "$LINKS" -le 0 ]]; then
  echo "error: --links must be > 0 (this script exists specifically to exercise symlinks; use run_all.sh for the no-symlinks sweep)" >&2
  exit 1
fi

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
  echo "run_all_symlinks.sh — $(date -u '+%Y-%m-%d %H:%M:%S UTC')"
  echo "host: $(uname -srmo 2>/dev/null || uname -a)"
  echo "cpu:  $(grep -m1 'model name' /proc/cpuinfo 2>/dev/null | cut -d: -f2 | sed 's/^ *//' || echo unknown)"
  echo "walkman:          $(realpath -- "$WALKMAN_BIN")"
  echo "ignore-parallel:  $(realpath -- "$IGNORE_PARALLEL_BIN")"
  echo "workers:  $WORKERS"
  echo "runs:     $RUNS   warmup: $WARMUP"
  echo "seed:     $SEED   links: $LINKS   mode: follow-links only"
  echo ""
} > "$MANIFEST"

IFS=',' read -ra SHAPE_LIST <<< "$SHAPES"
FAILURES=()

for shape in "${SHAPE_LIST[@]}"; do
  tree_path="$TREE_ROOT/tree_${shape}_symlinks"

  echo "" >&2
  echo "############################################" >&2
  echo "# shape=$shape (links=$LINKS)" >&2
  echo "############################################" >&2

  "$BUILD_TREE" --shape "$shape" --root "$tree_path" --seed "$SEED" --links "$LINKS" | tee -a "$MANIFEST" >&2

  # Sanity check: build_tree.sh silently drops a link if os.symlink() raises
  # (e.g. path collision), so a request for --links 200 could quietly land
  # fewer. Read the actual count back out of the manifest it just wrote
  # rather than assuming LINKS made it onto disk unchanged.
  tree_manifest="${tree_path}.manifest"
  actual_links="$(grep -oP 'links=\K\d+' "$tree_manifest" 2>/dev/null || echo "?")"
  if [[ "$actual_links" != "$LINKS" ]]; then
    echo "note: requested --links $LINKS, tree actually has links=$actual_links (see $tree_manifest)" >&2
  fi
  if [[ "$actual_links" == "0" ]]; then
    echo "error: tree at $tree_path has zero symlinks — nothing for --follow-links to exercise. Skipping shape=$shape." >&2
    FAILURES+=("build/$shape")
    continue
  fi

  common_args=(--walkman "$WALKMAN_BIN" --ignore-parallel "$IGNORE_PARALLEL_BIN" --tree "$tree_path" --workers "$WORKERS" --follow-links)

  # Each harness call is allowed to fail without taking down the rest of
  # the sweep — same reasoning as run_all.sh: bench_harness.sh can write
  # a complete, valid CSV and then fail on its own cosmetic
  # summary-table step, which isn't a reason to lose everything else.
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

rm -rf /tmp/tree*_symlinks /tmp/tree*_symlinks.manifest


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
