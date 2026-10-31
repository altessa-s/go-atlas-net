// Copyright 2021-2026 ALTESSA SOLUTIONS INC.
// Use of this source code is governed by the MIT license that can be found in
// the LICENSE file.

package urlx_test

import (
	"errors"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/altessa-s/go-atlas-net/urlx"

	coreerrs "github.com/altessa-s/go-atlas-core/errors"
)

func TestParseBaseURL(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		raw  string
		opts []urlx.Option
		want string // empty: the URL must be rejected
	}{
		{name: "https origin", raw: "https://api.example.org", want: "https://api.example.org"},
		{name: "http origin", raw: "http://localhost:8080", want: "http://localhost:8080"},
		{name: "root slash trimmed", raw: "https://api.example.org/", want: "https://api.example.org"},
		{name: "several slashes trimmed", raw: "https://api.example.org/v1//", want: "https://api.example.org/v1"},
		{name: "path kept", raw: "https://api.example.org/v1/openai", want: "https://api.example.org/v1/openai"},
		{name: "escaped path kept", raw: "https://h/a%2Fb/", want: "https://h/a%2Fb"},
		{name: "escaped trailing slash kept", raw: "https://h/a%2F", want: "https://h/a%2F"},
		{name: "upper-case scheme accepted", raw: "HTTPS://h/v1", want: "https://h/v1"},
		{name: "host case preserved", raw: "https://API.Example.org/v1", want: "https://API.Example.org/v1"},
		{name: "ipv6 host", raw: "https://[::1]:8443/x", want: "https://[::1]:8443/x"},
		{
			name: "endpoint mode keeps trailing slash",
			raw:  "https://h/custom/recognize/",
			opts: []urlx.Option{urlx.WithEndpointMode()},
			want: "https://h/custom/recognize/",
		},
		{
			name: "custom scheme",
			raw:  "smtps://mail.example.org:465",
			opts: []urlx.Option{urlx.WithAllowedSchemes("SMTPS")},
			want: "smtps://mail.example.org:465",
		},

		{name: "empty", raw: ""},
		{name: "malformed", raw: "https://h/%zz"},
		{name: "relative", raw: "/v1/chat"},
		{name: "scheme relative", raw: "//h/v1"},
		{name: "ftp scheme", raw: "ftp://h/v1"},
		{name: "http not in custom list", raw: "http://h", opts: []urlx.Option{urlx.WithAllowedSchemes("https")}},
		{name: "opaque", raw: "https:h/v1"},
		{name: "no host", raw: "https:///v1"},
		{name: "port only", raw: "https://:443/v1"},
		{name: "userinfo", raw: "https://user:pass@h/v1"},
		{name: "empty userinfo", raw: "https://@h/v1"},
		{name: "query", raw: "https://h/v1?key=secret"},
		{name: "bare question mark", raw: "https://h/?"},
		{name: "bare question mark endpoint", raw: "https://h/x?", opts: []urlx.Option{urlx.WithEndpointMode()}},
		{name: "fragment", raw: "https://h/v1#top"},
		{name: "bare hash", raw: "https://h/#"},
		{name: "bare hash on origin", raw: "https://h#"},
		{name: "leading space", raw: " https://h"},
		{name: "trailing space", raw: "https://h "},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			u, err := urlx.ParseBaseURL(tc.raw, tc.opts...)
			if tc.want == "" {
				require.ErrorIs(t, err, urlx.ErrInvalidURL)
				require.ErrorIs(t, err, coreerrs.ErrInvalidArgument)
				require.Nil(t, u)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.want, u.String())
		})
	}
}

func TestParseBaseURLErrorOmitsURL(t *testing.T) {
	t.Parallel()

	const secret = "123456:SECRET-TOKEN"
	for _, raw := range []string{
		"https://h/bot" + secret + "?",
		"https://h/bot" + secret + "#",
		"https://" + secret + "@h/",
		"https://h/bot" + secret + "/%zz",
		"ftp://h/bot" + secret,
	} {
		_, err := urlx.ParseBaseURL(raw)
		require.ErrorIs(t, err, urlx.ErrInvalidURL)
		require.NotContains(t, err.Error(), "SECRET")
	}
}

