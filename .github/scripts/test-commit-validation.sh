#!/bin/bash
# Exercise the shared validator and the installed-hook entry point without commits.
set -euo pipefail
repo_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
cd "$repo_dir"
message_file=$(mktemp)
trap 'rm -f "$message_file"' EXIT
checks=0

check() {
    local expected=$1 message=$2 entry actual
    printf '%s\n' "$message" > "$message_file"
    for entry in .github/scripts/validate-commit-message.sh .githooks/commit-msg; do
        actual=0
        if [[ "$entry" == .githooks/commit-msg ]]; then
            bash "$entry" "$message_file" >/dev/null 2>&1 || actual=$?
        else
            bash "$entry" < "$message_file" >/dev/null 2>&1 || actual=$?
        fi
        if [[ "$actual" != "$expected" ]]; then
            printf 'Unexpected status %s (wanted %s) from %s: %s\n' "$actual" "$expected" "$entry" "$message" >&2
            exit 1
        fi
        checks=$((checks + 1))
    done
}

check 0 'fix(http/client): preserve provider usage'
check 0 'build: pin the options generator'
check 0 'feat(urlx)!: reject recursive definitions'
check 0 'refactor!: remove the legacy entry point'
check 0 $'fix(http/client): preserve provider usage\n\nKeep usage when a paid reply contains an error.'
check 0 $'feat(urlx)!: reject recursion\n\nBREAKING CHANGE: recursive definitions now fail before dispatch.'
check 0 $'fix(http/client): preserve provider usage\n\nCo-authored-by: Jane Doe <jane@example.com>'
check 0 'ci: enforce the coverage floor'
check 0 'revert: restore endpoint validation'
check 1 'fix(root): update configuration'
check 1 'build(deps): '
check 1 'fix(unknown): update configuration'
check 1 'Fix(http/client): preserve provider usage'
check 1 'fix(http/client): Preserve provider usage'
check 1 'update configuration'
check 0 "Merge branch 'release/v0.1.3'"
check 0 "Merge tag 'v0.1.3' into develop"
check 0 'Merge pull request #12 from altessa-s/feature/x'
check 1 'Merge branch develop'
check 1 'Merge the release'
check 1 $'Merge branch \'develop\'\n\nGenerated with Claude Code'
check 1 $'fix(http/client): preserve provider usage\n\nTests: go test ./... passed'
check 1 $'fix(http/client): preserve provider usage\n\nCoverage: 95%'
check 1 $'fix(http/client): preserve provider usage\n\nAll tests passed'
check 1 $'fix(http/client): preserve provider usage\n\nGenerated with Claude Code'
check 1 $'build(deps): update the generator\n\nCo-authored-by: Codex <codex@example.com>'
check 1 $'fix(http/client): preserve provider usage\n\nAgent: Codex'
printf 'Passed %s commit validation checks\n' "$checks"
