// Copyright 2021-2026 ALTESSA SOLUTIONS INC. All rights reserved.
// Use of this source code is governed by license that can be found in
// the LICENSE file.

package urlx

import (
	"fmt"
	"net/url"
	"slices"
	"strings"

	coreerrs "github.com/altessa-s/go-atlas-core/errors"
)

// ErrInvalidURL reports a URL rejected by [ParseBaseURL]. It wraps
// [coreerrs.ErrInvalidArgument]; callers classify it with errors.Is and map it
// to their own sentinel. The error text names the violated rule but never
// contains the URL, which may carry a credential in its path.
var ErrInvalidURL = fmt.Errorf("%w: invalid URL", coreerrs.ErrInvalidArgument)

// ErrSchemeNotAllowed reports a URL [ParseBaseURL] rejected only for its
// scheme: it is well-formed and absolute, but the scheme is not one of the
// allowed ones. It wraps [ErrInvalidURL], so callers that do not care keep
// matching that; callers that word a scheme failure differently (for
// example "must be an https URL") classify it with errors.Is.
var ErrSchemeNotAllowed = fmt.Errorf("%w: scheme is not allowed", ErrInvalidURL)

// ParseBaseURL parses raw as the absolute URL of a service a client sends
// requests to, and rejects every form that would let a path appended later
// end up somewhere other than the path:
//
//   - the scheme must be one of the allowed schemes ("http" and "https" by
//     default, see [WithAllowedSchemes]);
//   - the host must be present (a bare port such as "https://:443" is
//     rejected);
//   - userinfo is rejected, even an empty one ("https://@host");
//   - a query is rejected, including a bare "?" ([url.URL.ForceQuery]);
//   - a fragment is rejected, including a bare "#" (detected on raw, because
//     [url.Parse] leaves [url.URL.Fragment] empty for it).
//
// In base mode (the default) trailing slashes of the escaped path are
// trimmed, so "https://h/v1/" becomes "https://h/v1" and [JoinPath] can append
// to it. [WithEndpointMode] keeps the path exactly as given, for a URL that
// names a full endpoint. The escaped path is never decoded: "/a%2Fb" stays
// distinct from "/a/b". Scheme and host case are left as parsed; use
// [Canonical] to compare URLs.
//
// raw is not trimmed; surrounding whitespace makes it invalid. Every failure
// matches [ErrInvalidURL] and [coreerrs.ErrInvalidArgument]; a URL that is
// valid except for its scheme also matches [ErrSchemeNotAllowed].
func ParseBaseURL(raw string, opts ...Option) (*url.URL, error) {
	o := newOptions(opts...)
	if raw == "" {
		return nil, invalid("URL is empty")
	}
	u, err := url.Parse(raw)
	if err != nil {
		// The *url.Error text quotes raw; keep it out of the chain.
		return nil, invalid("URL is malformed")
	}
	switch {
	case u.Scheme == "":
		return nil, invalid("URL must be absolute")
	case u.Opaque != "" || u.Hostname() == "":
		return nil, invalid("host is required")
	case u.User != nil:
		return nil, invalid("userinfo is not allowed")
	case u.RawQuery != "" || u.ForceQuery:
		return nil, invalid("query is not allowed")
	case u.Fragment != "" || strings.Contains(raw, "#"):
		return nil, invalid("fragment is not allowed")
	case !slices.Contains(o.allowedSchemes, u.Scheme):
		// Checked last, so ErrSchemeNotAllowed means the URL is otherwise valid.
		return nil, fmt.Errorf("%w: %s", ErrSchemeNotAllowed, u.Scheme)
	}

	if !o.endpointMode {
		setEscapedPath(u, strings.TrimRight(u.EscapedPath(), "/"))
	}
	return u, nil
}

// JoinPath returns base with elem appended to its path, as a string.
//
// The escaped path of base is kept byte for byte, minus trailing slashes.
// Each element is literal path text: it is split on "/", empty segments are
// dropped and every segment is escaped with [url.PathEscape], so "?", "#" and
// "%" in an element stay in the path and never start a query or fragment. A
// trailing slash on the last element is kept. Dot segments ("." and "..") are
// not resolved; pass only trusted elements. A query or fragment already on
// base is kept unchanged (a URL returned by [ParseBaseURL] has none).
func JoinPath(base *url.URL, elem ...string) string {
	if base == nil {
		return ""
	}
	var p strings.Builder
	p.WriteString(strings.TrimRight(base.EscapedPath(), "/"))
	trailing := false
	for _, e := range elem {
		for seg := range strings.SplitSeq(e, "/") {
			if seg == "" {
				continue
			}
			p.WriteByte('/')
			p.WriteString(url.PathEscape(seg))
		}
		if e != "" {
			trailing = strings.HasSuffix(e, "/")
		}
	}
	if trailing {
		p.WriteByte('/')
	}

	u := *base
	setEscapedPath(&u, p.String())
	return u.String()
}

// Canonical renders u in a form suitable for comparing endpoints, for
// example to bind a stored credential to the endpoint it was issued for:
// scheme and host are lower-cased, the escaped path is kept (so "/a%2Fb" and
// "/a/b" stay distinct) without trailing slashes, and userinfo, query and
// fragment are dropped. An explicit port is kept, default or not, because it
// changes the Host header a gateway may route on. A nil u yields "".
func Canonical(u *url.URL) string {
	if u == nil {
		return ""
	}
	return strings.ToLower(u.Scheme) + "://" + strings.ToLower(u.Host) + strings.TrimRight(u.EscapedPath(), "/")
}

// setEscapedPath sets the path of u from its escaped form ep, so that
// [url.URL.EscapedPath] returns ep afterwards. ep is always a valid escaping
// here: it is built from [url.URL.EscapedPath] and [url.PathEscape] output
// joined with literal slashes, which [url.PathUnescape] accepts.
func setEscapedPath(u *url.URL, ep string) {
	if p, err := url.PathUnescape(ep); err == nil {
		u.Path, u.RawPath = p, ep
	}
}

func invalid(reason string) error {
	return fmt.Errorf("%w: %s", ErrInvalidURL, reason)
}
