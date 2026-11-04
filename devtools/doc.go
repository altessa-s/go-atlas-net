// Copyright 2021-2026 ALTESSA SOLUTIONS INC.
// Use of this source code is governed by the MIT license that can be found in
// the LICENSE file.

// Package devtools pins the development tools of go-atlas-net in a module of
// their own, so their dependencies never reach the main go.mod. No code
// imports it.
//
// # Pinned tools
//
// tools.go (build tag tools) blank-imports the commands the Makefile runs:
// golangci-lint, optgen, gosec, govulncheck, dupl, benchstat and the copyright
// checker.
// Their versions are the ones this module's go.mod requires.
//
// # Usage
//
// Every pinned tool is built from this module and run from the built binary,
// so a different version on PATH never takes part. The make targets build
// theirs into bin/; bench-compare builds benchstat into a temporary directory.
// For example, what make golangci does:
//
//	go -C devtools build -o "$PWD/bin/golangci-lint" github.com/golangci/golangci-lint/v2/cmd/golangci-lint
//	bin/golangci-lint run ./...
//
// Upgrade a tool with go get in this directory, then go mod tidy.
package devtools
