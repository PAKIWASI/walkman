#!/usr/bin/env bash
# Builds the same deterministic synthetic tree used in walkman_bench_test.go's
# buildSyntheticTree() (depth=4, breadth=6 -> 1554 dirs, 3110 files), so
# walkman-bench and walkdir-cli can be pointed at an identical, reproducible
# tree instead of a live filesystem that changes between runs.
#
# Usage: ./build_tree.sh /path/to/output/dir

set -euo pipefail

ROOT="${1:?usage: build_tree.sh <output-dir>}"
DEPTH=10
BREADTH=10

build() {
	local dir="$1" level="$2" i
	mkdir -p "$dir"
	echo x > "$dir/file0.txt"
	echo x > "$dir/file1.txt"
	if [ "$level" -ge "$DEPTH" ]; then
		return
	fi
	for ((i = 0; i < BREADTH; i++)); do
		build "$dir/d$i" $((level + 1))
	done
}

build "$ROOT" 0
echo "built synthetic tree at $ROOT"
find "$ROOT" -type f | wc -l | xargs echo "files:"
find "$ROOT" -type d | wc -l | xargs echo "dirs (incl root):"
