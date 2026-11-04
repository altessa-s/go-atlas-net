SHELL          			:= /bin/bash -o pipefail
.DELETE_ON_ERROR:
MAKEFLAGS      			+= --warn-undefined-variables
MAKEFLAGS      			+= --no-builtin-rules
MODULE         			:= github.com/altessa-s/go-atlas-net
# Directories holding a go.mod, root first. Every Go target runs in each one.
MODULES        			:= .
# Sub-modules released with the root under the same version. Empty: no
# sub-module is released together with the root, so make release tags only
# VERSION.
NESTED_MODULES 			:=
# Sub-modules nested-build-check vets against the root without replace
# directives, together with the latest released <dir>/v* tag of each: every
# sub-module a consumer combines with a root release, so NESTED_MODULES and
# after them the sub-modules released on their own. Test-only modules stay out.
# Empty: no sub-module is consumed with a root release, so the check is a no-op.
NESTED_CHECK_MODULES 	:= $(NESTED_MODULES)
# make release VERSION=vX.Y.Z. VERSION is taken literally (its value is never
# expanded by make) and the release recipes read it from the environment,
# quoted, never spliced into shell source; a SemVer check
# (.github/scripts/check-release-version.sh) runs first.
VERSION 				?=
override VERSION 		:= $(value VERSION)
export VERSION

# Development tools are built from the versions pinned in devtools/go.mod into
# bin/ and run from there, so a different version on PATH never takes part.
# Recipes never splice external data (the checkout path, tags, branch names)
# into shell source: the paths reach them through the environment as
# REPO_ROOT and REPO_BIN and are quoted there; git data is read in the recipe.
BIN            			:= $(CURDIR)/bin
export REPO_ROOT		:= $(CURDIR)
export REPO_BIN			:= $(BIN)
GOLANGCI_LINT_PKG		:= github.com/golangci/golangci-lint/v2/cmd/golangci-lint
GOSEC_PKG      			:= github.com/securego/gosec/v2/cmd/gosec
GOVULNCHECK_PKG			:= golang.org/x/vuln/cmd/govulncheck
DUPL_PKG       			:= github.com/mibk/dupl
COPYRIGHT_PKG  			:= github.com/altessa-s/go-copyright-checker

# dupl configuration
# - DUPL_THRESHOLD: token threshold (higher -> fewer matches)
# - DUPL_PATH: scan root
# - DUPL_IGNORE_REGEX: paths to exclude from dupl output; we treat these as generated/noise.
DUPL_THRESHOLD ?= 130
DUPL_PATH ?= .
DUPL_IGNORE_REGEX ?= (_gen\.go|_string\.go|_test\.go)

# gosec rule classes excluded globally because they are by-design or redundant
# for a client library. Verified false positives in KEPT rules are suppressed
# individually via `// #nosec` so those rules still gate new code.
#   G104 - redundant with golangci-lint's errcheck (the project's error gate)
#   G115 - integer overflow on reviewed-safe numeric conversions
#   G117 - exported fields named like secrets (APIKey)
#   G702/G703/G704/G705 - experimental taint analysis (cmd-inject/path/SSRF/XSS):
#          flags the library's core function (outbound HTTP to configured
#          endpoints); provenance is the caller's job
GOSEC_EXCLUDED_RULES ?= G104,G115,G117,G702,G703,G704,G705

# Fuzzing configuration
# - FUZZTIME: per-target budget. `go test -fuzz` accepts exactly one target per
#   package per invocation, so `make fuzz` walks them and this is the cost of a
#   single target, not of the whole sweep.
# - FUZZPKG: restricts the sweep, e.g. FUZZPKG=./urlx
FUZZTIME ?= 30s
FUZZPKG ?= ./...
# The recipes read both from the environment, quoted, and never splice them
# into shell source; fuzz-inputs checks them against narrow allowlists first.
export FUZZTIME FUZZPKG

.PHONY: all
all: help

# bin/ is ignored by git, so a clean checkout has none; every tool target
# depends on it (order-only).
$(BIN):
	@mkdir -p "$$REPO_BIN"

# One target per tool binary, so make builds each tool once per run, also
# under -j, and every target using a tool depends on it. They are phony: go
# build is incremental, and rebuilding on every run keeps bin/ in step with
# devtools/go.mod and the Go toolchain without tracking either.
.PHONY: $(BIN)/golangci-lint $(BIN)/optgen $(BIN)/gosec $(BIN)/govulncheck $(BIN)/dupl $(BIN)/go-copyright-checker
$(BIN)/golangci-lint: | $(BIN)
	@go -C devtools build -o "$$REPO_BIN"/golangci-lint $(GOLANGCI_LINT_PKG)
