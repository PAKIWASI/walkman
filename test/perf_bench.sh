#!/usr/bin/env bash
# perf_bench.sh — perf stat-based comparison of walkman vs walkdir-cli vs
# ignore-parallel-cli, run from walkman/walkman/test/ (or point --*-bin at
# built binaries anywhere).
#
# Unlike bench_harness.sh (hyperfine wall-clock only) or the --bench N
# flags built into each CLI (wall-clock only), this pulls real kernel-level
# performance counters per run via `perf stat`:
#   - task-clock       : actual CPU time consumed (ms), independent of
#                         wall-clock scheduling luck
#   - context-switches  : how much the OS is bouncing this process around
#                         (relevant for a worker-pool comparison)
#   - page-faults       : allocator/memory-mapping pressure — this is what
#                         originally surfaced the realloc-churn difference
#                         between walkman's ReadDir batching and ignore's
#                         per-entry PathBuf allocation
#   - cycles/instructions/cache-misses when the hardware PMU is available
#     (skipped gracefully, printed as "not supported", on VMs/containers
#     without perf_event hardware counter passthrough — task-clock,
#     context-switches, and page-faults always work since they're
#     software events the kernel tracks directly)
#
# Runs -n repetitions per (tool, workers) combination and reports
# mean +/- stddev for every counter, not just wall time, so you can see
# *why* one tool is faster/slower, not just that it is.
#
# Usage:
#   python3 build_tree.py --shape wide --root /tmp/tree_wide --seed 42
#   # (or: ./build_tree.sh --shape wide --root /tmp/tree_wide --seed 42)
#
#   ./perf_bench.sh \
#       --walkman          ../build/main \
#       --walkdir          ../build/walkdir-cli \
#       --ignore-parallel  ../build/ignore-parallel-cli \
#       --tree             /tmp/tree_wide \
#       --workers          "1,2,4,$(nproc)" \
#       --runs             15 \
#       --out              perf_results.csv
#
# Then summarize any perf_results.csv (from this run or a past one) with:
#   ./perf_summary.py perf_results.csv

set -euo pipefail

WALKMAN_BIN=""
WALKDIR_BIN=""
IGNORE_PARALLEL_BIN=""
TREE=""
WORKERS="1,2,4,$(nproc 2>/dev/null || echo 4)"
RUNS=15
WARMUP=3
OUT="perf_results.csv"

while [[ $# -gt 0 ]]; do
  case "$1" in
    --walkman) WALKMAN_BIN="$2"; shift 2 ;;
    --walkdir) WALKDIR_BIN="$2"; shift 2 ;;
    --ignore-parallel) IGNORE_PARALLEL_BIN="$2"; shift 2 ;;
    --tree)    TREE="$2"; shift 2 ;;
    --workers) WORKERS="$2"; shift 2 ;;
    --runs)    RUNS="$2"; shift 2 ;;
    --warmup)  WARMUP="$2"; shift 2 ;;
    --out)     OUT="$2"; shift 2 ;;
    -h|--help) grep '^#' "$0" | sed 's/^#//'; exit 0 ;;
    *) echo "unknown arg: $1" >&2; exit 1 ;;
  esac
done

for req in WALKMAN_BIN WALKDIR_BIN TREE; do
  if [[ -z "${!req}" ]]; then
    echo "missing required --${req,,} (see --help)" >&2; exit 1
  fi
done

# Locate a usable perf binary. On some distro kernels (custom/VM kernels
# without a matching linux-tools-<version> package) the `perf` shim in
# PATH refuses to run at all; fall back to the raw linux-tools binary if
# one is present, since task-clock/context-switches/page-faults (software
# events) work regardless of whether the exact kernel package matches.
PERF_BIN="$(command -v perf || true)"
if [[ -z "$PERF_BIN" ]] || ! "$PERF_BIN" stat -e task-clock -- true >/dev/null 2>&1; then
  ALT="$(find /usr/lib/linux-tools-* -maxdepth 1 -name perf -type f 2>/dev/null | head -1)"
  if [[ -n "$ALT" ]]; then
    PERF_BIN="$ALT"
    echo "note: PATH's perf refused this kernel; using $PERF_BIN directly (software events only)" >&2
  else
    echo "perf not found or not usable. Install with: apt install linux-tools-generic linux-tools-common" >&2
    exit 1
  fi
fi

# Manifest check — same guard as bench_harness.sh, refuses to benchmark a
# tree that doesn't match what build_tree.py/build_tree.sh reported building.
MANIFEST="${TREE%/}.manifest"
if [[ -f "$MANIFEST" ]]; then
  EXPECTED_DIRS=$(grep -oP 'dirs=\K\d+' "$MANIFEST")
  ACTUAL_DIRS=$(find "$TREE" -type d | wc -l)
  ACTUAL_DIRS=$((ACTUAL_DIRS - 1))
  if [[ "$EXPECTED_DIRS" != "$ACTUAL_DIRS" ]]; then
    echo "error: tree at $TREE doesn't match its manifest ($MANIFEST)" >&2
    echo "  expected dirs=$EXPECTED_DIRS, found dirs=$ACTUAL_DIRS — rebuild it" >&2
    exit 1
  fi
else
  echo "warning: no manifest found at $MANIFEST — tree wasn't built by build_tree.py, counts unverified" >&2
fi

EVENTS="task-clock,context-switches,cpu-migrations,page-faults,cycles,instructions,cache-misses"

