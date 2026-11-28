// Copyright 2021-2026 ALTESSA SOLUTIONS INC.
// Use of this source code is governed by the MIT license that can be found in
// the LICENSE file.

package dnslookup

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net"
	"slices"
	"strings"

	"github.com/altessa-s/go-atlas-net/egressrule"
)

var (
	// ErrNotFound means the resolver found no MX records for the name.
	ErrNotFound = errors.New("MX records not found")
	// ErrTimeout means the lookup deadline expired or the resolver timed out.
	ErrTimeout = errors.New("MX lookup timed out")
	// ErrRefused means the query name is invalid or reserved for special use.
	ErrRefused = errors.New("MX lookup name refused")
	// ErrDenied means the egress policy denied the query name.
	ErrDenied = errors.New("MX lookup name denied")
	// ErrBusy means the lookup could not acquire a concurrency slot.
	ErrBusy = errors.New("MX lookup concurrency limit reached")
	// ErrResolver means the resolver failed or returned an invalid MX set.
	ErrResolver = errors.New("MX resolver failure")
)

var errInvalidMX = errors.New("invalid MX record set")

// LookupError classifies a lookup failure and preserves its underlying cause.
// Use [errors.Is] to check the category or the cause.
type LookupError struct {
	kind  error
	cause error
}

// Error returns the error category and, when available, the underlying cause.
func (e *LookupError) Error() string {
	if e.cause == nil {
		return e.kind.Error()
	}

	return fmt.Sprintf("%s: %v", e.kind, e.cause)
}

// Is reports whether target is this error's category.
func (e *LookupError) Is(target error) bool { return e.kind == target }

// Unwrap returns the resolver, policy, or context error, if present.
func (e *LookupError) Unwrap() error { return e.cause }

// MX is a mail exchanger. Host is normalized and Pref is its DNS preference.
type MX struct {
	Host string
	Pref uint16
}

// Resolver looks up MX records. Implementations must honor context deadlines
// and cancellation and support concurrent calls. [net.Resolver] satisfies it.
type Resolver interface {
	LookupMX(ctx context.Context, name string) ([]*net.MX, error)
}

// Lookup performs guarded MX queries and is safe for concurrent use.
// Construct it with [New].
type Lookup struct {
	options *options
	slots   chan struct{}
}

// New creates an MX lookup with the given options.
func New(opts ...Option) *Lookup {
	o := newOptions(opts...)
	if o.maxRecords <= 0 {
		o.maxRecords = DefaultMaxRecords
	}

	l := &Lookup{options: o}
	if o.maxConcurrent > 0 {
		l.slots = make(chan struct{}, o.maxConcurrent)
	}

	return l
}

// LookupMX queries the absolute form of name after validation and policy checks.
// Results are sorted by preference, then host, and capped at the configured
// maximum. A sole MX with preference zero and exchange "." returns an empty
// result. A resolver error can accompany partial results.
func (l *Lookup) LookupMX(ctx context.Context, name string) ([]MX, error) {
	if l.options.maxTimeout > 0 {
		var cancel context.CancelFunc

		ctx, cancel = context.WithTimeout(ctx, l.options.maxTimeout)
		defer cancel()
	}

	if err := ctx.Err(); err != nil {
		return nil, classifyError(err)
	}

	name = egressrule.NormalizeHost(name)
	if !validHost(name) || isSpecialUse(name) {
		return nil, &LookupError{kind: ErrRefused}
	}

	if policy := l.options.egressPolicy; policy != nil {
		if err := policy.CheckHost(name, 0); err != nil {
			return nil, &LookupError{kind: ErrDenied, cause: err}
		}
	}

	if l.slots != nil {
		select {
		case l.slots <- struct{}{}:
			defer func() { <-l.slots }()
		default:
			return nil, &LookupError{kind: ErrBusy}
		}
	}

	if err := ctx.Err(); err != nil {
		return nil, classifyError(err)
	}

	records, err := l.options.resolver.LookupMX(ctx, name+".")
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, classifyError(ctxErr)
	}

	mx, parseErr := parseMX(records, l.options.maxRecords)
	if err != nil {
		return mx, classifyError(err)
	}

	return mx, parseErr
}

func classifyError(err error) error {
	switch {
	case errors.Is(err, context.Canceled):
		return err
	case errors.Is(err, context.DeadlineExceeded):
		return &LookupError{kind: ErrTimeout, cause: err}
	}

	if netErr, ok := errors.AsType[net.Error](err); ok && netErr.Timeout() {
		return &LookupError{kind: ErrTimeout, cause: err}
	}

	if dnsErr, ok := errors.AsType[*net.DNSError](err); ok && dnsErr.IsNotFound {
		return &LookupError{kind: ErrNotFound, cause: err}
	}

	return &LookupError{kind: ErrResolver, cause: err}
}

func parseMX(records []*net.MX, maxRecords int) ([]MX, error) {
	if len(records) == 0 {
		return nil, nil
	}

	result := make([]MX, 0, len(records))
	for _, record := range records {
		if record == nil {
			return nil, &LookupError{kind: ErrResolver, cause: errInvalidMX}
		}

		if record.Host == "." {
			if len(records) == 1 && record.Pref == 0 {
				return nil, nil
			}

			return nil, &LookupError{kind: ErrResolver, cause: errInvalidMX}
		}

		result = append(result, MX{Host: egressrule.NormalizeHost(record.Host), Pref: record.Pref})
	}

	slices.SortFunc(result, func(a, b MX) int {
		return cmp.Or(cmp.Compare(a.Pref, b.Pref), cmp.Compare(a.Host, b.Host))
	})

	if len(result) > maxRecords {
		return slices.Clone(result[:maxRecords]), nil
	}

	return result, nil
}

// validHost checks DNS hostname syntax after normalization, including label
// and total wire length. An all-numeric name can be confused with an address.
func validHost(name string) bool {
	const maxNameLen, maxLabelLen = 253, 63
	if len(name) > maxNameLen {
		return false
	}

	nonNumeric := false

	for label := range strings.SplitSeq(name, ".") {
		if len(label) == 0 || len(label) > maxLabelLen || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}

		for i := range len(label) {
			switch c := label[i]; {
			case c >= 'a' && c <= 'z', c == '-':
				nonNumeric = true
			case c >= '0' && c <= '9':
			default:
				return false
			}
		}
	}

	return nonNumeric
}
