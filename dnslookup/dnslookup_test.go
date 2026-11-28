// Copyright 2021-2026 ALTESSA SOLUTIONS INC.
// Use of this source code is governed by the MIT license that can be found in
// the LICENSE file.

package dnslookup_test

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/altessa-s/go-atlas-net/dnslookup"

	httpclient "github.com/altessa-s/go-atlas-net/http/client"
)

type resolverFunc func(context.Context, string) ([]*net.MX, error)

func (f resolverFunc) LookupMX(ctx context.Context, name string) ([]*net.MX, error) {
	return f(ctx, name)
}

func TestLookupMX_QueryName(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct{ name, absolute string }{
		{"Contoso.COM.", "contoso.com."},
		{"Bücher。COM。", "xn--bcher-kva.com."},
		{"mail", "mail."},
		{"notexample.com", "notexample.com."},
		{"example.com.contoso.com", "example.com.contoso.com."},
		{"corp.internal", "corp.internal."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var calls []string

			l := dnslookup.New(dnslookup.WithResolver(resolverFunc(func(_ context.Context, name string) ([]*net.MX, error) {
				calls = append(calls, name)
				return nil, nil
			})))
			mx, err := l.LookupMX(t.Context(), tc.name)
			require.NoError(t, err)
			require.Empty(t, mx)
			require.Equal(t, []string{tc.absolute}, calls)
		})
	}
}

func TestLookupMX_RefusedName(t *testing.T) {
	t.Parallel()

	for _, name := range []string{
		"", ".", "contoso.com..", "a..com", ".com", " contoso.com", "contoso.com\n",
		"foo_bar.com", "-foo.com", "foo-.com", "a@contoso.com", "contoso.com:25",
		"é\u0080.com", "192.0.2.1", "2001:db8::1", "corp。LOCAL。", strings.Repeat("a", 64) + ".com",
		strings.Repeat(strings.Repeat("a", 63)+".", 4) + "com",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			l := dnslookup.New(dnslookup.WithResolver(resolverFunc(func(context.Context, string) ([]*net.MX, error) {
				t.Fatal("resolver called for refused name")
				return nil, nil
			})))
			mx, err := l.LookupMX(t.Context(), name)
			require.ErrorIs(t, err, dnslookup.ErrRefused)
			require.Empty(t, mx)
		})
	}
}

func TestLookupMX_EgressPolicy(t *testing.T) {
	t.Parallel()

	cidr := netip.MustParsePrefix("10.0.0.0/8")
	policy, err := httpclient.NewEgressPolicy(
		[]httpclient.EgressRule{{Host: "mx.corp.contoso.com", CIDR: cidr}, {Host: "allow.contoso.com", CIDR: cidr}},
		[]httpclient.EgressRule{
			{Host: "*.corp.contoso.com"},
			{Host: "cidr.contoso.com", CIDR: cidr},
			{Host: "port.contoso.com", Port: 25},
		},
	)
	require.NoError(t, err)

	for _, tc := range []struct {
		name string
		want error
	}{
		{"MX.CORP.CONTOSO.COM.", dnslookup.ErrDenied},
		{"cidr.contoso.com", nil},
		{"port.contoso.com", nil},
		{"allow.contoso.com", nil},
		{"outside.contoso.com", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var calls int

			l := dnslookup.New(dnslookup.WithEgressPolicy(policy), dnslookup.WithResolver(resolverFunc(
				func(context.Context, string) ([]*net.MX, error) {
					calls++
					return []*net.MX{{Host: "mx.corp.contoso.com."}}, nil
				},
			)))

			mx, err := l.LookupMX(t.Context(), tc.name)
			if tc.want != nil {
				require.ErrorIs(t, err, tc.want)
				require.ErrorIs(t, err, dnslookup.ErrRefused)
				require.ErrorIs(t, err, httpclient.ErrSSRFBlocked)
				require.Equal(t, tc.want.Error(), err.Error())

				var cause *httpclient.SSRFError
				require.ErrorAs(t, err, &cause)
				require.Equal(t, "mx.corp.contoso.com", cause.Host)
				require.Empty(t, mx)
				require.Zero(t, calls)

				return
			}

			require.NoError(t, err)
			require.Equal(t, []dnslookup.MX{{Host: "mx.corp.contoso.com"}}, mx)
			require.Equal(t, 1, calls)
		})
	}
}