func TestParseBaseURLSchemeNotAllowed(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		raw    string
		scheme bool
	}{
		{name: "ftp", raw: "ftp://h/v1", scheme: true},
		{name: "http when only https", raw: "http://h/v1", scheme: true},
		{name: "ftp with query is structural", raw: "ftp://h/v1?x=1"},
		{name: "ftp with userinfo is structural", raw: "ftp://u@h/v1"},
		{name: "no host is structural", raw: "ftp:///v1"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, err := urlx.ParseBaseURL(tc.raw, urlx.WithAllowedSchemes("https"))
			require.ErrorIs(t, err, urlx.ErrInvalidURL)
			require.Equal(t, tc.scheme, errors.Is(err, urlx.ErrSchemeNotAllowed))
		})
	}
}

func TestParseBaseURLDefaultSchemesIsolated(t *testing.T) {
	t.Parallel()

	def := urlx.DefaultAllowedSchemes()
	def[0] = "ftp"
	_, err := urlx.ParseBaseURL("ftp://h")
	require.ErrorIs(t, err, urlx.ErrInvalidURL)
	require.Equal(t, []string{"http", "https"}, urlx.DefaultAllowedSchemes())
}

func TestJoinPath(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		base string
		elem []string
		want string
	}{
		{name: "no elements", base: "https://h/v1/", want: "https://h/v1"},
		{name: "origin", base: "https://h", elem: []string{"/chat/completions"}, want: "https://h/chat/completions"},
		{name: "base path", base: "https://h/v1", elem: []string{"chat", "completions"}, want: "https://h/v1/chat/completions"},
		{name: "slashes collapsed", base: "https://h/v1/", elem: []string{"/a//b/", "", "/c"}, want: "https://h/v1/a/b/c"},
		{name: "trailing slash kept", base: "https://h/v1", elem: []string{"models/"}, want: "https://h/v1/models/"},
		{name: "escaped base kept", base: "https://h/a%2Fb", elem: []string{"c"}, want: "https://h/a%2Fb/c"},
		{name: "question mark escaped", base: "https://h/v1", elem: []string{"models?after=x"}, want: "https://h/v1/models%3Fafter=x"},
		{name: "hash escaped", base: "https://h/v1", elem: []string{"a#b"}, want: "https://h/v1/a%23b"},
		{name: "percent escaped", base: "https://h/v1", elem: []string{"a%2Fb"}, want: "https://h/v1/a%252Fb"},
		{name: "colon kept", base: "https://h/v1beta", elem: []string{"models/gemini:generateContent"},
			want: "https://h/v1beta/models/gemini:generateContent"},
		{name: "space escaped", base: "https://h", elem: []string{"a b"}, want: "https://h/a%20b"},
		{
			name: "endpoint mode base",
			base: "https://h/custom/",
			elem: []string{"recognize"},
			want: "https://h/custom/recognize",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			base, err := urlx.ParseBaseURL(tc.base, urlx.WithEndpointMode())
			require.NoError(t, err)
			got, err := urlx.JoinPath(base, tc.elem...)
			require.NoError(t, err)
			require.Equal(t, tc.want, got)

			parsed, err := url.Parse(got)
			require.NoError(t, err)
			require.Empty(t, parsed.RawQuery)
			require.False(t, parsed.ForceQuery)
			require.Empty(t, parsed.Fragment)
			require.False(t, strings.Contains(got, "#"))
		})
	}
}

func TestJoinPathDoesNotMutateBase(t *testing.T) {
	t.Parallel()

	base, err := urlx.ParseBaseURL("https://h/v1")
	require.NoError(t, err)
	_, err = urlx.JoinPath(base, "chat")
	require.NoError(t, err)
	require.Equal(t, "https://h/v1", base.String())
	got, err := urlx.JoinPath(nil, "x")
	require.NoError(t, err)
	require.Empty(t, got)
}

