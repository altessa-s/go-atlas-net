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
// golangci-lint, gci, optgen, gosec, govulncheck,
// dupl, benchstat and the copyright checker.
// Their versions are the ones this module's go.mod requires.
//
// # Usage
//
// Make targets build or install a tool from this module before running it.
// make generate builds optgen and gci into bin/, and make fmt builds gci
// there; both run them from bin/, so a different version on PATH never takes
// part:
//
//	go -C devtools build -o "$PWD/bin/gci" github.com/daixiang0/gci
//
// The other targets install their tool from this module:
//
//	go -C devtools install github.com/golangci/golangci-lint/v2/cmd/golangci-lint
//	golangci-lint run ./...
//
// Upgrade a tool with go get in this directory, then go mod tidy.
package devtools
