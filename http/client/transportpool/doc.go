// Copyright 2021-2026 ALTESSA SOLUTIONS INC.
// Use of this source code is governed by the MIT license that can be found in
// the LICENSE file.

// Package transportpool caches transports by caller-selected keys in a bounded
// LRU. Keys must include every policy, route and profile identity that affects
// connection reuse. Builders must return a non-nil transport and must not call
// back into the pool. Selectors and transports must support concurrent calls.
package transportpool
