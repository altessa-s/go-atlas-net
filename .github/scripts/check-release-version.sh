#!/bin/bash
# Check a release version for make release.
#
# Usage: check-release-version.sh VERSION [MODULE_PATH]
#
# VERSION must be a strict SemVer 2.0.0 version with a v prefix and without
# build metadata, as Go module versions require: vMAJOR.MINOR.PATCH without
# leading zeros and an optional prerelease of non-empty dot-separated
# identifiers (numeric ones without leading zeros). A tag with build metadata
# (+...) is no usable module version, so it is rejected.
#
# It must also follow Go's major version rule for MODULE_PATH (by default the
# module path of the repository's root go.mod): a path without a /vN suffix
# takes only v0.x.y and v1.x.y, a path ending in /vN (N >= 2) only vN.x.y.
#
# The version is only matched, never evaluated. Exits 0 when it is valid and
# 1 with a usage message otherwise.
set -euo pipefail

version=${1-}
module=${2-}
if [[ -z $module ]]; then
    root=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
    if [[ -f $root/go.mod ]]; then
        module=$(awk '$1 == "module" { print $2; exit }' "$root/go.mod")
    fi
    module=${module//\"/}
fi

usage() {
    {
        printf 'invalid VERSION %q for module %s: %s\n' "$version" "$module" "$1"
        echo "Usage: make release VERSION=v1.2.3"
        echo "Current version: $(git describe --tags --abbrev=0 --match 'v*' 2>/dev/null || echo 'no tags')"
    } >&2
    exit 1
}

num='(0|[1-9][0-9]*)'
pre='(0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*)'
pattern="^v$num\\.$num\\.$num(-$pre(\\.$pre)*)?\$"

if ! [[ $version =~ $pattern ]]; then
    usage "want vMAJOR.MINOR.PATCH[-PRERELEASE] (SemVer 2.0.0 without build metadata, no leading zeros)"
fi
major=${BASH_REMATCH[1]}

if [[ -z $module ]]; then
    usage "no module path (go.mod has no module line)"
fi
if [[ $module =~ /v([1-9][0-9]*)$ && ${BASH_REMATCH[1]} -ge 2 ]]; then
    suffix=${BASH_REMATCH[1]}
    if [[ $major != "$suffix" ]]; then
        usage "the module path ends in /v$suffix, so the major version must be $suffix"
    fi
elif [[ $major != 0 && $major != 1 ]]; then
    usage "a module path without a /vN suffix takes only v0 and v1; move the module to .../v$major first"
fi