$(BIN)/optgen: | $(BIN)
	@go -C devtools build -o "$$REPO_BIN"/optgen github.com/altessa-s/go-optgen/cmd/optgen
$(BIN)/gosec: | $(BIN)
	@go -C devtools build -o "$$REPO_BIN"/gosec $(GOSEC_PKG)
$(BIN)/govulncheck: | $(BIN)
	@go -C devtools build -o "$$REPO_BIN"/govulncheck $(GOVULNCHECK_PKG)
$(BIN)/dupl: | $(BIN)
	@go -C devtools build -o "$$REPO_BIN"/dupl $(DUPL_PKG)
$(BIN)/go-copyright-checker: | $(BIN)
	@go -C devtools build -o "$$REPO_BIN"/go-copyright-checker $(COPYRIGHT_PKG)

.PHONY: fmt
fmt: tidy $(BIN)/golangci-lint ## Format every module with the formatters of .golangci.yml (gci import order)
	@# ./... stops at module boundaries, so each module is formatted from its
	@# own directory; golangci-lint knows the standard library of the Go
	@# toolchain it was built with and skips testdata.
	@for m in $(MODULES); do (cd $$m && "$$REPO_BIN"/golangci-lint fmt ./...) || exit 1; done

.PHONY: fmt-check
fmt-check: $(BIN)/golangci-lint ## Fail when make fmt would rewrite a file
	@for m in $(MODULES); do \
	  out=$$(cd $$m && "$$REPO_BIN"/golangci-lint fmt --diff ./...) || { printf '%s\n' "$$out"; exit 1; }; \
	  if [ -n "$$out" ]; then printf '%s\n' "$$out"; echo "files in $$m are not formatted: run make fmt"; exit 1; fi; \
	done

.PHONY: lint
lint: tidy fmt ## Tidy, format and run the linter
	@$(MAKE) --no-print-directory golangci

.PHONY: golangci
golangci: $(BIN)/golangci-lint ## Run golangci-lint without rewriting files (as CI does)
	@for m in $(MODULES); do (cd $$m && "$$REPO_BIN"/golangci-lint run ./...) || exit 1; done

.PHONY: tidy
tidy: ## Run go mod tidy
	@for m in $(MODULES); do (cd $$m && go mod tidy) || exit 1; done

.PHONY: generate
generate: $(BIN)/optgen $(BIN)/golangci-lint ## Build the pinned optgen and golangci-lint into bin/, run go generate and format the output
	@for m in $(MODULES); do (cd $$m && PATH="$$REPO_BIN:$$PATH" go generate ./...) || exit 1; done
	@# optgen formats with gofmt -w; golangci-lint fmt then applies the gci
	@# import order of .golangci.yml to the generated files only (generated
	@# files are not excluded from the formatters there). File arguments must
	@# belong to one package, so it runs once per package directory of each
	@# module.
	@for m in $(MODULES); do \
	  (cd $$m && go list -f '{{.Dir}}' ./...) | while IFS= read -r d; do \
	    files=$$(cd "$$d" && find . -maxdepth 1 -type f -name '*_gen.go' | sort); \
	    if [ -n "$$files" ]; then (cd "$$d" && "$$REPO_BIN"/golangci-lint fmt $$files) || exit 1; fi; \
	  done || exit 1; \
	done