# Sanity check: run perf once against a trivial command and confirm we
# actually get parseable stats back, before burning the whole sweep on a
# permissions problem that would otherwise fail silently in every single
# row (perf_event_paranoid commonly blocks non-root perf stat by default
# on regular desktop/laptop distros — this is the #1 cause of an
# all-empty-fields CSV, not a bug in the tool being benchmarked).
SANITY_OUT="$(mktemp)"
"$PERF_BIN" stat -e task-clock -- true >/dev/null 2>"$SANITY_OUT" || true
if ! grep -q "task-clock" "$SANITY_OUT"; then
  echo "" >&2
  echo "perf stat produced no usable output on a trivial test command. Raw output was:" >&2
  echo "---" >&2
  cat "$SANITY_OUT" >&2
  echo "---" >&2
  if grep -qi "permission\|paranoid" "$SANITY_OUT"; then
    PARANOID_VAL="$(cat /proc/sys/kernel/perf_event_paranoid 2>/dev/null || echo unknown)"
    echo "" >&2
    echo "This is a perf_event_paranoid permissions issue (current value: $PARANOID_VAL)." >&2
    echo "Fix with ONE of:" >&2
    echo "  sudo sysctl kernel.perf_event_paranoid=-1     # session-only, resets on reboot" >&2
    echo "  sudo $0 ...                                    # just run this script as root" >&2
    echo "  sudo setcap cap_perfmon=ep \$(command -v $PERF_BIN)   # grant perf to this binary only" >&2
  fi
  rm -f "$SANITY_OUT"
  exit 1
fi
rm -f "$SANITY_OUT"

echo "tool,workers,run,task_clock_ms,wall_s,context_switches,cpu_migrations,page_faults,cycles,instructions,cache_misses,user_s,sys_s" > "$OUT"

# Runs one perf stat invocation, parses its stderr report (perf always
# writes stats to stderr, stdout is left free for the wrapped program),
# and appends one CSV row. Fields perf marks "<not supported>" (common on
# VMs without hardware PMU passthrough) are written as empty rather than
# guessed at.
#
# Takes the target command as an ARRAY ("$@" from position 4 onward), not
# a quoted string — string-interpolating a command that itself contains
# quoted arguments (e.g. "$BIN --quiet '$TREE'") and then word-splitting
# it unquoted leaves literal quote characters IN the argument, which
# silently fails the walk on a mangled path (looks like an ultra-fast,
# suspiciously-good benchmark result instead of an error). Passing a real
# array avoids that entirely.
run_once() {
  local tool="$1" workers="$2" run_idx="$3"; shift 3
  local perf_out
  perf_out="$(mktemp)"
  "$PERF_BIN" stat -e "$EVENTS" -- "$@" >/dev/null 2>"$perf_out" || true

  if ! grep -q "task-clock\|seconds time elapsed" "$perf_out"; then
    echo "warning: perf produced no parseable output for $tool workers=$workers run=$run_idx — row will be blank. Raw perf output:" >&2
    sed 's/^/    /' "$perf_out" >&2
  fi

  python3 - "$OUT" "$tool" "$workers" "$run_idx" "$perf_out" <<'PYEOF'
import re, sys

out, tool, workers, run_idx, perf_out = sys.argv[1:6]
text = open(perf_out).read()

def grab(pattern):
    m = re.search(pattern, text)
    if not m:
        return ""
    return m.group(1).replace(",", "")

task_clock = grab(r"([\d.]+)\s+msec task-clock")
wall = grab(r"([\d.]+)\s+seconds time elapsed")
ctx_sw = grab(r"([\d,]+)\s+context-switches")
migrations = grab(r"([\d,]+)\s+cpu-migrations")
faults = grab(r"([\d,]+)\s+page-faults")
cycles = grab(r"([\d,]+)\s+cycles")
instr = grab(r"([\d,]+)\s+instructions")
cache_miss = grab(r"([\d,]+)\s+cache-misses")
user = grab(r"([\d.]+)\s+seconds user")
sys_t = grab(r"([\d.]+)\s+seconds sys")

with open(out, "a") as f:
    f.write(f"{tool},{workers},{run_idx},{task_clock},{wall},{ctx_sw},{migrations},"
            f"{faults},{cycles},{instr},{cache_miss},{user},{sys_t}\n")
PYEOF
  rm -f "$perf_out"
}

bench_tool() {
  local label="$1" tool="$2" workers="$3"; shift 3
  echo "=== $label ===" >&2
  for ((i = 0; i < WARMUP; i++)); do
    "$@" >/dev/null 2>&1 || true
  done
  for ((i = 1; i <= RUNS; i++)); do
    run_once "$tool" "$workers" "$i" "$@"
  done
}

bench_tool "walkdir-cli (Rust, sequential — reference point)" \
  "walkdir" "-" \
  "$WALKDIR_BIN" --quiet "$TREE"

IFS=',' read -ra WLIST <<< "$WORKERS"
for w in "${WLIST[@]}"; do
  bench_tool "walkman (Go, workers=$w)" \
    "walkman" "$w" \
    "$WALKMAN_BIN" --quiet --workers "$w" "$TREE"
done

if [[ -n "$IGNORE_PARALLEL_BIN" ]]; then
  for w in "${WLIST[@]}"; do
    bench_tool "ignore-parallel-cli (Rust, ignore::WalkParallel, workers=$w)" \
      "ignore-parallel" "$w" \
      "$IGNORE_PARALLEL_BIN" --quiet --workers "$w" "$TREE"
  done
fi

echo "results written to $OUT" >&2
echo "summarize with: ./perf_summary.py $OUT" >&2
