#!/bin/bash
# Compare the benchmarks of HEAD against a base ref and fail on a regression.
#
# Usage: bench-regression.sh BASE_REF
#
# The base is checked out into a temporary worktree, both trees run every
# benchmark BENCH_COUNT times, and benchstat compares them. A benchmark fails
# the run when benchstat reports a significant increase (alpha 0.01) in
# sec/op or allocs/op larger than BENCH_THRESHOLD percent. Benchmarks that
# exist on only one side are reported by benchstat but never fail the run.
#
# Environment:
#   BENCH_THRESHOLD  allowed significant increase, percent (default 20)
#   BENCH_COUNT      runs per benchmark, benchstat needs >= 6 (default 8)
#   BENCH_TIME       -benchtime per run (default 300ms)
#   BENCH_REPORT     also write the benchstat report to this file
set -euo pipefail

base_ref=${1:?usage: bench-regression.sh BASE_REF}
threshold=${BENCH_THRESHOLD:-20}
count=${BENCH_COUNT:-8}
benchtime=${BENCH_TIME:-300ms}

repo_dir=$(git rev-parse --show-toplevel)
work=$(mktemp -d)
cleanup() {
    git -C "$repo_dir" worktree remove --force "$work/base" >/dev/null 2>&1 || true
    rm -rf "$work"
}
trap cleanup EXIT

# run_benchmarks DIR OUT runs every benchmark of every module of the tree at
# DIR (each tracked go.mod outside devtools and testdata) and writes the lines
# benchstat reads to OUT. Modules under testdata are generator inputs that need
# not compile on their own; their generated output is checked by the tests.
# A failing run prints its output and fails; a tree without benchmarks yields
# an empty OUT.
run_benchmarks() {
    local modules m
    modules=$(git -C "$1" ls-files -- go.mod '*/go.mod' | grep -Ev '^devtools/|(^|/)testdata/' | xargs -n1 dirname)
    : >"$2"
    for m in $modules; do
        if ! go -C "$1/$m" test -run='^$' -bench=. -benchmem -count="$count" -benchtime="$benchtime" ./... >"$2.raw" 2>&1; then
            cat "$2.raw" >&2
            return 1
        fi
        grep -E '^(goos|goarch|pkg|cpu|Benchmark)' "$2.raw" >>"$2" || [[ $? -eq 1 ]]
    done
}

git -C "$repo_dir" worktree add --detach "$work/base" "$base_ref" >/dev/null
echo "Running benchmarks on $base_ref ..."
run_benchmarks "$work/base" "$work/base.txt"
echo "Running benchmarks on HEAD ..."
run_benchmarks "$repo_dir" "$work/head.txt"

go -C "$repo_dir/devtools" build -o "$work/benchstat" golang.org/x/perf/cmd/benchstat
"$work/benchstat" -alpha 0.01 base="$work/base.txt" head="$work/head.txt" | tee "$work/report.txt"
if [[ -n "${BENCH_REPORT:-}" ]]; then
    cp "$work/report.txt" "$BENCH_REPORT"
fi

# benchstat emits "~" for insignificant changes. Parse CSV so commas inside
# quoted benchmark names cannot shift the delta into a different column.
"$work/benchstat" -alpha 0.01 -format csv base="$work/base.txt" head="$work/head.txt" >"$work/report.csv"
python3 - "$work/report.csv" "$threshold" <<'PYTHON'
import csv
import re
import sys
from decimal import Decimal

threshold = Decimal(sys.argv[2])
unit = None
failed = False
with open(sys.argv[1], newline="") as report:
    for row in csv.reader(report):
        if len(row) != 7:
            continue
        name, base, _, head, _, delta, comparison = row
        if not name:
            unit = base if delta == "vs base" and head == base else None
        elif unit in ("sec/op", "allocs/op") and name != "geomean" and comparison:
            if re.fullmatch(r"\+\d+(?:\.\d+)?%", delta) and Decimal(delta[:-1]) > threshold:
                print(f"REGRESSION {name} {unit}: {delta} (threshold {threshold}%)")
                failed = True
sys.exit(int(failed))
PYTHON
echo "No benchmark regressed by more than ${threshold}%."