.PHONY: generate-check
# generate-check snapshots the generated files, runs make generate and
# compares: only what generate itself changes, adds or removes fails the
# check, not uncommitted edits made before it. A generated file already
# missing from the tree is not snapshotted and is ignored unless generate
# recreates it, which the check reports as added. In CI the snapshot is HEAD.
generate-check: ## Fail when make generate changes, adds or removes generated files (as CI does)
	@snap=$$(mktemp -d) || exit 1; trap 'rm -rf "$$snap"' EXIT; \
	git ls-files -co --exclude-standard -- '*_gen.go' | sort > "$$snap/before" || exit 1; \
	while IFS= read -r f; do \
	  [ -f "$$f" ] || continue; \
	  mkdir -p "$$snap/tree/$$(dirname "$$f")" && cp -p "$$f" "$$snap/tree/$$f" || exit 1; \
	done < "$$snap/before"; \
	$(MAKE) --no-print-directory generate || exit 1; \
	git ls-files -co --exclude-standard -- '*_gen.go' | sort > "$$snap/after" || exit 1; \
	stale=0; \
	while IFS= read -r f; do \
	  [ -f "$$f" ] || continue; \
	  if [ ! -f "$$snap/tree/$$f" ]; then echo "make generate added $$f"; stale=1; \
	  elif ! cmp -s "$$snap/tree/$$f" "$$f"; then diff -u "$$snap/tree/$$f" "$$f"; stale=1; fi; \
	done < "$$snap/after"; \
	while IFS= read -r f; do \
	  if [ -f "$$snap/tree/$$f" ] && [ ! -f "$$f" ]; then echo "make generate removed $$f"; stale=1; fi; \
	done < "$$snap/before"; \
	if [ $$stale -ne 0 ]; then echo "generated files are stale: run make generate and commit the result"; exit 1; fi

.PHONY: vet
vet: ## Run go vet and fail when a go fix modernizer would rewrite code (as CI does)
	@for m in $(MODULES); do (cd $$m && go vet ./...) || exit 1; done
	@for m in $(MODULES); do (cd $$m && go fix -diff ./...) || { echo "go fix -diff is not empty in $$m: run go fix ./..."; exit 1; }; done

.PHONY: build
build: ## Build all packages (honors GOOS/GOARCH)
	@for m in $(MODULES); do (cd $$m && go build ./...) || exit 1; done

.PHONY: test
test: ## Run all tests
	@for m in $(MODULES); do (cd $$m && go test ./...) || exit 1; done 2>&1 | awk -F'\t' '\
	  function pad(s,w, _l,_p) { gsub(/^[ \t]+|[ \t]+$$/, "", s); _l=length(s); _p=""; while(_l++<w) _p=_p" "; return s _p } \
	  function color(s,c) { return "\033[" c "m" s "\033[0m" } \
	  { \
	    st=$$1; gsub(/^[ \t]+|[ \t]+$$/, "", st); \
	    pkg=$$2; gsub(/^[ \t]+|[ \t]+$$/, "", pkg); sub(/github.com\/altessa-s\/go-atlas-net\/?/, "", pkg); \
	    if (st=="ok") cs="32"; else if (st=="FAIL") cs="31"; else cs="33"; \
	    printf "| %s | %s | %s |\n", color(pad(st,4),cs), pad(pkg,56), pad($$3,15) \
	  }'

.PHONY: test-verbose
test-verbose: ## Run all tests with verbose output
	@for m in $(MODULES); do (cd $$m && go test -v ./...) || exit 1; done

.PHONY: test-race
test-race: ## Run all tests with the race detector (slower)
	@for m in $(MODULES); do (cd $$m && go test -race ./...) || exit 1; done

.PHONY: test-shuffle
test-shuffle: ## Run all tests with randomized order (helps catch order-dependence)
	@for m in $(MODULES); do (cd $$m && go test -shuffle=on ./...) || exit 1; done

.PHONY: test-coverage
test-coverage: ## Run tests with coverage report
	@for m in $(MODULES); do (cd $$m && go test -cover ./...) || exit 1; done

.PHONY: test-coverage-func
test-coverage-func: ## Run tests and show formatted per-package coverage
	@for m in $(MODULES); do (cd $$m && go test -cover ./...) || exit 1; done 2>&1 | awk -F'\t' '\
	  function pad(s,w, _l,_p) { gsub(/^[ \t]+|[ \t]+$$/, "", s); _l=length(s); _p=""; while(_l++<w) _p=_p" "; return s _p } \
	  function color(s,c) { return "\033[" c "m" s "\033[0m" } \
	  { \
	    st=$$1; gsub(/^[ \t]+|[ \t]+$$/, "", st); \
	    pkg=$$2; gsub(/^[ \t]+|[ \t]+$$/, "", pkg); sub(/github.com\/altessa-s\/go-atlas-net\/?/, "", pkg); \
	    if (st=="ok") cs="32"; else if (st=="FAIL") cs="31"; else cs="33"; \
	    cov=$$4; gsub(/^[ \t]+|[ \t]+$$/, "", cov); \
	    match(cov, /[0-9]+\.[0-9]+/); pct=substr(cov,RSTART,RLENGTH)+0; \
	    if (cov=="") cc="0"; else if (pct>=80) cc="32"; else if (pct>=50) cc="33"; else cc="31"; \
	    printf "| %s | %s | %s | %s |\n", color(pad(st,4),cs), pad(pkg,56), pad($$3,15), color(pad(cov,30),cc) \
	  }'

