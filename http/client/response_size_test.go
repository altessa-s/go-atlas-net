// Copyright 2021-2026 ALTESSA SOLUTIONS INC.
// Use of this source code is governed by the MIT license that can be found in
// the LICENSE file.

package client_test

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	httpclient "github.com/altessa-s/go-atlas-net/http/client"
)

// An oversized reply announced by Content-Length is reported with its status
// and header, and is never retried — not even with a retryable status.
func TestResponseSizeErrorCarriesStatus(t *testing.T) {
	t.Parallel()

	const limit = 16
	for _, status := range []int{http.StatusOK, http.StatusServiceUnavailable} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			t.Parallel()

			var hits atomic.Int32
			body := strings.Repeat("x", limit*2)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				hits.Add(1)
				w.Header().Set("X-Request-Id", "abc")
				w.Header().Set("Content-Length", strconv.Itoa(len(body)))
				w.WriteHeader(status)
				_, _ = w.Write([]byte(body))
			}))
			t.Cleanup(srv.Close)

			c := httpclient.New(loopback, httpclient.WithMaxResponseSize(limit), httpclient.WithRetryMax(3), fastRetries)
			err := doGet(t, c, t.Context(), srv.URL)
			require.EqualValues(t, 1, hits.Load())
			require.ErrorIs(t, err, httpclient.ErrResponseSizeExceeded)
			require.Nil(t, httpclient.IsRetryExhaustedError(err))
			se := httpclient.IsResponseSizeError(err)
			require.NotNil(t, se, "%v", err)
			require.Equal(t, status, se.Status)
			require.Equal(t, "abc", se.Header.Get("X-Request-Id"))
			require.EqualValues(t, limit, se.Limit)
			require.EqualValues(t, len(body), se.Size)
		})
	}
}