func TestLookupMX_RecordSet(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name    string
		records []*net.MX
		want    []dnslookup.MX
		err     error
	}{
		{"null MX", []*net.MX{{Host: "."}}, []dnslookup.MX{{Host: "."}}, nil},
		{"nonzero root preference", []*net.MX{{Host: ".", Pref: 1}}, []dnslookup.MX{{Host: ".", Pref: 1}}, nil},
		{
			"mixed null and ordinary",
			[]*net.MX{{Host: "SMTP.GOOGLE.COM.", Pref: 1}, {Host: "."}},
			[]dnslookup.MX{{Host: "smtp.google.com", Pref: 1}, {Host: "."}},
			nil,
		},
		{"duplicate null MX", []*net.MX{{Host: "."}, {Host: "."}}, []dnslookup.MX{{Host: "."}, {Host: "."}}, nil},
		{"missing record", []*net.MX{nil}, nil, dnslookup.ErrResolver},
		{
			"ordinary mixed exchanges",
			[]*net.MX{{Host: "C.CONTOSO.COM.", Pref: 20}, {Host: "B.CONTOSO.COM.", Pref: 10}, {Host: "a.contoso.com", Pref: 10}},
			[]dnslookup.MX{{Host: "c.contoso.com", Pref: 20}, {Host: "b.contoso.com", Pref: 10}, {Host: "a.contoso.com", Pref: 10}},
			nil,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			l := dnslookup.New(dnslookup.WithResolver(resolverFunc(func(context.Context, string) ([]*net.MX, error) {
				return tc.records, nil
			})))
			mx, err := l.LookupMX(t.Context(), "contoso.com")
			require.ErrorIs(t, err, tc.err)
			require.Equal(t, tc.want, mx)
		})
	}
}

func TestLookupMX_PreservesAllRecordsInOrder(t *testing.T) {
	t.Parallel()

	const count = 33

	failure := errors.New("server failure")

	for _, tc := range []struct {
		name  string
		cause error
		want  error
	}{
		{name: "complete"},
		{name: "partial", cause: failure, want: dnslookup.ErrResolver},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			records := make([]*net.MX, count)
			want := make([]dnslookup.MX, count)

			for i := range records {
				pref := uint16(count - i)
				records[i] = &net.MX{Host: fmt.Sprintf("MX%d.CONTOSO.COM.", pref), Pref: pref}
				want[i] = dnslookup.MX{Host: fmt.Sprintf("mx%d.contoso.com", pref), Pref: pref}
			}

			l := dnslookup.New(dnslookup.WithResolver(resolverFunc(func(context.Context, string) ([]*net.MX, error) {
				return records, tc.cause
			})))
			mx, err := l.LookupMX(t.Context(), "contoso.com")
			require.ErrorIs(t, err, tc.want)
			require.ErrorIs(t, err, tc.cause)
			require.Equal(t, want, mx)

			require.Equal(t, &net.MX{Host: "MX33.CONTOSO.COM.", Pref: 33}, records[0])
		})
	}
}

