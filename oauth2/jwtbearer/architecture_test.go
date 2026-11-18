// Copyright 2021-2026 ALTESSA SOLUTIONS INC.
// Use of this source code is governed by the MIT license that can be found in
// the LICENSE file.

package jwtbearer_test

import (
	"go/build"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// Keep the shared packages importable below the application factories and adapters.
func TestSharedPackageDependencies(t *testing.T) {
	t.Parallel()

	for _, dir := range []string{".", "../../proxyconfig"} {
		t.Run(dir, func(t *testing.T) {
			t.Parallel()

			p, err := build.Default.ImportDir(dir, 0)
			require.NoError(t, err)

			for _, path := range p.Imports {
				switch {
				case strings.HasPrefix(path, "github.com/altessa-s/go-atlas-core/"),
					path == "github.com/altessa-s/go-atlas-net/urlx", path == "github.com/altessa-s/go-atlas-net/httpx":
				default:
					first, _, _ := strings.Cut(path, "/")
					require.NotContains(t, first, ".", "unexpected production dependency %s", path)
				}
			}
		})
	}
}