func TestJoinPathRejectsDotSegments(t *testing.T) {
	t.Parallel()

	base, err := urlx.ParseBaseURL("https://h/v1")
	require.NoError(t, err)
	for _, tc := range []struct {
		name string
		elem []string
	}{
		{name: "dot", elem: []string{"."}},
		{name: "dot dot", elem: []string{".."}},
		{name: "leading dot dot", elem: []string{"../admin"}},
		{name: "inner dot dot", elem: []string{"a/../../admin"}},
		{name: "trailing dot", elem: []string{"a/."}},
		{name: "dot dot with slashes", elem: []string{"//..//"}},
		{name: "later element", elem: []string{"chat", ".."}},
		{name: "secret element", elem: []string{"botSECRET-TOKEN/.."}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := urlx.JoinPath(base, tc.elem...)
			require.ErrorIs(t, err, urlx.ErrDotSegment)
			require.ErrorIs(t, err, coreerrs.ErrInvalidArgument)
			require.NotContains(t, err.Error(), "SECRET")
			require.Empty(t, got)
		})
	}

	// Segments that merely contain dots, or escaped dots, are literal names.
	for elem, want := range map[string]string{
		"...":    "https://h/v1/...",
		".well":  "https://h/v1/.well",
		"a..b":   "https://h/v1/a..b",
		"%2e%2e": "https://h/v1/%252e%252e",
	} {
		got, err := urlx.JoinPath(base, elem)
		require.NoError(t, err)
		require.Equal(t, want, got)
	}
}

// FuzzJoinPath checks that an element either is rejected with ErrDotSegment
// or yields a URL whose path stays under the base path and has no dot
// segment, so no server-side resolution can climb above the base.
func FuzzJoinPath(f *testing.F) {
	for _, seed := range []string{"chat/completions", "..", "../x", "a/./b", "%2e%2e", "a?b#c", "//", "", "..."} {
		f.Add(seed)
	}
	base, err := urlx.ParseBaseURL("https://h/v1")
	require.NoError(f, err)
	f.Fuzz(func(t *testing.T, elem string) {
		got, err := urlx.JoinPath(base, elem)
		if err != nil {
			require.ErrorIs(t, err, urlx.ErrDotSegment)
			return
		}
		u, err := url.Parse(got)
		require.NoError(t, err)
		require.Empty(t, u.RawQuery)
		require.Empty(t, u.Fragment)
		p := u.EscapedPath()
		require.True(t, p == "/v1" || strings.HasPrefix(p, "/v1/"), "path %q escapes the base", p)
		for seg := range strings.SplitSeq(u.Path, "/") {
			require.NotContains(t, []string{".", ".."}, seg, "path %q has a dot segment", u.Path)
		}
	})
}

func TestCanonical(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		raw  string
		want string
	}{
		{name: "lower-cases scheme and host", raw: "HTTPS://API.Example.ORG/V1", want: "https://api.example.org/V1"},
		{name: "trims trailing slashes", raw: "https://h/v1//", want: "https://h/v1"},
		{name: "root", raw: "https://h/", want: "https://h"},
		{name: "keeps escaped path", raw: "https://h/a%2Fb", want: "https://h/a%2Fb"},
		{name: "keeps default port", raw: "https://h:443/x", want: "https://h:443/x"},
		{name: "drops userinfo query fragment", raw: "https://u:p@h/x/?q=1#f", want: "https://h/x"},
		{name: "drops bare query and fragment", raw: "https://h/x?#", want: "https://h/x"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			u, err := url.Parse(tc.raw)
			require.NoError(t, err)
			require.Equal(t, tc.want, urlx.Canonical(u))
		})
	}
	require.Empty(t, urlx.Canonical(nil))
}

func TestCanonicalDistinguishesEncodedSeparator(t *testing.T) {
	t.Parallel()

	a, err := urlx.ParseBaseURL("https://h/a/b")
	require.NoError(t, err)
	b, err := urlx.ParseBaseURL("https://h/a%2Fb")
	require.NoError(t, err)
	require.NotEqual(t, urlx.Canonical(a), urlx.Canonical(b))
}
