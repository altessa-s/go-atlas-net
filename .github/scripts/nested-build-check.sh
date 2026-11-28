#!/bin/bash
# Vet the working-tree nested modules of go-atlas-net and build their released tags.
# The staged pass uses the root and sibling module zips, with their module
# boundaries intact. The declared pass checks the published dependency graph
# of a module with a direct root requirement. An MVS upgrade fails the check:
# the graph cannot demonstrate that the nested module's own code builds with
# the declared root. Direct sibling versions are checked for publication,
# and MVS selects the versions used in the build. Publication is inferred
# from local tags and commits. GOPROXY=off skips the declared pass.
#
# Go ignores replacements in dependencies' manifests. Both passes discard
# remote replacements, including third-party forks, to match that behavior.
# Local replacements are allowed only for the root and checked nested modules.
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

cd "$(git rev-parse --show-toplevel)"
module=github.com/altessa-s/go-atlas-net
accept_indent="    "

read -r -a nested <<<"$(make --no-print-directory -s print-nested-check-modules | tr '\n' ' ')"
if [[ ${#nested[@]} -eq 0 ]]; then
    echo "no nested modules"
    exit 0
fi

work=$(mktemp -d)
trap 'chmod -R u+w "$work" 2>/dev/null || true; rm -rf "$work"' EXIT

# go mod edit -fmt renders both single-line directives and blocks. Extract
# require and replace arguments, including whether a replacement is local.
manifest_entries() {
    go mod edit -fmt -print "$1" | awk '
        function entry(kind, first,    path, version, rhs) {
            path = $first
            version = $(first + 1)
            gsub(/"/, "", path)
            if (kind == "replace") {
                rhs = $(first + (version == "=>" ? 2 : 3))
                gsub(/"/, "", rhs)
                if (rhs ~ /^(\.\.?($|[\/\\])|[\/\\]|[[:alpha:]]:)/) {
                    print kind, path, version, "local"
                    return
                }
            }
            print kind, path, version
        }
        /^[[:space:]]*\)/ { block = ""; next }
        $1 == "require" || $1 == "replace" {
            if ($2 == "(") { block = $1; next }
            entry($1, 2)
            next
        }
        block && NF && $1 !~ /^\/\// { entry(block, 1) }
    '
}

drop_replacements() {
    local file=$1 mode=${2:-drop} entries kind path old target owned args=()
    entries=$(manifest_entries "$file") || return 1
    while read -r kind path old target; do
        [[ $kind == replace ]] || continue
        if [[ $target == local ]]; then
            for owned in "$module" "${nested[@]/#/$module/}"; do
                [[ $path != "$owned" ]] || continue 2
            done
            echo "$file still replaces a module with a local directory" >&2
            return 1
        fi
    done <<<"$entries"
    [[ $mode != check ]] || return 0

    while read -r kind path old target; do
        [[ $kind == replace ]] || continue
        [[ $old != '=>' ]] || old=""
        args+=("-dropreplace=$path${old:+@$old}")
    done <<<"$entries"
    if [[ ${#args[@]} -gt 0 ]]; then
        go mod edit "${args[@]}" "$file"
    fi
}

# A path-wide replacement applies at every version MVS could select.
# The RHS is a version served by the file proxy, so Go still loads module
# zips and their manifests through the usual download path.
pin_staged() {
    local file=$1 self=$2 path args=()
    drop_replacements "$file" || return 1
    for path in "$module" "${nested[@]/#/$module/}"; do
        [[ $path != "$self" ]] || continue
        args+=("-replace=$path=$path@$version")
    done
    go mod edit "${args[@]}" "$file"
}

# Save the published dependency environment before enabling the staged proxy.
# In the release tooling branch the staged cache is separate and may contain
# artifacts served at a release version.
published_proxy=$(go env GOPROXY)
published_cache=$(go env GOMODCACHE)
published_private=$(go env GOPRIVATE)
published_noproxy=$(go env GONOPROXY)
published_nosumdb=$(go env GONOSUMDB)

has_published_ref() {
    local path=${1%@*} required=${1##*@} prefix=""
    if [[ $required =~ ^v[0-9]+\.(0\.0-|[0-9]+\.[0-9]+-([^+]*\.)?0\.)[0-9]{14}-([0-9a-f]{12})(\+incompatible)?$ ]]; then
        git rev-parse --verify --quiet "${BASH_REMATCH[3]}^{commit}" >/dev/null
        return
    fi
    [[ $path == "$module" ]] || prefix="${path#"$module/"}/"
    git rev-parse --verify --quiet "refs/tags/$prefix${required%+incompatible}^{commit}" >/dev/null
}

manifest_requirements() {
    manifest_entries "$1" | awk '$1 == "require" { print $2, $3 }' | LC_ALL=C sort
}

check_declared() (
    local dir=$1 label=$2 check=$3 original=$4 action=$5
    local manifest="$work/declared.mod" entries kind path required root_version=""
    local before after selected graph parent
    local requirements=()

    [[ $published_proxy != off ]] || return 0
    export GOPROXY="$published_proxy" GOMODCACHE="$published_cache" GOWORK=off
    export GOPRIVATE="$published_private" GONOPROXY="$published_noproxy" GONOSUMDB="$published_nosumdb"
    export GOFLAGS=-mod=mod
    cp "$original" "$manifest" || return 1
    rm -f "$work/declared.sum" || return 1
    if [[ -f ${original%.mod}.sum ]]; then
        cp "${original%.mod}.sum" "$work/declared.sum" || return 1
    fi
    entries=$(manifest_entries "$manifest") || return 1
    while read -r kind path required _; do
        [[ $kind == require ]] || continue
        if [[ $path == "$module" ]]; then
            root_version=$required
        fi
        if [[ $path == "$module" || $path == "$module/"* ]]; then
            requirements+=("$path@$required")
        fi
    done <<<"$entries"
    if [[ -z $root_version ]]; then
        echo "    $dir ($label): no declared root requirement, skipped"
        return 0
    fi

    echo "==> $dir ($label, declared requirements) against the root at $root_version"
    for required in "${requirements[@]}"; do
        if ! has_published_ref "$required"; then
            echo "    $required is not published, skipped"
            return 0
        fi
    done
    for required in "${requirements[@]}"; do
        (cd "$work" && go mod download "$required") || return 1
    done

    drop_replacements "$manifest" || return 1
    before=$(manifest_requirements "$manifest") || return 1
    cd "$check" || return 1
    # The main module's edges in go mod graph contain the versions selected
    # by MVS. This needs manifests, without fetching every module's metadata.
    graph=$(go mod graph -modfile="$manifest") || return 1
    selected=$(awk -v main="$module/$dir" -v target="$module@" '
        $1 == main && index($2, target) == 1 { print substr($2, length(target) + 1); exit }
    ' <<<"$graph")
    if [[ $selected != "$root_version" ]]; then
        parent=$(awk -v target="$module@$selected" '$1 ~ /@/ && $2 == target { print $1; exit }' <<<"$graph")
        echo "    $dir ($label): declared $root_version, graph selects $selected${parent:+ through $parent}" >&2
        return 1
    fi

    # Go can fill in checksums in the copy. A change to its requirements
    # means the original manifest did not describe the build that ran.
    go "$action" -modfile="$manifest" ./... || return 1
    after=$(manifest_requirements "$manifest") || return 1
    if [[ $before != "$after" ]]; then
        echo "    $dir ($label): build requires changes to the declared go.mod requirements" >&2
        return 1
    fi
)

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

# Prepare the manifest included in the proxy artifact.
rewrite_gomod() {
    local file=$1 entries kind path required args=()
    drop_replacements "$file"
    entries=$(manifest_entries "$file")
    while read -r kind path required _; do
        [[ $kind == require ]] || continue
        if [[ $path == "$module" || $path == "$module/"* ]]; then
            args+=("-require=$path@$version")
        fi
    done <<<"$entries"
    if [[ ${#args[@]} -gt 0 ]]; then
        go mod edit "${args[@]}" "$file"
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

export GOWORK=off
export GOFLAGS=-mod=mod
export GOPROXY="file://$proxy,$published_proxy"
export GONOPROXY=none
export GONOSUMDB="$module${published_nosumdb:+,$published_nosumdb}"
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

selected_tags() {
    git tag -l "$1/v*" --sort=-v:refname | awk 'NR == 1'
}

if [[ $published_proxy == off ]]; then
    echo "Declared requirements check skipped because GOPROXY=off."
fi
failed=""
for dir in "${nested[@]}"; do
    content="$stage/$dir/$module/$dir@$version"
    check="$work/check/$dir"
    mkdir -p "$check"
    cp -R "$content/." "$check/"
    chmod -R u+w "$check"
    pin_staged "$check/go.mod" "$module/$dir"
    echo "==> $dir (working tree) against the root at $version"
    (cd "$check" && go vet ./...) || failed="$failed $dir"
    check_declared "$dir" "working tree" "$check" "$dir/go.mod" vet ||
        failed="$failed $dir (declared requirements)"

    tags=$(selected_tags "$dir")
    if [[ -z $tags ]]; then
        echo "==> $dir: no release tag, skipped"
        continue
    fi

    while IFS= read -r tag; do
        released="$work/released/$tag"
        mkdir -p "$released"
        git archive "$tag" -- "$dir" | tar -x -C "$released"
        cp "$released/$dir/go.mod" "$work/original.mod"
        rm -f "$work/original.sum"
        if [[ -f $released/$dir/go.sum ]]; then
            cp "$released/$dir/go.sum" "$work/original.sum"
        fi
        pin_staged "$released/$dir/go.mod" "$module/$dir"
        echo "==> $dir ($tag) against the root at $version"
        tag_failed=""
        (cd "$released/$dir" && go build ./...) || tag_failed=" $tag"
        check_declared "$dir" "$tag" "$released/$dir" "$work/original.mod" build ||
            tag_failed="$tag_failed $tag (declared requirements)"
        if [[ -n $tag_failed ]]; then
            if accepted "$tag"; then
                echo "${accept_indent}$tag breakage accepted by NESTED_BUILD_ACCEPT"
            else
                failed="$failed$tag_failed"
            fi
        fi
    done <<<"$tags"
done

if [[ -n $failed ]]; then
    echo "nested modules failing against the root without replace:$failed" >&2
    exit 1
fi
echo "Every nested module builds against the root without replace."
