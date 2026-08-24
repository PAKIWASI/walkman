#!/usr/bin/env bash
# bench_harness.sh — fair, statistically-rigorous walkman vs walkdir-cli comparison.
#
# What this fixes vs the hand-rolled --bench N loop in main.go / main.rs:
#
#   1. Non-deterministic root. Always benchmarks a tree built by
#      build_tree.sh (fixed seed) rather than "/" — see that script's
#      header for why "/" invalidates the comparison.
#
#   2. No warm-up. hyperfine's --warmup runs N untimed passes first, so
#      you're comparing steady-state page-cache-hot performance for BOTH
#      binaries, not "whichever one happened to run first got the cold
#      cache penalty." (If you specifically want cold-cache numbers, see
#      the --cold flag below — page cache eviction needs root, so it's
#      opt-in and will just warn and skip if not available.)
#
#   3. Manual min/avg/max has no rigor. hyperfine reports mean, stddev,
#      and min/max across --min-runs (default 10, configurable), and
#      flags statistical outliers instead of silently averaging them in.
#
#   4. Wall-clock alone hides total CPU cost. A worker pool can win on
#      wall time while burning more aggregate core-seconds — that's a
#      completely reasonable trade (per your point: idle cores are
#      wasted, so spending more total CPU for less wall time is usually
#      a win) but it should be a number you can SEE, not an assumption.
#      This script captures user+sys time per run via `/usr/bin/time`
#      alongside hyperfine's wall-clock numbers.
#
#   5. Single worker-count snapshot. This sweeps --workers across
#      1,2,4,8,$(nproc) (configurable) so you get the actual scaling
#      curve, not one point on it — which is what tells you whether
#      you're still climbing or already past the plateau the walkman.go
#      comment mentions for workstealpool.
#
# Requires: hyperfine (https://github.com/sharkdp/hyperfine), GNU time
# (`apt install time` — the standalone binary at /usr/bin/time, not the
# shell builtin).
#
# Usage:
#   ./build_tree.sh --shape wide  --root /tmp/tree_wide  --seed 42
#   ./bench_harness.sh \
#       --walkman   /path/to/walkman/build/main \
#       --walkdir   /path/to/walkman/build/walkdir-cli \
#       --tree      /tmp/tree_wide \
#       --workers   "1,2,4,8,$(nproc)" \
#       --out       results_wide.csv

set -euo pipefail

WALKMAN_BIN=""
WALKDIR_BIN=""
TREE=""
WORKERS="1,2,4,$(nproc 2>/dev/null || echo 4)"
OUT="bench_results.csv"
MIN_RUNS=15
WARMUP=5
COLD=0

while [[ $# -gt 0 ]]; do
  case "$1" in
    --walkman) WALKMAN_BIN="$2"; shift 2 ;;
    --walkdir) WALKDIR_BIN="$2"; shift 2 ;;
    --tree)    TREE="$2"; shift 2 ;;
    --workers) WORKERS="$2"; shift 2 ;;
    --out)     OUT="$2"; shift 2 ;;
    --min-runs) MIN_RUNS="$2"; shift 2 ;;
    --warmup)  WARMUP="$2"; shift 2 ;;
    --cold)    COLD=1; shift 1 ;;
    -h|--help) grep '^#' "$0" | sed 's/^#//'; exit 0 ;;
    *) echo "unknown arg: $1" >&2; exit 1 ;;
  esac
done

for req in WALKMAN_BIN WALKDIR_BIN TREE; do
  if [[ -z "${!req}" ]]; then
    echo "missing required --${req,,} (see --help)" >&2; exit 1
  fi
done

command -v hyperfine >/dev/null || { echo "hyperfine not found: https://github.com/sharkdp/hyperfine#installation" >&2; exit 1; }
GNU_TIME="$(command -v time || true)"
if [[ -z "$GNU_TIME" ]]; then
  echo "warning: GNU time not found (apt install time) — CPU-time columns will be blank" >&2
fi

if [[ "$COLD" -eq 1 ]]; then
  if [[ -w /proc/sys/vm/drop_caches ]]; then
    sync; echo 3 > /proc/sys/vm/drop_caches
    echo "dropped page cache for cold-cache run"
  else
    echo "warning: --cold requested but can't write /proc/sys/vm/drop_caches (need root) — skipping" >&2
  fi
