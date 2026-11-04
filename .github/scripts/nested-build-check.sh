#!/bin/bash
# Build every sub-module of the Makefile's NESTED_CHECK_MODULES (the nested
# modules released with the root and the sub-modules released on their own)
# against the root module without replace directives, the way a consumer's
# module graph combines them. Below, "nested module" means one of these.
#
# Usage: nested-build-check.sh
#
# The nested go.mod files point at the working tree with replace directives,
# which hides two kinds of breakage that only show up after a release: a
# nested module that relies on something the root module zip does not carry
# (an internal package, a file outside the module boundary), and a root change
# that breaks a nested module version already released. This script:
#
#   1. Serves the root module and every nested module from the working tree
#      through a local file:// GOPROXY at one pseudo-version derived from the
#      tree contents, with every go-atlas-net requirement rewritten to it and the
#      replace directives dropped.
#   2. Vets each nested module from its served zip against the served root:
#      go vet compiles every package together with its test files, but no
#      test runs here (the module's own test targets run them).
#   3. Builds (go build, no tests) the latest released tag of each nested
#      module (<dir>/v*) against the served root, so a root change breaking a
#      released nested module fails here. A nested module without a release
#      is skipped.
#
# Environment:
#   NESTED_BUILD_ACCEPT  space-separated <tag>@<commit> entries naming released
#                        nested tags whose breakage is accepted (reported, not
#                        failed). The commit (a prefix of the tagged commit
#                        hash) pins the entry, so a tag moved to another commit
#                        is checked again.
#
# The pseudo-version is a hash of the served contents, so a rerun over an
# unchanged tree reuses the module cache entries and a changed tree never sees
# stale ones. Requires git, zip and the Go toolchain; GOWORK is ignored.
set -euo pipefail

module=github.com/altessa-s/go-atlas-net
repo_dir=$(git rev-parse --show-toplevel)
cd "$repo_dir"

read -r -a nested <<<"$(make --no-print-directory -s print-nested-check-modules | tr '\n' ' ')"
if [[ ${#nested[@]} -eq 0 ]]; then
    echo "no nested modules"
    exit 0
fi

work=$(mktemp -d)
trap 'chmod -R u+w "$work" 2>/dev/null || true; rm -rf "$work"' EXIT

# Directories holding a go.mod other than the root: module boundaries that a
# module zip never crosses.
boundaries=$(git ls-files -co --exclude-standard -- '*/go.mod' | xargs -n1 dirname | sort -u)

# module_files DIR lists the files of the module rooted at DIR (relative to
# DIR), tracked or untracked but not ignored, without the files of modules
# nested below DIR.
module_files() {
    local dir=$1 prefix="" b
    [[ $dir == . ]] || prefix="$dir/"
    git ls-files -co --exclude-standard -- "${dir}" | while IFS= read -r f; do
        [[ -f $f ]] || continue
        for b in $boundaries; do
            [[ $b != "$dir" && $f == "$b/"* ]] && continue 2
        done
        printf '%s\n' "${f#"$prefix"}"
    done | sort
}

# The pseudo-version hashes every served file, path included.
hash=$(for dir in . "${nested[@]}"; do
    module_files "$dir" | while IFS= read -r f; do
        p=$f
        [[ $dir == . ]] || p="$dir/$f"
        printf '%s\0' "$p"
        cat "$p"
    done
done | shasum -a 256 | cut -c1-12)
version="v0.0.0-20000101000000-$hash"
echo "Serving the working tree as $version"

# rewrite_gomod FILE drops the go-atlas-net replace directives of FILE and
# requires every go-atlas-net module it names at $version.
rewrite_gomod() {
    local file=$1 path args=()
    for path in "$module" "${nested[@]/#/$module/}"; do
        args+=("-dropreplace=$path")
    done
    for path in $(grep -oE "(^|[[:space:]])$module(/[^[:space:]]+)?[[:space:]]+v[^[:space:]]+" "$file" | awk '{print $1}' | sort -u); do
        args+=("-require=$path@$version")
    done
    go mod edit "${args[@]}" "$file"
    if grep -qE '=>[[:space:]]*\.' "$file"; then
        echo "$file still replaces a module with a local directory" >&2
        exit 1
    fi
}

proxy="$work/proxy"
stage="$work/stage"

# serve DIR stages the module rooted at DIR and adds it to the proxy.
serve() {
    local dir=$1 path=$module
    [[ $dir == . ]] || path="$module/$dir"
    local root="$stage/$dir" content="$stage/$dir/$path@$version"
    mkdir -p "$content"
    module_files "$dir" | while IFS= read -r f; do
        mkdir -p "$content/$(dirname "$f")"
        cp "$dir/$f" "$content/$f"
    done
    rewrite_gomod "$content/go.mod"

    local at="$proxy/$path/@v"
    mkdir -p "$at"
    printf '%s\n' "$version" >"$at/list"
    printf '{"Version":"%s","Time":"2000-01-01T00:00:00Z"}\n' "$version" >"$at/$version.info"
    cp "$content/go.mod" "$at/$version.mod"
    (cd "$root" && zip -qrX -D "$at/$version.zip" "$path@$version")
}

for dir in . "${nested[@]}"; do
    serve "$dir"
done

upstream=$(go env GOPROXY)
export GOWORK=off
export GOFLAGS=-mod=mod
export GOPROXY="file://$proxy,$upstream"
export GONOPROXY=none
export GONOSUMDB="$module"
export GOSUMDB="${GOSUMDB:-$(go env GOSUMDB)}"

# accepted TAG reports whether NESTED_BUILD_ACCEPT accepts the breakage of
# TAG at the commit it points to.
accepted() {
    local tag=$1 commit entry
    commit=$(git rev-parse "$tag^{commit}")
    for entry in ${NESTED_BUILD_ACCEPT:-}; do
        [[ ${entry%@*} == "$tag" && -n ${entry#*@} && $commit == "${entry#*@}"* ]] && return 0
    done
    return 1
}

failed=""

for dir in "${nested[@]}"; do
    content="$stage/$dir/$module/$dir@$version"
    check="$work/check/$dir"
    mkdir -p "$check"
    cp -R "$content/." "$check/"
    chmod -R u+w "$check"
    echo "==> $dir (working tree) against the root at $version"
    (cd "$check" && go vet ./...) || failed="$failed $dir"
done

for dir in "${nested[@]}"; do
    tag=$(git tag -l "$dir/v*" --sort=-v:refname | head -n1)
    if [[ -z $tag ]]; then
        echo "==> $dir: no release tag, skipped"
        continue
    fi

    released="$work/released"
    mkdir -p "$released"
    git archive "$tag" -- "$dir" | tar -x -C "$released"
    rewrite_gomod "$released/$dir/go.mod"
    echo "==> $dir ($tag) against the root at $version"
    if ! (cd "$released/$dir" && go build ./...); then
        if accepted "$tag"; then
            echo "    $tag breakage accepted by NESTED_BUILD_ACCEPT"
        else
            failed="$failed $tag"
        fi
    fi
done

if [[ -n $failed ]]; then
    echo "nested modules failing against the root without replace:$failed" >&2
    exit 1
fi
echo "Every nested module builds against the root without replace."
