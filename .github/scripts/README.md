# GitHub Actions scripts

## Commit messages

`validate-commit-message.sh` reads a complete message from stdin. The local `commit-msg` hook and CI use this same validator. It checks
Conventional Commits (`type[(scope)][!]: description`), types, lowercase descriptions and scopes from [`commit_scopes.txt`](../../commit_scopes.txt).
Only the scope lines of that file count; comment and blank lines name no scope. The `root` scope is forbidden; omit the scope for repository-wide
changes. Breaking changes use `!` before the colon; a `BREAKING CHANGE:` footer is rejected without it, because release notes group
breaking changes by the subject; a merge commit, whose subject cannot carry the `!`, may not have one.

Messages must describe the concrete change without filler, test results, coverage figures or validation summaries. Agent attribution, generation
signatures and agent co-author trailers are forbidden. The validator rejects common report headings and attribution patterns, matching agent names
as whole words only; reviewers check meaning and concision. Human co-author trailers and `BREAKING CHANGE:` explanations remain allowed.

`validate-commits.sh` validates the complete message of every commit in a range. All failures fail CI. The only exemption is the subject of a
merge git generates itself (`Merge branch|tag|remote-tracking branch '…'`, `Merge pull request #N from …`), as git flow merges releases and
hotfixes; its body is checked like any other. Automated and revert commits use a conventional subject: Dependabot writes `build(deps): bump …`
(see [`dependabot.yml`](../dependabot.yml)), which the `deps` scope covers, so it needs no exemption. PR titles use the same validator.

## Usage

```sh
# Enable the local hook.
git config core.hooksPath .githooks

# Exercise validation rules and the local hook without creating commits.
bash .github/scripts/test-commit-validation.sh

# Validate one message.
bash .github/scripts/validate-commit-message.sh < /tmp/commit-message

# Validate a specific range.
bash .github/scripts/validate-commits.sh origin/develop..HEAD
```

Without a range, local runs compare against the merge base with `origin/develop`. CI supplies `BASE_SHA` and `HEAD_SHA` for the event. A push without
a usable base (a new branch, or a force push whose `BASE_SHA` is no longer a commit of the clone) checks all commits reachable from the head. A pull
request whose `BASE_SHA` is not a commit of the clone compares against its base branch (`origin/$GITHUB_BASE_REF`) and fails when that branch is
unknown; it never falls back to `origin/develop`. Outside CI, a `BASE_SHA` that is not a commit fails the check. Missing references and invalid
messages fail the check; history is never fetched implicitly or replaced with a shorter range. `test-commit-validation.sh` also exercises these
range rules in a scratch repository.

## Examples

```text
fix(http/client): keep the retry budget per request
feat(urlx): reject base URLs with a fragment
build(deps): bump golang.org/x/sys from 0.48.0 to 0.49.0
build: pin the options generator
```

Exit status is zero only when every message is valid (or the selected range is empty).

## Benchmark regressions

`bench-regression.sh BASE_REF` checks the base out into a temporary worktree, runs every benchmark of both trees `BENCH_COUNT` times with
`-benchtime` `BENCH_TIME` and compares them with the `benchstat` pinned in [`devtools`](../../devtools). It fails when a benchmark shows a
statistically significant increase in `sec/op` or `allocs/op` above `BENCH_THRESHOLD` percent; changes benchstat marks as insignificant (`~`) and
benchmarks present on one side only never fail. `BENCH_REPORT` names a file that also receives the report. The defaults of these variables are set
and documented in the script's header. The [`bench.yml`](../workflows/bench.yml) workflow runs it on pull requests against the base branch; locally
use `make bench-compare BASE=<ref>`.

## Release versions

`check-release-version.sh VERSION [MODULE_PATH]` accepts only SemVer 2.0 without build metadata, as Go module versions require:
`vMAJOR.MINOR.PATCH` without leading zeros and an optional prerelease of non-empty dot-separated identifiers (numeric ones without leading zeros).
It also applies Go's major version rule to `MODULE_PATH` (by default the root `go.mod` module path): a path without a `/vN` suffix takes only
`v0` and `v1`, a path ending in `/vN` only `vN`. `make release` runs it first and passes `VERSION` through the environment, never through shell
source. `test-release-version.sh` exercises it with accepted (`v1.2.3`, `v1.2.3-rc.1`, `v1.0.0-0.3.7`, `v2.0.0` for `…/v2`) and rejected
(`v1.2.3-..`, `v1.2.3-`, `v01.2.3`, `v1.2.3-01`, `v1.2.3+build.5`, `1.2.3`, `v1.2`, `v2.0.0` without `/v2`, `v1.0.0` for `…/v2`, shell
metacharacters) versions, and with the root `go.mod`; the `lint` job of [`ci.yml`](../workflows/ci.yml) runs it.

## Nested modules

`nested-build-check.sh` builds every sub-module of the Makefile's `NESTED_CHECK_MODULES` the way a consumer's module graph combines it with the root
module, without the `replace` directives of the sub-module `go.mod` files. `NESTED_CHECK_MODULES` holds every sub-module a consumer combines with a
root release: the nested modules released together with the root (the Makefile defines it as `$(NESTED_MODULES)`, so a new nested module is checked
too) and after them the sub-modules released on their own; test-only modules stay out. The script serves the root module and every listed sub-module
from the working tree through a local `file://` `GOPROXY` at one pseudo-version hashed from the served files, rewrites every go-atlas-net requirement to
it, then vets each sub-module against the served root: `go vet` compiles every package together with its test files, but the check runs no test (the
test targets do). It also builds (`go build`, no tests) the latest released tag of each listed sub-module (`<dir>/v*`) against the served root, so a
root change that breaks a released sub-module fails the check. The Makefile variable `NESTED_BUILD_ACCEPT` lists released tags whose breakage is
accepted, pinned to the tagged commit as `<tag>@<commit>`. Sub-modules must not import the root module's `internal` packages, which a consumer's
newer root release may change.

`NESTED_CHECK_MODULES` is empty in this repository, so the Makefile defines no `NESTED_BUILD_ACCEPT`.

`make ci` and the `lint` job of [`ci.yml`](../workflows/ci.yml) run it through `make nested-build-check` in every repository; with an empty
`NESTED_CHECK_MODULES` it prints "no nested modules" and exits successfully.
No sub-module of this repository is consumed with a root release, so the check has nothing to build.
