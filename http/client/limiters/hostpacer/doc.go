// Copyright 2021-2026 ALTESSA SOLUTIONS INC.
// Use of this source code is governed by the MIT license that can be found in
// the LICENSE file.

// Package hostpacer provides a waiting per-host [limiters.RequestsLimiter]:
// every host gets a token bucket refilled once per interval, and
// [Pacer.Allow] blocks until the host's next slot instead of rejecting the
// request, so a crawl fan-out is spaced rather than failed.
//
// The buckets live in memory. Hosts whose bucket has refilled completely are
// dropped once [DefaultMaxHosts] (see [WithMaxHosts]) are tracked, which never
// loses pacing state. The bound is soft: buckets still refilling are kept, so
// it can be exceeded by the hosts used within one burst × interval window.
// LRU eviction is deliberately not used, as dropping a refilling bucket would
// let its host exceed its pace. Time is read and waits run through an
// injected clock.Clock ([WithClock]).
//
// # Usage
//
//	pacer, err := hostpacer.New(time.Second, 1)
//	if err != nil {
//	    return err
//	}
//	c := client.New(
//	    client.WithLimiter(limiters.Chain(budget, pacer)),
//	    client.WithLimiterPerAttempt(),
//	)
package hostpacer
