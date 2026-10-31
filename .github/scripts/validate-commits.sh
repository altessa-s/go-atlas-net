#!/bin/bash
# Validate full messages of every commit in the requested range.
set -euo pipefail
repo_dir=$(git rev-parse --show-toplevel)
validator="$repo_dir/.github/scripts/validate-commit-message.sh"
head=${HEAD_SHA:-HEAD}

if [[ $# -gt 0 ]]; then
    range=$1
elif [[ -n "${BASE_SHA:-}" && ! "$BASE_SHA" =~ ^0+$ ]]; then
    base=$BASE_SHA
    if [[ "${GITHUB_EVENT_NAME:-}" == pull_request ]]; then
        base=$(git merge-base "$base" "$head")
    fi
    range="$base..$head"
elif [[ "${GITHUB_EVENT_NAME:-}" == push ]]; then
    range=$head
else
    base=$(git merge-base origin/develop "$head")
    range="$base..$head"
fi

commits=$(git rev-list --reverse "$range" --)
failed=0
while IFS= read -r hash; do
    [[ -n "$hash" ]] || continue
    if git show -s --format=%B "$hash" | bash "$validator"; then
        printf 'Valid: %s\n' "$hash"
    else
        printf 'Invalid: %s\n' "$hash" >&2
        failed=1
    fi
done <<< "$commits"
exit "$failed"
