// Copyright 2021-2026 ALTESSA SOLUTIONS INC.
// Use of this source code is governed by the MIT license that can be found in
// the LICENSE file.

package transportpool

import (
	"container/list"
	"context"
	"io"
	"net/http"
	"slices"
	"sync"
	"time"
)

// Selector selects a transport key and its lazy builder for a request context.
// The builder is called only on a cache miss, serialized with other builders.
type Selector func(ctx context.Context) (key string, build func() http.RoundTripper)

// Retirer is implemented by a transport that can stop keeping connections:
// after Retire it closes every connection as soon as it becomes idle. The
// pool calls Retire once, after it evicted the transport and every request it
// had acquired the transport for has finished, so Retire never races with a
// request of the pool. A transport that does not implement it is cleaned up
// with CloseIdleConnections as described on [Pool]; the connections that
// cleanup misses live until the transport's own idle timeout, so such a
// transport should have one.
type Retirer interface {
	Retire()
}

// Pool is a concurrent, bounded LRU of transports. Construct it with New.
//
// Eviction does not interrupt active requests. An evicted transport is
// retired: it serves no new request and the pool keeps owning its cleanup.
// A [Retirer] is told once its acquired requests have finished. Its idle
// connections are closed at eviction, whenever one of its responses ends or
// is closed, [WithRetireDelay] after its
// last response finished (a transport may hand a connection back only after
// the body was closed), on every later eviction and on every
// [Pool.CloseIdleConnections]. The pool keeps as many retired transports as
// it caches; the oldest is dropped, after one more cleanup, when a newer one
// retires.
type Pool struct {
	mu          sync.Mutex
	selector    Selector
	max         int
	retireDelay time.Duration
	entries     map[string]*list.Element
	lru         list.List
	retired     []*entry // oldest first, at most max
}

type entry struct {
	key       string
	transport http.RoundTripper
	// inflight counts the requests acquired for the transport whose response
	// has not finished; evicted and retireTold are set once; timer is the
	// delayed cleanup of the retired entry. All are guarded by Pool.mu.
	inflight   int
	evicted    bool
	retireTold bool
	timer      *time.Timer
}

// New constructs a pool. A nil selector panics. Nonpositive limits use 64.
func New(selector Selector, opts ...Option) *Pool {
	if selector == nil {
		panic("transportpool: nil selector")
	}

	o := newOptions(opts...)
	if o.maxEntries <= 0 {
		o.maxEntries = DefaultMaxEntries
	}

	if o.retireDelay <= 0 {
		o.retireDelay = DefaultRetireDelay
	}

	return &Pool{
		selector: selector, max: o.maxEntries, retireDelay: o.retireDelay,
		entries: make(map[string]*list.Element),
	}
}

// RoundTrip selects a transport and sends the request exactly once.
func (p *Pool) RoundTrip(req *http.Request) (*http.Response, error) {
	key, build := p.selector(req.Context())

	e, evicted := p.acquire(key, build)
	if evicted != nil {
		p.retire(evicted)
	}

	resp, err := e.transport.RoundTrip(req)
	if err != nil || resp == nil || resp.Body == nil {
		p.finish(e)
		p.cleanup(e)

		return resp, err
	}

	body := &responseBody{ReadCloser: resp.Body, finish: func() { p.finish(e) }, cleanup: func() { p.cleanup(e) }}
	if writer, ok := resp.Body.(io.Writer); ok {
		resp.Body = &responseReadWriteBody{responseBody: body, Writer: writer}
	} else {
		resp.Body = body
	}

	return resp, nil
}

// acquire returns the entry of key with one more request in flight, building
// it on a miss, and the entry that had to be evicted for it, if any.
func (p *Pool) acquire(key string, build func() http.RoundTripper) (*entry, *entry) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if el := p.entries[key]; el != nil {
		p.lru.MoveToFront(el)

		e := el.Value.(*entry) //nolint:errcheck // Only *entry values enter this private list.
		e.inflight++

		return e, nil
	}

	tr := build()
	if tr == nil {
		panic("transportpool: builder returned nil")
	}

	e := &entry{key: key, transport: tr, inflight: 1}
	p.entries[key] = p.lru.PushFront(e)

	var old *entry

	if p.lru.Len() > p.max {
		el := p.lru.Back()
		old = el.Value.(*entry) //nolint:errcheck // Only *entry values enter this private list.
		old.evicted = true
		delete(p.entries, old.key)
		p.lru.Remove(el)
	}

	return e, old
}

