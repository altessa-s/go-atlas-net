// Copyright 2021-2026 ALTESSA SOLUTIONS INC.
// Use of this source code is governed by the MIT license that can be found in
// the LICENSE file.

package drivertest

import (
	"context"
	"io"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"

	"github.com/altessa-s/go-atlas-net/http/client"
)

func testH2ProcessedReplay(t *testing.T, f func(*testing.T) client.TransportDriver) {
	for _, goAway := range []bool{false, true} {
		for _, withoutRetry := range []bool{false, true} {
			for _, rewindable := range []bool{false, true} {
				name := "processed/" + map[bool]string{false: "close", true: "GOAWAY"}[goAway] +
					map[bool]string{false: "/plain", true: "/WithoutRetry"}[withoutRetry] +
					map[bool]string{false: streamSuffix, true: rewindableSuffix}[rewindable]
				t.Run(name, func(t *testing.T) {
					t.Parallel()
					testH2ProcessedReplayCase(t, f, goAway, withoutRetry, rewindable)
				})
			}
		}
	}
}

func testH2ProcessedReplayCase(t *testing.T, f func(*testing.T) client.TransportDriver, goAway, withoutRetry, rewindable bool) {
	var events replayEvents

	var completed atomic.Int32

	env := h2ReplayEnvironment(t, &events, func(conn net.Conn) {
		serveProcessedFrames(t, conn, &events, &completed, goAway)
	})

	var c *http.Client

	if withoutRetry {
		c = client.New(client.WithoutSSRFProtection(), client.WithoutProxy(), client.WithoutRetry(),
			client.WithTransportDriver(f(t)), client.WithTransport(&http.Transport{DialContext: env.Dial, TLSClientConfig: env.TLS}))
		t.Cleanup(c.CloseIdleConnections)
	} else {
		c = &http.Client{Transport: transport(t, f(t), env, false)}
	}

	var body io.Reader = strings.NewReader(payload)
	if !rewindable {
		body = io.LimitReader(body, int64(len(payload)))
	}

	ctx, cancel := context.WithTimeout(t.Context(), replayTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://"+authority+"/", body)
	require.NoError(t, err)
	require.Equal(t, rewindable, req.GetBody != nil)

	resp, err := c.Do(req)
	if resp != nil {
		_ = resp.Body.Close()
	}

	require.Error(t, err, "a processed POST without a response must fail")

	// Keep accepting connections after the caller returns to expose detached
	// retries, including ones that transmit only headers or part of the body.
	events.check(t)
	_, streams := events.counts()
	require.Equal(t, 1, streams, "processed POST must be transmitted exactly once across all connections")
	require.EqualValues(t, 1, completed.Load(), "fixture must receive the complete POST before dropping it")
}

func serveProcessedFrames(t *testing.T, conn net.Conn, events *replayEvents, completed *atomic.Int32, goAway bool) {
	t.Helper()

	preface := make([]byte, len(http2.ClientPreface))
	if _, err := io.ReadFull(conn, preface); err != nil || string(preface) != http2.ClientPreface {
		return
	}

	fr := http2.NewFramer(conn, conn)

	fr.ReadMetaHeaders = hpack.NewDecoder(h2HeaderTableSize, nil)
	if err := fr.WriteSettings(); err != nil {
		return
	}

	bodies := make(map[uint32]string)

	for {
		frame, err := fr.ReadFrame()
		if err != nil {
			return
		}

		var ended uint32

		switch frame := frame.(type) {
		case *http2.MetaHeadersFrame:
			events.record(false)

			if frame.PseudoValue("method") != http.MethodPost {
				t.Error("fixture received a non-POST request")
				return
			}

			bodies[frame.StreamID] = ""
			if frame.StreamEnded() {
				ended = frame.StreamID
			}
		case *http2.DataFrame:
			bodies[frame.StreamID] += string(frame.Data())
			if frame.StreamEnded() {
				ended = frame.StreamID
			}
		default:
			if _, ok := endedStream(fr, frame); !ok {
				return
			}
		}

		if ended == 0 {
			continue
		}

		if bodies[ended] != payload {
			t.Errorf("received POST body %q; want %q", bodies[ended], payload)
			return
		}

		completed.Add(1)

		// Including this stream in GOAWAY means it may have been processed.
		// Neither this signal nor a bare connection loss permits a POST replay.
		if goAway {
			if err := fr.WriteGoAway(ended, http2.ErrCodeNo, nil); err != nil {
				t.Errorf("sending processed GOAWAY: %v", err)
			}
		}

		return
	}
}

const h2HeaderTableSize = 4096
