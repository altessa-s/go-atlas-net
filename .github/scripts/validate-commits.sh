#!/bin/bash
# Validate full messages of every commit in the requested range.
set -euo pipefail
repo_dir=$(git rev-parse --show-toplevel)
validator="$repo_dir/.github/scripts/validate-commit-message.sh"
head=${HEAD_SHA:-HEAD}

# pr_base prints the merge base of the head with the pull request's base
# branch (origin/$GITHUB_BASE_REF), or fails when that branch is unknown.
pr_base() {
    local ref=${GITHUB_BASE_REF:-}
    if [[ -z $ref ]] || ! git rev-parse -q --verify "refs/remotes/origin/$ref^{commit}" >/dev/null; then
        printf 'BASE_SHA %s is not a commit of this clone and the pull request base branch origin/%s is unknown: fetch the history (fetch-depth: 0)\n' \
            "${BASE_SHA:-(unset)}" "$ref" >&2
        exit 1
    fi
    git merge-base "refs/remotes/origin/$ref" "$head"
}

if [[ $# -gt 0 ]]; then
    range=$1
elif [[ -n "${BASE_SHA:-}" && ! "$BASE_SHA" =~ ^0+$ ]] && git cat-file -e "$BASE_SHA^{commit}" 2>/dev/null; then
    base=$BASE_SHA
    if [[ "${GITHUB_EVENT_NAME:-}" == pull_request ]]; then
        base=$(git merge-base "$base" "$head")
    fi
    range="$base..$head"
elif [[ "${GITHUB_EVENT_NAME:-}" == pull_request ]]; then
    # The base commit is missing (or was not supplied): compare against the
    # pull request's base branch, never against an unrelated default.
    base=$(pr_base)
    range="$base..$head"
elif [[ "${GITHUB_EVENT_NAME:-}" == push ]]; then
    # A new branch or a force push leaves no usable base: validate the whole
    # history reachable from the pushed commit.
    range=$head
elif [[ -n "${BASE_SHA:-}" && ! "$BASE_SHA" =~ ^0+$ ]]; then
    printf 'BASE_SHA %s is not a commit of this clone\n' "$BASE_SHA" >&2
    exit 1
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
