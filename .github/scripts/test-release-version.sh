#!/bin/bash
# Exercise check-release-version.sh with accepted and rejected versions.
set -euo pipefail
script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
checks=0

# check EXPECTED VERSION [MODULE_PATH] runs the check (MODULE_PATH defaults to
# a path without a major version suffix) and compares its exit status.
check() {
    local expected=$1 version=$2 module=${3-example.com/m} status=0
    bash "$script_dir/check-release-version.sh" "$version" "$module" >/dev/null 2>&1 || status=$?
    if [[ $status -ne $expected ]]; then
        printf 'check-release-version.sh %q %q: exit %s, want %s\n' "$version" "$module" "$status" "$expected" >&2
        exit 1
    fi
    checks=$((checks + 1))
}

# Syntax.
for version in v0.0.0 v1.2.3 v1.20.30 v1.2.3-rc.1 v1.2.3-alpha v1.2.3-0 v1.0.0-0.3.7 v1.0.0-x.7.z.92 v1.0.0-x-y-z.-- \
    v1.2.3-01a; do
    check 0 "$version"
done

for version in "" v 1.2.3 V1.2.3 v1 v1.2 v1.2.3.4 v01.2.3 v1.02.3 v1.2.03 v1.2.3- v1.2.3-.. v1.2.3-rc. v1.2.3-.rc \
    v1.2.3-rc..1 v1.2.3-01 v1.2.3-rc.01 v1.2.3+ v1.2.3+. v1.2.3+a..b v1.2.3+build.5 v1.2.3+001 \
    v1.2.3-rc.1+exp.sha.5114f85 v1.2.3-rc_1 'v1.2.3 ' ' v1.2.3' $'v1.2.3\n' \
    'v1.2.3;id' 'v1.2.3$(id)' 'v1.2.3`id`' "v1.2.3'" 'v1.2.3"' 'v1.2.3 v1.2.4'; do
    check 1 "$version"
done

# Major version rule.
check 1 v2.0.0
check 1 v10.20.30
check 0 v2.0.0 example.com/m/v2
check 0 v2.3.4-rc.1 example.com/m/v2
check 1 v1.0.0 example.com/m/v2
check 1 v3.0.0 example.com/m/v2
check 0 v10.20.30 example.com/m/v10
check 0 v1.0.0 example.com/m/v1x

# The repository's own go.mod is the default module path.
module=$(awk '$1 == "module" { print $2; exit }' "$script_dir/../../go.mod")
if [[ $module =~ /v([1-9][0-9]*)$ && ${BASH_REMATCH[1]} -ge 2 ]]; then own=v${BASH_REMATCH[1]}.0.0; else own=v0.1.0; fi
status=0
bash "$script_dir/check-release-version.sh" "$own" >/dev/null 2>&1 || status=$?
if [[ $status -ne 0 ]]; then
    printf 'check-release-version.sh %s with the root go.mod (%s): exit %s, want 0\n' "$own" "$module" "$status" >&2
    exit 1
fi
checks=$((checks + 1))

printf 'Passed %s release version checks\n' "$checks"