// retire takes an evicted entry into the retired set, dropping the oldest
// retired entry beyond the limit, and cleans every retired transport up.
func (p *Pool) retire(e *entry) {
	p.mu.Lock()
	p.retired = append(p.retired, e)

	var dropped *entry

	if len(p.retired) > p.max {
		dropped = p.retired[0]
		p.retired = slices.Delete(p.retired, 0, 1)

		// The pool stops owning dropped: its pending delayed cleanup must not
		// keep it alive. A response of it still in flight schedules its own.
		if dropped.timer != nil {
			dropped.timer.Stop()
		}
	}

	retired := slices.Clone(p.retired)
	p.mu.Unlock()

	if dropped != nil {
		closeIdle(dropped.transport)
	}

	for _, r := range retired {
		if r != e { // cleanup below takes care of e
			closeIdle(r.transport)
		}
	}

	p.cleanup(e)
}

// finish records the end of one response of e.
func (p *Pool) finish(e *entry) {
	p.mu.Lock()
	e.inflight--
	p.mu.Unlock()
}

// cleanup closes the idle connections of e when it is retired and, while no
// response of it is in flight, schedules one more cleanup after the retire
// delay. It runs at the end of a body and again at its Close, so the delay
// counts from whichever comes last. The entry stays in the retired set.
//
// A [Retirer] is told once, when the entry is retired and nothing the pool
// sent on it is in flight any more: a request that acquired the transport
// before its eviction still has to be served. The delayed cleanup is one
// timer per entry, restarted rather than multiplied.
func (p *Pool) cleanup(e *entry) {
	p.mu.Lock()
	evicted := e.evicted
	drained := evicted && e.inflight == 0
	retire := drained && !e.retireTold

	if retire {
		e.retireTold = true
	}

	if drained {
		if e.timer == nil {
			e.timer = time.AfterFunc(p.retireDelay, func() { closeIdle(e.transport) })
		} else {
			e.timer.Reset(p.retireDelay)
		}
	}
	p.mu.Unlock()

	if !evicted {
		return
	}

	if r, ok := e.transport.(Retirer); ok && retire {
		r.Retire()
	}

	closeIdle(e.transport)
}

// CloseIdleConnections closes idle connections on every cached transport and
// on every retired one. Active requests continue and the pool remains usable.
func (p *Pool) CloseIdleConnections() {
	p.mu.Lock()

	transports := make([]http.RoundTripper, 0, len(p.entries)+len(p.retired))
	for _, el := range p.entries {
		transports = append(transports, el.Value.(*entry).transport) //nolint:errcheck // Only *entry values enter this private list.
	}

	for _, e := range p.retired {
		transports = append(transports, e.transport)
	}
	p.mu.Unlock()

	for _, tr := range transports {
		closeIdle(tr)
	}
}

func closeIdle(tr http.RoundTripper) {
	if c, ok := tr.(interface{ CloseIdleConnections() }); ok {
		c.CloseIdleConnections()
	}
}

// responseBody reports the end of a response once (finish) and asks for the
// cleanup of its transport both when the body ends and when it is closed: a
// transport may still hold the connection busy at the end of the body and
// release it only in Close.
type responseBody struct {
	io.ReadCloser
	once    sync.Once
	finish  func()
	cleanup func()
}

func (b *responseBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if err != nil {
		// Only the first terminal read ends the response; later ones repeat
		// the error and change nothing.
		b.once.Do(func() {
			b.finish()
			b.cleanup()
		})
	}

	return n, err
}

func (b *responseBody) Close() error {
	err := b.ReadCloser.Close()

	b.once.Do(b.finish)
	b.cleanup()

	return err
}

// responseReadWriteBody preserves duplex upgraded responses and release tracking.
type responseReadWriteBody struct {
	*responseBody
	io.Writer
}