.PHONY: coverage
coverage: ## Run tests with race detector and shuffle, writing the profile to coverage.out
	@echo "mode: atomic" > coverage.out
	@for m in $(MODULES); do \
	  (cd $$m && go test -race -shuffle=on -covermode=atomic -coverpkg=./... -coverprofile="$$REPO_ROOT/coverage.part" ./...) || { rm -f coverage.part; exit 1; }; \
	  if [ -f coverage.part ]; then tail -n +2 coverage.part >> coverage.out; rm -f coverage.part; fi; \
	done

# go tool cover cannot resolve packages of other modules from the root, so the
# total is computed from the profile: covered statements over all statements,
# printed in go tool cover's "total:" format. With -coverpkg every test binary
# reports every block of its module, so each block counts once and is covered
# when any test binary covered it.
.PHONY: coverage-total
coverage-total: ## Print the total statement coverage of coverage.out
	@awk 'NR > 1 { size[$$1] = $$2; if ($$3 > 0) hit[$$1] = 1 } \
	  END { for (b in size) { stmts += size[b]; if (b in hit) covered += size[b] } \
	        if (stmts == 0) { print "coverage-total: no statements in coverage.out" > "/dev/stderr"; exit 1 } \
	        printf "total:\t(statements)\t%.1f%%\n", covered * 100 / stmts }' coverage.out

.PHONY: test-all
test-all: coverage ## Run tests with race detector, shuffle, and coverage
	@$(MAKE) --no-print-directory coverage-total
	@rm -f coverage.out

.PHONY: test-integration
test-integration: ## Run unit and integration tests (-tags integration) with race detector and shuffle
	@for m in $(MODULES); do (cd $$m && go test -race -shuffle=on -tags=integration ./...) || exit 1; done

.PHONY: bench
bench: ## Run all benchmarks
	@for m in $(MODULES); do (cd $$m && go test -bench=. -benchmem -run=^$$ ./...) || exit 1; done 2>&1 | awk -F'\t' '\
	  function pad(s,w, _l,_p) { gsub(/^[ \t]+|[ \t]+$$/, "", s); _l=length(s); _p=""; while(_l++<w) _p=_p" "; return s _p } \
	  function color(s,c) { return "\033[" c "m" s "\033[0m" } \
	  /^(ok|FAIL|\?)/ { \
	    st=$$1; gsub(/^[ \t]+|[ \t]+$$/, "", st); \
	    pkg=$$2; gsub(/^[ \t]+|[ \t]+$$/, "", pkg); sub(/github.com\/altessa-s\/go-atlas-net\/?/, "", pkg); \
	    if (st=="ok") cs="32"; else if (st=="FAIL") cs="31"; else cs="33"; \
	    printf "| %s | %s | %s |\n", color(pad(st,4),cs), pad(pkg,56), pad($$3,15); next \
	  } \
	  { print }'

.PHONY: bench-count
bench-count: ## Run benchmarks multiple times for benchstat (COUNT=5)
	@for m in $(MODULES); do (cd $$m && go test -bench=. -benchmem -run=^$$ -count=$${COUNT:-5} ./...) || exit 1; done | tee bench.txt
	@echo "Results saved to bench.txt (use benchstat bench.txt)"

.PHONY: bench-compare
bench-compare: ## Compare benchmarks with BASE (default origin/develop); fails on regressions above BENCH_THRESHOLD%
	@bash .github/scripts/bench-regression.sh $${BASE:-origin/develop}

.PHONY: fuzz-inputs
fuzz-inputs: # Refuse FUZZTIME and FUZZPKG outside their allowlists
	@[[ "$$FUZZTIME" =~ ^[0-9]{1,6}(ms|s|m|h|x)$$ ]] || { printf 'invalid FUZZTIME %q (a count of ms, s, m, h or x)\n' "$$FUZZTIME" >&2; exit 2; }
	@[[ "$$FUZZPKG" =~ ^\./[A-Za-z0-9_./-]*$$ ]] || { printf 'invalid FUZZPKG %q (one ./ package pattern)\n' "$$FUZZPKG" >&2; exit 2; }

