#!/bin/bash
# Compare the benchmarks of HEAD against a base ref and fail on a regression.
#
# Usage: bench-regression.sh BASE_REF
#
# The base is checked out into a temporary worktree, both trees run every
# benchmark BENCH_COUNT times, and benchstat compares them. A benchmark fails
# the run when benchstat reports a statistically significant increase in
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
# DIR (each tracked go.mod outside devtools) and writes the lines benchstat
# reads to OUT. A failing run prints its output and fails; a tree without
# benchmarks yields an empty OUT.
run_benchmarks() {
    local modules m
    modules=$(git -C "$1" ls-files -- go.mod '*/go.mod' | grep -v '^devtools/' | xargs -n1 dirname)
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
"$work/benchstat" base="$work/base.txt" head="$work/head.txt" | tee "$work/report.txt"
if [[ -n "${BENCH_REPORT:-}" ]]; then
    cp "$work/report.txt" "$BENCH_REPORT"
fi

# CSV rows: name,base,CI,head,CI,vs base,P. A unit header row carries the unit
# in column 2; "vs base" is "~" when the change is not significant.
"$work/benchstat" -format csv base="$work/base.txt" head="$work/head.txt" 2>/dev/null |
    awk -F, -v threshold="$threshold" '
        $1 == "" && $2 != "" { unit = $2; next }
        unit != "sec/op" && unit != "allocs/op" { next }
        $1 == "geomean" || $6 !~ /^\+/ { next }
        {
            pct = $6
            gsub(/[+%]/, "", pct)
            if (pct + 0 > threshold + 0) {
                printf "REGRESSION %s %s: +%s%% (threshold %s%%)\n", $1, unit, pct, threshold
                failed = 1
            }
        }
        END { exit failed }
    '
echo "No benchmark regressed by more than ${threshold}%."
