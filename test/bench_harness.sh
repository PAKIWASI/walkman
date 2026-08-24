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
# Requires: hyperfine (https://github.com/sharkdp/hyperfine), zsh (for the
# `time` builtin used to capture user+sys CPU seconds — bash's own `time`
# reserved word has no machine-parseable output format, which zsh's does
# via TIMEFMT).
#
# Usage (run from walkman/walkman/test/, against binaries already built
# into walkman/walkman/build/):
#   ./build_tree.sh --shape wide  --root /tmp/tree_wide  --seed 42
#   ./bench_harness.sh \
#       --walkman   ../build/main \
#       --walkdir   ../build/walkdir-cli \
#       --tree      /tmp/tree_wide \
#       --workers   "1,2,4,$(nproc)" \
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
# CPU-time capture uses zsh's `time` builtin rather than the GNU coreutils
# `time` binary — no extra package to install, and TIMEFMT gives a
# machine-parseable "%U %S" line the same way GNU time's -f flag did.
# (bash's own `time` reserved word has no equivalent format control, and
# `command -v time` / `type -P time` can't reliably find it since it's a
# shell keyword, not a PATH executable — hence checking for zsh itself.)
ZSH_BIN="$(type -P zsh || true)"
if [[ -z "$ZSH_BIN" ]]; then
  echo "warning: zsh not found — CPU-time columns will be blank" >&2
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

# Private per-run scratch dir instead of fixed /tmp filenames. Fixed names
# (e.g. /tmp/_hf_walkdir.json) are a collision/permission hazard: a
# leftover file from a prior run — especially one invoked under sudo, or
# by another user/cron job — can be left owned by someone else, and /tmp's
# sticky bit then blocks us from removing or overwriting it. mktemp -d
# gives us a directory only we own, and the trap guarantees cleanup even
# if the script errors out partway through.
SCRATCH="$(mktemp -d "${TMPDIR:-/tmp}/bench_harness.XXXXXX")"
trap 'rm -rf "$SCRATCH"' EXIT

capture_cpu_time() {
  # One extra untimed-by-hyperfine run purely to get user/sys aggregate
  # CPU seconds, since hyperfine itself only reports wall clock. Run
  # after the hyperfine measurement so it doesn't perturb it.
  #
  # Uses zsh's `time` builtin with TIMEFMT set to just "%U %S" — the
  # command's own stdout/stderr are discarded *inside* the timed group,
  # so the only thing landing in $cpu_out is zsh's own timing report,
  # not a mix of the two (which is what silently corrupted the CSV via
  # `sed` before this was rewritten to write whole rows in Python).
  local cmd="$1"
  local cpu_out="$SCRATCH/cpu_time.$$"
  if [[ -n "$ZSH_BIN" ]]; then
    "$ZSH_BIN" -c "TIMEFMT='%U %S'; time ( $cmd >/dev/null 2>&1 )" 2>"$cpu_out"
    read -r user sys < "$cpu_out"
    rm -f "$cpu_out"
    echo "$user,$sys"
  else
    echo ","
  fi
}

# Writes one finished CSV row (hyperfine stats + CPU time together), so
# there's no separate patch-the-row-after-the-fact step and thus nothing
# for a `sed` pattern (or a stray `/` in $cpu) to collide with.
write_row() {
  local tool="$1" workers="$2" json_file="$3" user="$4" sys="$5"
  python3 - "$OUT" "$tool" "$workers" "$json_file" "$user" "$sys" <<'PYEOF'
import json, sys
out, tool, workers, jf, user, sys_t = sys.argv[1:7]
d = json.load(open(jf))["results"][0]
with open(out, "a") as f:
    f.write(f"{tool},{workers},{d['mean']},{d['stddev']},{d['min']},{d['max']},{user},{sys_t}\n")
PYEOF
}

echo "=== walkdir-cli (Rust, sequential — reference point) ==="
CMD="$WALKDIR_BIN --quiet '$TREE'"
hyperfine --warmup "$WARMUP" --min-runs "$MIN_RUNS" --export-json "$SCRATCH/hf_walkdir.json" "$CMD"
IFS=',' read -r cpu_user cpu_sys <<< "$(capture_cpu_time "$CMD")"
write_row "walkdir" "-" "$SCRATCH/hf_walkdir.json" "$cpu_user" "$cpu_sys"

IFS=',' read -ra WLIST <<< "$WORKERS"
for w in "${WLIST[@]}"; do
  echo "=== walkman (Go, workers=$w) ==="
  CMD="$WALKMAN_BIN --quiet --workers $w '$TREE'"
  hyperfine --warmup "$WARMUP" --min-runs "$MIN_RUNS" --export-json "$SCRATCH/hf_walkman.json" "$CMD"
  IFS=',' read -r cpu_user cpu_sys <<< "$(capture_cpu_time "$CMD")"
  write_row "walkman" "$w" "$SCRATCH/hf_walkman.json" "$cpu_user" "$cpu_sys"
done

echo "results written to $OUT"
column -s, -t "$OUT"
