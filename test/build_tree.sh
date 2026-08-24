#!/usr/bin/env bash
# build_tree.sh — reproducible synthetic directory tree generator.
#
# Improvements over the original test/build_tree.sh:
#   1. Fixed seed by default -> byte-identical tree across runs, across
#      machines, across "walkman vs walkdir" invocations. This is what
#      fixes the / non-determinism problem: benchmark THIS, not a live
#      mutating filesystem.
#   2. Shape control (--shape wide|deep|mixed) since a work-stealing pool
#      and a stack-based sequential walker have different asymptotic
#      behavior on wide-shallow trees (lots of independent, stealable work)
#      vs deep-narrow trees (long dependency chains, less to steal).
#      Reporting only one shape hides that difference.
#   3. Writes a manifest (exact file/dir/link counts) next to the tree so
#      every later benchmark run can assert its walker saw the same
#      counts, instead of trusting that nothing drifted.
#
# Usage:
#   ./build_tree.sh --shape wide  --root /tmp/tree_wide  --seed 42
#   ./build_tree.sh --shape deep  --root /tmp/tree_deep  --seed 42
#   ./build_tree.sh --shape mixed --root /tmp/tree_mixed --seed 42
#
# Shapes (tunable via env vars, see below):
#   wide  : shallow, high branching factor  (many independent dirs -> stealable)
#   deep  : deep, low branching factor       (long chains -> little to steal)
#   mixed : moderate depth and branching     (closer to a real source tree)

set -euo pipefail

SHAPE="mixed"
ROOT="/tmp/synth_tree"
SEED=42
LINKS=0   # number of symlinks to scatter in (0 = none, for cycle-free baseline runs)

while [[ $# -gt 0 ]]; do
  case "$1" in
    --shape) SHAPE="$2"; shift 2 ;;
    --root)  ROOT="$2"; shift 2 ;;
    --seed)  SEED="$2"; shift 2 ;;
    --links) LINKS="$2"; shift 2 ;;
    -h|--help)
      grep '^#' "$0" | sed 's/^#//'; exit 0 ;;
    *) echo "unknown arg: $1" >&2; exit 1 ;;
  esac
done

case "$SHAPE" in
  wide)  DEPTH=3;  BRANCH=12; FILES_PER_DIR=6 ;;
  deep)  DEPTH=18; BRANCH=2;  FILES_PER_DIR=2 ;;
  mixed) DEPTH=6;  BRANCH=5;  FILES_PER_DIR=4 ;;
  *) echo "unknown shape: $SHAPE (want wide|deep|mixed)" >&2; exit 1 ;;
esac

rm -rf "$ROOT"
mkdir -p "$ROOT"

# Deterministic PRNG seeded from --seed, independent of system entropy,
# so the exact same tree (same names, same symlink placement) is produced
# every time this is called with the same arguments.
export PYTHONHASHSEED=0
python3 - "$ROOT" "$DEPTH" "$BRANCH" "$FILES_PER_DIR" "$SEED" "$LINKS" <<'PYEOF'
import os, random, sys

root, depth, branch, files_per_dir, seed, links = sys.argv[1:7]
depth, branch, files_per_dir, seed, links = int(depth), int(branch), int(files_per_dir), int(seed), int(links)
rng = random.Random(seed)

dir_count = 0
file_count = 0
all_dirs = [root]

def make_level(path, remaining_depth):
    global dir_count, file_count
    if remaining_depth == 0:
        return
    for i in range(branch):
        d = os.path.join(path, f"d{i}")
        os.makedirs(d, exist_ok=True)
        dir_count += 1
        all_dirs.append(d)
        for j in range(files_per_dir):
            fp = os.path.join(d, f"f{j}.txt")
            with open(fp, "w") as fh:
                fh.write("x" * rng.randint(0, 64))
            file_count += 1
        make_level(d, remaining_depth - 1)

make_level(root, depth)

link_count = 0
for _ in range(links):
    src_dir = rng.choice(all_dirs)
    target_dir = rng.choice(all_dirs)
    link_path = os.path.join(src_dir, f"link{link_count}")
    try:
        os.symlink(target_dir, link_path)
        link_count += 1
    except OSError:
        pass

manifest_path = os.path.join(os.path.dirname(root.rstrip("/")) or ".", os.path.basename(root.rstrip("/")) + ".manifest")
with open(manifest_path, "w") as mf:
    mf.write(f"root={root}\n")
    mf.write(f"shape={sys.argv[2]}\n" if False else "")
    mf.write(f"depth={depth}\n")
    mf.write(f"branch={branch}\n")
    mf.write(f"files_per_dir={files_per_dir}\n")
    mf.write(f"seed={seed}\n")
    mf.write(f"dirs={dir_count}\n")
    mf.write(f"files={file_count}\n")
    mf.write(f"links={link_count}\n")

print(f"built {root}: dirs={dir_count} files={file_count} links={link_count} (seed={seed})")
PYEOF