func TestLookupMX_ResolverError(t *testing.T) {
	t.Parallel()

	const name = "contoso.com."
	for _, tc := range []struct {
		name        string
		records     []*net.MX
		cause       *net.DNSError
		want        error
		wantRecords []dnslookup.MX
	}{
		{"not found", nil, &net.DNSError{Name: name, Err: "no such host", IsNotFound: true}, dnslookup.ErrNotFound, nil},
		{"resolver timeout", nil, &net.DNSError{Name: name, Err: "timeout", IsTimeout: true}, dnslookup.ErrTimeout, nil},
		{
			"partial answer with root exchange",
			[]*net.MX{{Host: ".", Pref: 7}, {Host: "SMTP.GOOGLE.COM.", Pref: 10}},
			&net.DNSError{Name: name, Err: "server failure"},
			dnslookup.ErrResolver,
			[]dnslookup.MX{{Host: ".", Pref: 7}, {Host: "smtp.google.com", Pref: 10}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			l := dnslookup.New(dnslookup.WithResolver(resolverFunc(func(context.Context, string) ([]*net.MX, error) {
				return tc.records, tc.cause
			})))
			mx, err := l.LookupMX(t.Context(), name)
			require.ErrorIs(t, err, tc.want)
			require.ErrorIs(t, err, tc.cause)
			require.Equal(t, tc.want.Error(), err.Error())
			require.Equal(t, tc.wantRecords, mx)

			var lookupErr *dnslookup.LookupError
			require.ErrorAs(t, err, &lookupErr)

			var cause *net.DNSError
			require.ErrorAs(t, err, &cause)
			require.Same(t, tc.cause, cause)
		})
	}
}

func TestLookupMX_Deadline(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name              string
		parent, max, want time.Duration
	}{
		{"context deadline only", time.Second, 0, time.Second},
		{"option deadline only", 0, time.Second, time.Second},
		{"earlier parent deadline", time.Second, 3 * time.Second, time.Second},
		{"earlier option deadline", 3 * time.Second, time.Second, time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			synctest.Test(t, func(t *testing.T) {
				ctx := t.Context()

				if tc.parent > 0 {
					var cancel context.CancelFunc

					ctx, cancel = context.WithTimeout(ctx, tc.parent)
					defer cancel()
				}

				l := dnslookup.New(dnslookup.WithMaxTimeout(tc.max), dnslookup.WithResolver(resolverFunc(
					func(ctx context.Context, _ string) ([]*net.MX, error) {
						deadline, ok := ctx.Deadline()
						require.True(t, ok)
						require.Equal(t, tc.want, time.Until(deadline))
						<-ctx.Done()

						return nil, ctx.Err()
					},
				)))
				mx, err := l.LookupMX(ctx, "contoso.com")
				require.Empty(t, mx)
				require.ErrorIs(t, err, dnslookup.ErrTimeout)
				require.ErrorIs(t, err, context.DeadlineExceeded)
			})
		})
	}
}

func TestLookupMX_Canceled(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	l := dnslookup.New(dnslookup.WithResolver(resolverFunc(func(context.Context, string) ([]*net.MX, error) {
		t.Fatal("resolver called with canceled context")
		return nil, nil
	})))
	_, err := l.LookupMX(ctx, "contoso.com")
	require.ErrorIs(t, err, context.Canceled)
}

func TestLookupMX_CanceledDuringQuery(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	l := dnslookup.New(dnslookup.WithResolver(resolverFunc(func(context.Context, string) ([]*net.MX, error) {
		cancel()
		return []*net.MX{{Host: "smtp.google.com."}}, nil
	})))
	mx, err := l.LookupMX(ctx, "contoso.com")
	require.Empty(t, mx)
	require.ErrorIs(t, err, context.Canceled)
}

func TestLookupMX_Busy(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		release := make(chan struct{})
		l := dnslookup.New(dnslookup.WithMaxConcurrent(1), dnslookup.WithResolver(resolverFunc(
			func(ctx context.Context, _ string) ([]*net.MX, error) {
				<-release
				return nil, ctx.Err()
			},
		)))

		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()

		done := make(chan error, 1)

		go func() {
			_, err := l.LookupMX(ctx, "contoso.com")
			done <- err
		}()

		synctest.Wait()

		_, err := l.LookupMX(t.Context(), "contoso.com")

		close(release)
		require.ErrorIs(t, err, dnslookup.ErrBusy)
		require.NoError(t, <-done)

		_, err = l.LookupMX(t.Context(), "contoso.com")
		require.NoError(t, err)
	})
}