# fuzz-dirs sets $$fdirs to the package directories of every module that
# FUZZPKG selects. FUZZPKG is a path relative to the repository root; a
# trailing /... selects the directory and everything below it, across module
# boundaries. Packages run from their own directory, so each uses its module.
define fuzz-dirs
want="$${FUZZPKG#./}"; recursive=0; \
case "$$want" in ...) recursive=1; want=.;; */...) recursive=1; want="$${want%/...}";; esac; \
want=$$(cd "$$REPO_ROOT/$${want:-.}" 2>/dev/null && pwd) || { printf '\033[31mFUZZPKG %s names no directory\033[0m\n' "$$FUZZPKG" >&2; exit 2; }; \
fdirs=""; \
for m in $(MODULES); do \
  dirs=$$(cd $$m && go list -f '{{.Dir}}' ./...) || { printf '\033[31mcannot list the packages of %s\033[0m\n' "$$m" >&2; exit 1; }; \
  for d in $$dirs; do \
    if [ "$$d" = "$$want" ]; then fdirs="$$fdirs $$d"; \
    elif [ $$recursive = 1 ]; then case "$$d/" in "$$want"/*) fdirs="$$fdirs $$d";; esac; fi; \
  done; \
done; \
[ -n "$$fdirs" ] || { printf '\033[31mFUZZPKG %s matches no package\033[0m\n' "$$FUZZPKG" >&2; exit 2; }
endef

.PHONY: fuzz-list
fuzz-list: fuzz-inputs ## List every fuzz target (package and name)
	@$(fuzz-dirs); \
	for d in $$fdirs; do \
	  list=$$(cd "$$d" && go test -list '^Fuzz' .) || exit 1; \
	  rel="$${d#"$$REPO_ROOT"}"; rel="$${rel#/}"; rel="$${rel:-.}"; \
	  for t in $$(printf '%s\n' "$$list" | grep '^Fuzz' || true); do \
	    printf '%s\t%s\n' "$$rel" "$$t"; \
	  done; \
	done

.PHONY: fuzz-seed
fuzz-seed: fuzz-inputs ## Run every fuzz target over its seed corpus only (no mutation, fast)
	@$(fuzz-dirs); \
	for d in $$fdirs; do (cd "$$d" && go test -run '^Fuzz' .) || exit 1; done

.PHONY: fuzz
fuzz: fuzz-inputs ## Fuzz every target for FUZZTIME each (FUZZTIME=30s FUZZPKG=./...)
	@$(fuzz-dirs); \
	failed=""; \
	for d in $$fdirs; do \
	  rel="$${d#"$$REPO_ROOT"}"; rel="$${rel#/}"; rel="$${rel:-.}"; \
	  if ! list=$$(cd "$$d" && go test -list '^Fuzz' .); then \
	    failed="$$failed $$rel:(target discovery)"; \
	    continue; \
	  fi; \
	  for t in $$(printf '%s\n' "$$list" | grep '^Fuzz' || true); do \
	    printf '\033[36m==> %s %s (%s)\033[0m\n' "$$rel" "$$t" "$$FUZZTIME"; \
	    (cd "$$d" && go test -run '^$$' -fuzz "^$$t$$" -fuzztime "$$FUZZTIME" .) || failed="$$failed $$rel:$$t"; \
	  done; \
	done; \
	if [ -n "$$failed" ]; then \
	  printf '\033[31mfailing fuzz targets:%s\033[0m\n' "$$failed"; \
	  printf 'minimized reproducers were written to the package testdata/fuzz directories; commit them as regression seeds\n'; \
	  exit 1; \
	fi

# dupl-scan drops the ignored files before dupl sees them (filtering its
# output instead would hide a clone group whose printed pairs all involve an
# ignored file), then scans the rest. Listing the files or scanning them
# failing fails the recipe (exit 2); dupl reports a file it cannot read or
# parse on stderr and still exits 0, so any diagnostic is a failed scan too.
# A filter keeping no file (grep status 1) is a clean result, only grep
# statuses above 1 are errors.
define dupl-scan
files="$$(find "$(DUPL_PATH)" -type f -name '*.go')" || { echo "cannot list the Go files of $(DUPL_PATH)" >&2; exit 2; }; \
kept="$$(printf '%s\n' "$$files" | grep -Ev "$(DUPL_IGNORE_REGEX)")"; status=$$?; \
if [ "$$status" -gt 1 ]; then echo "dupl file filter failed" >&2; exit 2; fi; \
out=""; \
if [ -n "$$kept" ]; then \
  diag="$$(mktemp)" || exit 2; \
  out="$$(printf '%s\n' "$$kept" | "$$REPO_BIN"/dupl -files -t "$(DUPL_THRESHOLD)" -plumbing 2>"$$diag")" || { cat "$$diag" >&2; rm -f "$$diag"; echo "dupl failed" >&2; exit 2; }; \
  if [ -s "$$diag" ]; then cat "$$diag" >&2; rm -f "$$diag"; echo "dupl could not scan every file" >&2; exit 2; fi; \
  rm -f "$$diag"; \
fi
endef

.PHONY: dupl
dupl: $(BIN)/dupl ## Run dupl (ignoring generated files)
	@$(dupl-scan); \
	if [ -n "$$out" ]; then printf '%s\n' "$$out"; fi

.PHONY: dupl-check
dupl-check: $(BIN)/dupl ## Fail if dupl finds duplicates (ignoring generated files)
	@$(dupl-scan); \
	if [ -n "$$out" ]; then \
		printf '%s\n' "$$out"; \
		exit 1; \
	fi

.PHONY: ci
# ci runs its steps one after the other even under make -j: lint rewrites
# files that fmt-check and generate-check read, so they must not overlap. Each
# step may still use -j inside.
CI_STEPS := lint fmt-check generate-check test-all test-integration security-scan dupl-check nested-build-check
ci: ## Run full CI checks locally, one step after the other (lint, generated files, test, security, dupl, nested modules)
	@for step in $(CI_STEPS); do $(MAKE) --no-print-directory $$step || exit 1; done

.PHONY: clean
clean: ## Remove generated artifacts (coverage.out, bench.txt)
	@rm -f coverage.out bench.txt

.PHONY: copyright
copyright: $(BIN)/go-copyright-checker ## Add copyright header to all files
	@echo "Updating copyright header to all go files"
	"$$REPO_BIN"/go-copyright-checker check --fix .

.PHONY: print-nested-modules
print-nested-modules: ## Print the nested module directories released with the root, one per line
	@printf '%s\n' $(NESTED_MODULES)

.PHONY: print-nested-check-modules
print-nested-check-modules: ## Print the sub-module directories nested-build-check builds against the root, one per line
	@printf '%s\n' $(NESTED_CHECK_MODULES)

.PHONY: nested-build-check
nested-build-check: ## Build every sub-module of NESTED_CHECK_MODULES against the root without replace directives; a no-op when it is empty (see .github/scripts/nested-build-check.sh)
	@bash .github/scripts/nested-build-check.sh

# release-preflight refuses a release from anything but a clean main that
# equals origin/main after a fetch, so the tags land on the reviewed history.
define release-preflight
branch=$$(git symbolic-ref --quiet --short HEAD) || branch="(detached HEAD)"; \
if [ "$$branch" != main ]; then echo "make release runs only on main (current: $$branch)"; exit 1; fi; \
if [ -n "$$(git status --porcelain)" ]; then echo "working tree is not clean"; exit 1; fi; \
pushurls=$$(git remote get-url --push --all origin) || { echo "origin has no push URL"; exit 1; }; \
if [ "$$(printf '%s\n' "$$pushurls" | wc -l)" -ne 1 ]; then echo "origin has several push URLs; make release needs exactly one to verify the pushed tags"; exit 1; fi; \
git fetch -q origin +refs/heads/main:refs/remotes/origin/main || { echo "cannot fetch origin/main"; exit 1; }; \
if [ "$$(git rev-parse HEAD)" != "$$(git rev-parse refs/remotes/origin/main)" ]; then \
	echo "HEAD ($$(git rev-parse --short HEAD)) is not origin/main ($$(git rev-parse --short refs/remotes/origin/main)): pull or push main first"; exit 1; \
fi
endef

# release-pushed runs after a failed push of the tags in $$tags and sets
# pushed to yes when origin has every tag as the same object as the local
# tag (the push landed and only its response was lost), no when origin has
# none of them as the local object (the push did not land), and unknown when
# origin cannot be queried or holds only some of them.
define release-pushed
pushed=unknown; pushurl=$$(git remote get-url --push origin); \
if remote=$$(git ls-remote --tags "$$pushurl" 2>/dev/null); then \
	same=0; for t in "$${tags[@]}"; do \
		local_obj=$$(git rev-parse -q --verify "refs/tags/$$t") || continue; \
		remote_obj=$$(printf '%s\n' "$$remote" | awk -v r="refs/tags/$$t" '$$2 == r {print $$1}'); \
		if [ "$$remote_obj" = "$$local_obj" ]; then same=$$((same + 1)); fi; \
	done; \
	if [ $$same -eq $${#tags[@]} ]; then pushed=yes; elif [ $$same -eq 0 ]; then pushed=no; fi; \
fi
endef

# release-tags-free fails when a tag of $$tags (a bash array of tag names)
# exists locally or on origin, before the release changes anything. Both
# macros query origin at its push URL (pushurl), where the release tags are
# pushed; release-preflight requires origin to have exactly one. The URL is
# never printed, since it may carry credentials; the diagnostics of git
# ls-remote, which can quote it, are discarded.
define release-tags-free
pushurl=$$(git remote get-url --push origin); \
remote=$$(git ls-remote --tags "$$pushurl" 2>/dev/null) || { echo "cannot list the tags at the push URL of origin"; exit 1; }; \
for t in "$${tags[@]}"; do \
	if printf '%s\n' "$$remote" | awk '{print $$2}' | grep -qxF -e "refs/tags/$$t" -e "refs/tags/$$t^{}"; then \
		echo "tag $$t already exists on origin"; exit 1; \
	fi; \
	if git rev-parse -q --verify "refs/tags/$$t" >/dev/null; then echo "tag $$t already exists locally (git tag -d $$t if it is stale)"; exit 1; fi; \
done
endef

.PHONY: release
release: ## Semantic versioning - create and push a new release tag (clean main equal to origin/main only, one push URL for origin)
	@bash .github/scripts/check-release-version.sh "$$VERSION"
	@$(release-preflight)
	@tags=("$$VERSION"); $(release-tags-free)
	@echo "Creating release $$VERSION..."
	@git tag -a "$$VERSION" -m "Release $$VERSION"
	@tags=("$$VERSION"); git push origin "refs/tags/$$VERSION" || { \
		$(release-pushed); \
		case $$pushed in \
		yes) echo "git push failed, but origin has the local $$VERSION: the release was pushed; the local tag is kept" >&2; exit 1 ;; \
		no) git tag -d "$$VERSION" >/dev/null; echo "push failed and origin has no local $$VERSION; deleted the local tag $$VERSION" >&2; exit 1 ;; \
		*) echo "git push failed and origin could not be verified; the local tag $$VERSION is kept: compare "'git ls-remote --tags "$$(git remote get-url --push origin)"'" with it before retrying" >&2; exit 1 ;; \
		esac; \
	}
	@echo "Release $$VERSION created and pushed"

.PHONY: gosec
gosec: $(BIN)/gosec ## Run gosec static security scanner (see GOSEC_EXCLUDED_RULES)
	@for m in $(MODULES); do (cd $$m && "$$REPO_BIN"/gosec -quiet -exclude-generated -exclude=$(GOSEC_EXCLUDED_RULES) ./...) || exit 1; done

.PHONY: govulncheck
govulncheck: $(BIN)/govulncheck ## Run govulncheck
	@for m in $(MODULES); do (cd $$m && "$$REPO_BIN"/govulncheck ./...) || exit 1; done

.PHONY: security-scan
security-scan: $(BIN)/govulncheck $(BIN)/gosec ## Security checks - run vulnerability and security scanners
	@# Run every scanner even when an earlier one reports findings, then fail
	@# if any did, so one report never hides the other.
	@status=0; \
	echo "Running Go vulnerability check ($$(go env GOVERSION))..."; \
	for m in $(MODULES); do (cd $$m && "$$REPO_BIN"/govulncheck ./...) || status=1; done; \
	echo ""; \
	echo "Running gosec security scanner..."; \
	for m in $(MODULES); do (cd $$m && "$$REPO_BIN"/gosec -quiet -exclude-generated -exclude=$(GOSEC_EXCLUDED_RULES) ./...) || status=1; done; \
	echo ""; \
	if [ $$status -ne 0 ]; then echo "Security scan found issues"; exit 1; fi; \
	echo "Security scan completed"

# Absolutely awesome: http://marmelab.com/blog/2016/02/29/auto-documented-makefile.html
help: ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "\033[36m%-30s\033[0m %s\n", $$1, $$2}'
