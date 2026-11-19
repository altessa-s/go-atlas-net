// Copyright 2021-2026 ALTESSA SOLUTIONS INC.
// Use of this source code is governed by the MIT license that can be found in
// the LICENSE file.

package client_test

import (
	"context"
	"errors"
	"io"
	"iter"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	httpclient "github.com/altessa-s/go-atlas-net/http/client"
)

// pairs returns an iterator over the given key/value pairs, in order.
func pairs(kv ...[2]string) iter.Seq2[string, string] {
	return func(yield func(string, string) bool) {
		for _, p := range kv {
			if !yield(p[0], p[1]) {
				return
			}
		}
	}
}

// TestWithHeaders_CaseVariants checks WithHeaders through PostWithOptions in
// both iteration orders: an ordinary header ends with one value, the later
// entry's; idempotency key variants are all kept and refused.
func TestWithHeaders_CaseVariants(t *testing.T) {
	t.Parallel()

	var (
		mu           sync.Mutex
		contentTypes [][]string
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()

		contentTypes = append(contentTypes, r.Header.Values("Content-Type"))
		mu.Unlock()

		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	c := httpclient.NewHTTPClient(loopback)

	ct := [][2]string{{"Content-Type", "application/a"}, {"content-type", "application/b"}}
	keys := [][2]string{{"Idempotency-Key", "A"}, {"idempotency-key", "B"}}

	for _, order := range [][2]int{{0, 1}, {1, 0}} {
		resp, err := c.PostWithOptions(t.Context(), srv.URL, "text/plain", strings.NewReader("x"),
			httpclient.WithHeaders(pairs(ct[order[0]], ct[order[1]])))
		require.NoError(t, err)

		_ = resp.Body.Close()

		_, err = c.PostWithOptions(t.Context(), srv.URL, "text/plain", strings.NewReader("x"), //nolint:bodyclose // refused
			httpclient.WithHeaders(pairs(keys[order[0]], keys[order[1]])))
		require.ErrorIs(t, err, httpclient.ErrAmbiguousIdempotencyKey)
	}

	mu.Lock()
	defer mu.Unlock()

	require.Equal(t, [][]string{{"application/b"}, {"application/a"}}, contentTypes,
		"one Content-Type, the later entry of the iteration; the refused requests never arrive")
}

// TestRetrySnapshot_URLChangeDetection checks that the snapshot is what
// telemetry reports and that the live URL is compared structurally: a Host
// change of an Opaque URL, which URL.String omits, stops the loop.
func TestRetrySnapshot_URLChangeDetection(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		opaque bool
		change func(*http.Request)
	}{
		{name: "host of an opaque URL", opaque: true, change: func(r *http.Request) { r.URL.Host = "other.test:1" }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			srv, seen := recordingServer(t)
			obs := &recordingObserver{}

			u, err := url.Parse(srv.URL)
			require.NoError(t, err)

			host := u.Hostname()

			var req *http.Request

			c := fastRetry(httpclient.WithObserver(obs),
				httpclient.WithRetryPolicyHandler(func(ctx context.Context, resp *http.Response, err error) (bool, error) {
					tc.change(req)
					return httpclient.DefaultRetryPolicy(ctx, resp, err)
				}))

			req, err = http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL+"/path", nil)
			require.NoError(t, err)

			if tc.opaque {
				req.URL.Opaque = "/path"
			}

			_, err = c.Do(req) //nolint:bodyclose // a failed request returns no response
			require.ErrorContains(t, err, "request changed during the retry loop")
			require.Len(t, seen(), 1)

			obs.mu.Lock()
			defer obs.mu.Unlock()

			require.Len(t, obs.done, 1)
			require.Equal(t, host, obs.done[0].Host, "telemetry reports the snapshot")
		})
	}
}

// eofTrailerBody streams a pipe, sets its trailer's X-Sum at the end and
// then closes eof.
type eofTrailerBody struct {
	r       *io.PipeReader
	trailer http.Header
	eof     chan struct{}
	once    sync.Once
}

