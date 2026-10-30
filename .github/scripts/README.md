# GitHub Actions scripts

## Commit messages

`validate-commit-message.sh` reads a complete message from stdin. The local `commit-msg` hook and CI use this same validator. It checks
Conventional Commits (`type[(scope)][!]: description`), types, lowercase descriptions and scopes from [`commit_scopes.txt`](../../commit_scopes.txt).
The `root` scope is forbidden; omit the scope for repository-wide changes. Breaking changes may use `!` before the colon.

Messages must describe the concrete change without filler, test results, coverage figures or validation summaries. Agent attribution, generation
signatures and agent co-author trailers are forbidden. The validator rejects common report headings and attribution patterns; reviewers check
meaning and concision. Human co-author trailers and `BREAKING CHANGE:` explanations remain allowed.

`validate-commits.sh` validates the complete message of every commit in a range. All failures fail CI. The only exemption is the subject of a
merge git generates itself (`Merge branch|tag|remote-tracking branch '…'`, `Merge pull request #N from …`), as git flow merges releases and
hotfixes; its body is checked like any other. Automated and revert commits use a conventional subject. PR titles use the same validator.

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

Without a range, local runs compare against the merge base with `origin/develop`. CI supplies `BASE_SHA` and `HEAD_SHA` for the event. A new branch
push without a base checks all commits reachable from the head. Missing references and invalid messages fail the check; history is never fetched
implicitly or replaced with a shorter range.

## Examples

```text
fix(http/client): preserve token usage on provider errors
feat(urlx)!: reject recursive definitions
build: pin the options generator
```

Exit status is zero only when every message is valid (or the selected range is empty).