fi

# Manifest check: refuse to benchmark a tree whose manifest doesn't match
# what's actually on disk, so a stale/partially-built tree can't silently
# skew results.
MANIFEST="${TREE%/}.manifest"
if [[ -f "$MANIFEST" ]]; then
  EXPECTED_DIRS=$(grep -oP 'dirs=\K\d+' "$MANIFEST")
  ACTUAL_DIRS=$(find "$TREE" -type d | wc -l)
  ACTUAL_DIRS=$((ACTUAL_DIRS - 1)) # exclude root itself, matching manifest's convention
  if [[ "$EXPECTED_DIRS" != "$ACTUAL_DIRS" ]]; then
    echo "error: tree at $TREE doesn't match its manifest ($MANIFEST)" >&2
    echo "  expected dirs=$EXPECTED_DIRS, found dirs=$ACTUAL_DIRS — rebuild with build_tree.sh" >&2
    exit 1
  fi
else
  echo "warning: no manifest found at $MANIFEST — tree wasn't built by build_tree.sh, counts unverified" >&2
fi

echo "tool,workers,mean_wall_s,stddev_wall_s,min_wall_s,max_wall_s,user_s,sys_s" > "$OUT"

capture_cpu_time() {
  # One extra untimed-by-hyperfine run purely to get user/sys aggregate
  # CPU seconds via GNU time, since hyperfine itself only reports wall
  # clock. Run after the hyperfine measurement so it doesn't perturb it.
  local cmd="$1"
  if [[ -n "$GNU_TIME" ]]; then
    "$GNU_TIME" -f "%U %S" -o /tmp/_cpu_time.$$ bash -c "$cmd" >/dev/null 2>/tmp/_cpu_time.$$
    read -r user sys < /tmp/_cpu_time.$$
    rm -f /tmp/_cpu_time.$$
    echo "$user,$sys"
  else
    echo ","
  fi
}

echo "=== walkdir-cli (Rust, sequential — reference point) ==="
CMD="$WALKDIR_BIN --quiet '$TREE'"
hyperfine --warmup "$WARMUP" --min-runs "$MIN_RUNS" --export-json /tmp/_hf_walkdir.json "$CMD"
python3 - "$OUT" "walkdir" "-" /tmp/_hf_walkdir.json <<'PYEOF'
import json, sys
out, tool, workers, jf = sys.argv[1:5]
d = json.load(open(jf))["results"][0]
with open(out, "a") as f:
    f.write(f"{tool},{workers},{d['mean']},{d['stddev']},{d['min']},{d['max']},,\n")
PYEOF
cpu=$(capture_cpu_time "$CMD")
sed -i "s/^walkdir,-,\([^,]*,[^,]*,[^,]*,[^,]*\),,\$/walkdir,-,\1,${cpu}/" "$OUT"

IFS=',' read -ra WLIST <<< "$WORKERS"
for w in "${WLIST[@]}"; do
  echo "=== walkman (Go, workers=$w) ==="
  CMD="$WALKMAN_BIN --quiet --workers $w '$TREE'"
  hyperfine --warmup "$WARMUP" --min-runs "$MIN_RUNS" --export-json /tmp/_hf_walkman.json "$CMD"
  python3 - "$OUT" "walkman" "$w" /tmp/_hf_walkman.json <<'PYEOF'
import json, sys
out, tool, workers, jf = sys.argv[1:5]
d = json.load(open(jf))["results"][0]
with open(out, "a") as f:
    f.write(f"{tool},{workers},{d['mean']},{d['stddev']},{d['min']},{d['max']},,\n")
PYEOF
  cpu=$(capture_cpu_time "$CMD")
  sed -i "s/^walkman,${w},\([^,]*,[^,]*,[^,]*,[^,]*\),,\$/walkman,${w},\1,${cpu}/" "$OUT"
done

rm -f /tmp/_hf_walkdir.json /tmp/_hf_walkman.json
echo "results written to $OUT"
column -s, -t "$OUT"
