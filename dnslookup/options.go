// Copyright 2021-2026 ALTESSA SOLUTIONS INC.
// Use of this source code is governed by the MIT license that can be found in
// the LICENSE file.

package dnslookup

//go:generate optgen generate --type=options

import (
	"net"
	"time"

	httpclient "github.com/altessa-s/go-atlas-net/http/client"
)

const (
	// DefaultMaxTimeout leaves the deadline to the caller's context.
	DefaultMaxTimeout time.Duration = 0
	// DefaultMaxConcurrent leaves the number of concurrent queries unlimited.
	DefaultMaxConcurrent = 0
)

var _ Resolver = (*net.Resolver)(nil)

type options struct {
	// resolver executes MX queries. Defaults to [net.DefaultResolver].
	resolver Resolver `optgen:"notnil,default=net.DefaultResolver"`

	// egressPolicy applies name-only deny rules to the queried host.
	egressPolicy *httpclient.EgressPolicy

	// maxTimeout bounds the lookup duration. Nonpositive values are ignored.
	maxTimeout time.Duration `optgen:"default=DefaultMaxTimeout"`

	// maxConcurrent limits concurrent lookups. Saturation returns
	// [ErrBusy] immediately. Nonpositive values disable the limit.
	maxConcurrent int `optgen:"default=DefaultMaxConcurrent"`
}
