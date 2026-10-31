// Copyright 2021-2026 ALTESSA SOLUTIONS INC.
// Use of this source code is governed by the MIT license that can be found in
// the LICENSE file.

package client

import (
	"context"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/altessa-s/go-atlas-net/httpx"
)

func TestRetryRoundTripper_PolicyPin(t *testing.T) {
	t.Parallel()

	t.Run("a policy retrying a transport error retries it", func(t *testing.T) {
		t.Parallel()
		var calls atomic.Int32
		rt := &retryRoundTripper{
			observer:          NoopObserver{},
			retryableStatuses: DefaultRetryableStatuses(),
			next: httpx.RoundTripperFunc(func(*http.Request) (*http.Response, error) {
				if calls.Add(1) < 3 {
					return nil, errors.New("transient")
				}
				return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody}, nil
			}),
			retryPolicyHandler: func(_ context.Context, _ *http.Response, err error) (bool, error) {
				return err != nil, nil
			},
			retryOpts:   newTestRetryOpts(5),
			maxAttempts: 5,
		}

		req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://example.com", nil)
		resp, err := rt.RoundTrip(req)
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, resp.StatusCode)
		require.Equal(t, int32(3), calls.Load())
	})

	t.Run("a policy retrying an empty outcome stops", func(t *testing.T) {
		t.Parallel()
		var calls atomic.Int32
		rt := &retryRoundTripper{
			observer:          NoopObserver{},
			retryableStatuses: DefaultRetryableStatuses(),
			next: httpx.RoundTripperFunc(func(*http.Request) (*http.Response, error) {
				calls.Add(1)
				return nil, nil //nolint:nilnil // the degenerate outcome under test
			}),
			retryPolicyHandler: func(context.Context, *http.Response, error) (bool, error) { return true, nil },
			retryOpts:          newTestRetryOpts(5),
			maxAttempts:        5,
		}

		req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://example.com", nil)
		resp, err := rt.RoundTrip(req) //nolint:bodyclose // no reply
		require.NoError(t, err)
		require.Nil(t, resp)
		require.Equal(t, int32(1), calls.Load())
	})
}