func (b *eofTrailerBody) Read(p []byte) (int, error) {
	n, err := b.r.Read(p)
	if errors.Is(err, io.EOF) {
		b.once.Do(func() {
			b.trailer.Set("X-Sum", "abc")
			close(b.eof)
		})
	}

	return n, err
}

func (b *eofTrailerBody) Close() error { return b.r.Close() }

// TestResponseView_FullDuplexHTTP2 sends a streaming upload over HTTP/2 to a
// server that replies before reading it, so the reply's view is built while
// the upload may still be writing its trailers (run with -race: the view never
// reads the live map). The policy then waits for the upload's end: the view is
// metadata-only (no body), carries the declaration made before sending or the
// final snapshot, and editing it reaches neither the trailers sent nor the
// caller's map.
func TestResponseView_FullDuplexHTTP2(t *testing.T) {
	t.Parallel()

	var (
		mu          sync.Mutex
		gotTrailer  string
		uploadBytes int
	)

	replied := make(chan struct{})

	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// A non-2xx reply would make net/http stop the upload: reply 202.
		w.WriteHeader(http.StatusAccepted)
		http.NewResponseController(w).Flush() //nolint:errcheck // a flush failure only delays the reply
		close(replied)

		b, _ := io.ReadAll(r.Body)

		mu.Lock()
		uploadBytes, gotTrailer = len(b), r.Trailer.Get("X-Sum")
		mu.Unlock()
	}))
	srv.EnableHTTP2 = true
	srv.StartTLS()
	t.Cleanup(srv.Close)

	type seenView struct {
		proto      int
		bodyNil    bool
		getBodyNil bool
		declared   bool
		value      string
	}

	views := make(chan seenView, 1)
	eof := make(chan struct{})

	c := httpclient.New(httpclient.WithoutSSRFProtection(), httpclient.WithClient(srv.Client()),
		httpclient.WithRetryPolicyHandler(func(_ context.Context, resp *http.Response, _ error) (bool, error) {
			if resp != nil {
				<-eof // the upload has written its trailers

				_, declared := resp.Request.Trailer["X-Sum"]
				views <- seenView{
					proto:      resp.ProtoMajor,
					bodyNil:    resp.Request.Body == nil,
					getBodyNil: resp.Request.GetBody == nil,
					declared:   declared,
					value:      resp.Request.Trailer.Get("X-Sum"),
				}
				// Editing the view cannot reach the upload.
				resp.Request.Trailer.Set("X-Sum", "evil")
			}

			return false, nil
		}))

	pr, pw := io.Pipe()

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL, nil)
	require.NoError(t, err)

	req.Trailer = http.Header{"X-Sum": nil}
	req.Body = &eofTrailerBody{r: pr, trailer: req.Trailer, eof: eof}

	go func() {
		_, _ = io.WriteString(pw, "first ")

		// The upload ends, writing its trailers, after the reply is on its
		// way and (most likely) after the client built the view, with no
		// synchronization between the two: the race detector would report
		// a view that reads the live map.
		<-replied
		time.Sleep(50 * time.Millisecond)

		_, _ = io.WriteString(pw, "second")
		_ = pw.Close()
	}()

	resp, err := c.Do(req)
	require.NoError(t, err)

	v := <-views
	require.Equal(t, 2, v.proto)
	require.True(t, v.bodyNil)
	require.True(t, v.getBodyNil)
	require.True(t, v.declared, "the declared trailer key is visible")
	require.Contains(t, []string{"", "abc"}, v.value,
		"the view holds the pre-send declaration or the synchronized final snapshot")

	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()

	mu.Lock()
	defer mu.Unlock()

	require.Equal(t, len("first second"), uploadBytes)
	require.Equal(t, "abc", gotTrailer, "the upload's trailer is not the view's edit")
	require.Equal(t, "abc", req.Trailer.Get("X-Sum"), "the view's edit never reaches the caller's map")
}
