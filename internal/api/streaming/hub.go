package streaming

import (
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// DefaultBufferSize is the per-topic ring-buffer capacity used when
// no explicit override is supplied. 256 events is the sweet spot:
//
//   - At the 1m closed-bucket cadence (/v1/price/stream): ~4 hours of
//     replay window — generous for the typical 30s reconnect blip
//     and enough for a multi-minute network outage.
//   - At the 5s tip cadence (/v1/price/tip/stream): ~21 minutes,
//     comfortable for short reconnect storms.
//
// At 4 KiB per Event the buffer is ~1 MiB per topic — fine even with
// hundreds of topics.
const DefaultBufferSize = 256

// subscriberQueueDepth is the per-subscriber channel capacity. Once
// the channel fills up the subscription is dropped (see Hub.Publish).
// 32 events is enough for short bursts but small enough that a stuck
// subscriber gets evicted quickly rather than ballooning memory.
const subscriberQueueDepth = 32

// DefaultTopicIdleTTL is how long a topic that still holds buffered
// events but has NO subscribers is kept before the reaper drops it
// (REL-05). It is the window in which a client that disconnects can
// come back with Last-Event-ID and still get its replay: 15 minutes is
// far beyond the typical reconnect blip (seconds) and a multi-minute
// network outage, while stopping a pair that goes quiet for good from
// holding its ring buffer for the life of the process.
//
// Topics whose buffer is EMPTY — never published to, which is exactly
// the shape a client mints by streaming an arbitrary pair — have
// nothing to replay and are dropped as soon as their last subscriber
// leaves, without waiting out this TTL.
const DefaultTopicIdleTTL = 15 * time.Minute

// DefaultMaxTopics is the ceiling on live topics per Hub. Real
// deployments key topics by traded pair — hundreds, not thousands — so
// 4096 leaves generous headroom while capping the worst case (an empty
// 256-event ring is ~20 KiB, so ~80 MiB at the ceiling) well short of
// anything that could exhaust the API process.
//
// Over the ceiling the reaper evicts SUBSCRIBER-LESS topics
// oldest-first. Topics with a live subscriber are never evicted (that
// would silently break an open stream), so the true bound is
// max(DefaultMaxTopics, concurrent subscribers) — and concurrent
// subscribers are themselves capped before Subscribe can allocate
// anything (see [Stream] / maxConcurrentStreams).
const DefaultMaxTopics = 4096

const (
	// topicSweepGrowth reaps once this many topics have been created
	// since the last pass. This is what bounds a BURST: a flood minting
	// one topic per connection is swept every 64 creations instead of
	// accumulating for a whole sweep interval.
	topicSweepGrowth = 64

	// topicSweepInterval is the slow-trickle floor — a Hub that creates
	// topics rarely still reaps roughly this often.
	//
	// Reaping is opportunistic: it runs on topic CREATION, so a Hub
	// that stops creating topics keeps what it already holds. That is
	// deliberate — it keeps the Hub free of a background goroutine (it
	// has no Close/lifecycle to stop one) and is sufficient, because
	// the map can only grow through the same creation path that reaps.
	topicSweepInterval = 30 * time.Second
)

// Hub is the pub/sub primitive backing the SSE stream handlers.
// One instance is shared across all stream endpoints (each endpoint
// uses different topic names — typically pair-keyed, e.g.
// "tip:native/fiat:USD").
//
// Hub is goroutine-safe.
type Hub struct {
	gen        Generator
	bufferSize int

	mu     sync.RWMutex
	topics map[string]*topicState
	// Reaper state, guarded by mu (see reapLocked).
	idleTTL    time.Duration
	maxTopics  int
	lastSweep  time.Time
	sinceSweep int

	// reaped is a cumulative diagnostic counter; atomic so
	// [Hub.TopicsReaped] can be read without taking mu.
	reaped atomic.Uint64
}

// topicState is the per-topic ring buffer + subscriber list.
//
// Held via a per-topic pointer so Hub.Publish can lock the topic
// alone, not the whole Hub — avoids lock-contention spikes when many
// topics publish concurrently.
type topicState struct {
	mu     sync.Mutex
	buffer *ring
	subs   map[*subscription]struct{}

	// lastUsed is the last publish/subscribe/unsubscribe on this topic;
	// it drives idle reaping. evicted marks a state the reaper has
	// detached from Hub.topics — publishing into or subscribing on a
	// detached state would silently lose the event, so every user
	// re-checks it under mu (see Hub.withTopic).
	lastUsed time.Time
	evicted  bool
}

// NewHub returns a Hub with [DefaultBufferSize] per topic. Pass 0 to
// take the default; positive values override per-topic capacity.
//
// Topic retention takes [DefaultTopicIdleTTL] and [DefaultMaxTopics];
// override with [Hub.SetTopicIdleTTL] / [Hub.SetMaxTopics].
func NewHub(bufferSize int) *Hub {
	if bufferSize <= 0 {
		bufferSize = DefaultBufferSize
	}
	return &Hub{
		bufferSize: bufferSize,
		topics:     make(map[string]*topicState),
		idleTTL:    DefaultTopicIdleTTL,
		maxTopics:  DefaultMaxTopics,
		lastSweep:  time.Now(),
	}
}

// SetTopicIdleTTL overrides how long a subscriber-less topic keeps its
// replay buffer before the reaper drops it. Pass <= 0 to restore
// [DefaultTopicIdleTTL]. Call at startup (or from tests).
func (h *Hub) SetTopicIdleTTL(d time.Duration) {
	if d <= 0 {
		d = DefaultTopicIdleTTL
	}
	h.mu.Lock()
	h.idleTTL = d
	h.mu.Unlock()
}

// SetMaxTopics overrides the topic-map ceiling. Pass <= 0 to restore
// [DefaultMaxTopics]. Call at startup (or from tests).
func (h *Hub) SetMaxTopics(n int) {
	if n <= 0 {
		n = DefaultMaxTopics
	}
	h.mu.Lock()
	h.maxTopics = n
	h.mu.Unlock()
}

// TopicCount reports how many topics the Hub currently holds — the
// gauge for the bound [DefaultMaxTopics] enforces.
func (h *Hub) TopicCount() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.topics)
}

// TopicsReaped reports the cumulative number of topics the reaper has
// dropped since the Hub was created — the counter that shows the bound
// doing work (a flat zero next to a high [Hub.TopicCount] means the
// retention policy is mistuned).
func (h *Hub) TopicsReaped() uint64 { return h.reaped.Load() }

// Publish broadcasts a fresh Event on the given topic. The Event's
// ID and Timestamp are populated by Hub — callers MUST leave both
// zero. Returns the assigned ID.
//
// Slow subscribers are dropped: if a subscriber's queue is full,
// its channel is closed and the subscription removed. The dropped
// client sees the connection close and reconnects with
// Last-Event-ID for buffered replay.
func (h *Hub) Publish(topic, eventType string, data []byte) string {
	ev := Event{
		Type:      eventType,
		Data:      data,
		Timestamp: time.Now(),
	}

	var subs []*subscription
	h.withTopic(topic, func(t *topicState) {
		// Draw the ID INSIDE the topic lock. Assigning it outside meant
		// ID assignment and ring insertion were not atomic, so two
		// concurrent publishes to one topic could enter the ring out of
		// ID order: G1 draws A, G2 draws B>A, G2 wins the lock and
		// pushes B, then G1 pushes A. The ring's ascending-ID invariant
		// — which snapshotAfter's strict `>` filter depends on — is
		// broken, and the consequences are all silent: a live subscriber
		// receives B before A on a channel documented as ID-ordered; an
		// EventSource that stores the LAST id it saw (A) gets B again on
		// reconnect; a client tracking the MAX id (B) never receives A
		// even though it is sitting in the buffer; and ring.push evicts
		// slot 0, which under inversion is not the lowest-ID event
		// (cold audit 2026-08-04).
		ev.ID = h.gen.Next()
		t.buffer.push(ev)
		// Snapshot subscribers so we can release the topic lock before
		// sending — keeps a slow sub from blocking publishers (sends
		// below are non-blocking anyway, but the snapshot lets us drop
		// them off-lock).
		subs = make([]*subscription, 0, len(t.subs))
		for s := range t.subs {
			subs = append(subs, s)
		}
	})

	for _, s := range subs {
		// trySend is guarded by the subscription's own mutex, so it can
		// never race with a concurrent close() (CS-012): a send on a
		// closed channel panics and would crash the whole process, and
		// the send here is deliberately off the topic lock, so a
		// cancel()/drop on another goroutine (or another topic, for a
		// multi-topic sub) could otherwise close sub.ch mid-send.
		if full := s.trySend(ev); full {
			h.dropSubscriber(topic, s)
		}
	}
	return ev.ID
}

// Subscribe registers a subscriber across one or more topics, with
// an optional Last-Event-ID resume cursor. The returned chan
// receives buffered-replay events first (in ID order), then live
// events. Unsubscribe by calling cancel().
//
// If lastEventID is empty, no replay happens — the client gets only
// events published after Subscribe returns. If lastEventID is older
// than the subscriber queue can hold, the OLDEST events are dropped and
// replay starts partway through the buffer (the client sees an ID jump,
// which is the documented signal that some events were lost). It is not
// disconnected — see the note in the loop below.
func (h *Hub) Subscribe(topics []string, lastEventID string) (<-chan Event, func()) {
	sub := &subscription{
		ch:     make(chan Event, subscriberQueueDepth),
		topics: append([]string(nil), topics...),
	}
	cancel := func() {
		for _, topic := range topics {
			h.dropSubscriber(topic, sub)
		}
	}

	for _, topic := range topics {
		// Replay and live registration happen in ONE topic-locked
		// critical section. Doing them in two (snapshot, then register)
		// leaves a gap: Publish takes the same lock to push into the
		// ring AND to snapshot subscribers, so an event landing in the
		// gap is in neither this subscriber's replay nor its fanout and
		// is silently lost. Interleaved this way every event is in
		// exactly one of the two — no loss, no duplicate — and replay
		// is queued before any live event can be, so the channel stays
		// in ID order per topic.
		h.withTopic(topic, func(t *topicState) {
			// Replay the NEWEST events that fit, then register for live.
			//
			// This used to replay oldest-first and CLOSE the connection
			// the moment the queue filled, which is the worst of both:
			// the client received the 32 OLDEST buffered events — the
			// stalest prices in the ring — and was then disconnected.
			// Measured on r1: every reconnect returned exactly 32 events
			// and closed in 6-8ms, so a client 20 minutes behind ground
			// through ~8 reconnect rounds rendering progressively-stale
			// prices as live, and one that only advanced its
			// Last-Event-ID after a "complete" sync never converged at
			// all. Nothing on the wire distinguishes a replayed event
			// from a live one.
			//
			// Dropping the oldest instead is the honest trade: the
			// client lands on the CURRENT price immediately and stays
			// connected. The lost span is exactly the gap the ID jump is
			// documented to signal (cold audit 2026-08-04).
			replay := t.buffer.snapshotAfter(lastEventID)
			if len(replay) > subscriberQueueDepth {
				replay = replay[len(replay)-subscriberQueueDepth:]
			}
			for _, ev := range replay {
				if !sub.sendReplay(ev) {
					break
				}
			}
			t.subs[sub] = struct{}{}
		})
	}

	return sub.ch, cancel
}

// dropSubscriber removes sub from the named topic's subscriber set
// and closes its channel exactly once. Called when a subscription
// is cancelled or its queue fills up.
func (h *Hub) dropSubscriber(topic string, sub *subscription) {
	h.mu.RLock()
	t, ok := h.topics[topic]
	h.mu.RUnlock()
	if !ok {
		return
	}
	t.mu.Lock()
	_, present := t.subs[sub]
	if present {
		delete(t.subs, sub)
	}
	// Unsubscribing is activity: it starts the idle clock the reaper
	// measures against, so a topic that just lost its last listener
	// keeps its replay buffer for the full idle TTL.
	t.lastUsed = time.Now()
	t.mu.Unlock()
	// Close outside the topic lock; sub.close() is idempotent and
	// mutually exclusive with trySend via the subscription's own mutex.
	if present {
		sub.close()
	}
}

// withTopic runs fn under the named topic's lock, creating the topic
// on first use and refreshing its idle clock so anything in active use
// is never reaped.
//
// It retries when the reaper evicted the state between lookup and
// lock: an evicted topicState is detached from Hub.topics, so a push
// into it (or a registration on it) would be invisible to everyone
// else. The loop makes progress — every retry costs some other
// goroutine a whole completed sweep, landing in the nanosecond window
// between the lookup and the lock here — so it cannot spin against
// itself.
func (h *Hub) withTopic(name string, fn func(t *topicState)) {
	for {
		t := h.getOrCreateTopic(name)
		t.mu.Lock()
		if t.evicted {
			t.mu.Unlock()
			continue
		}
		t.lastUsed = time.Now()
		fn(t)
		t.mu.Unlock()
		return
	}
}

// getOrCreateTopic returns the topicState for `name`, creating it
// on first use. Held outside any topic lock so two concurrent
// publishers on different topics don't serialise.
func (h *Hub) getOrCreateTopic(name string) *topicState {
	h.mu.RLock()
	t, ok := h.topics[name]
	h.mu.RUnlock()
	if ok {
		return t
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	// Double-check under write lock — another goroutine may have
	// won the race.
	if t, ok = h.topics[name]; ok {
		return t
	}
	now := time.Now()
	// Bound the map BEFORE inserting: the key is caller-supplied and,
	// on the /v1/price/stream path, client-supplied — without this a
	// stream of made-up pairs grows h.topics forever (REL-05).
	h.maybeReapLocked(now)
	t = &topicState{
		buffer:   newRing(h.bufferSize),
		subs:     make(map[*subscription]struct{}),
		lastUsed: now,
	}
	h.topics[name] = t
	return t
}

// maybeReapLocked runs a reap pass when enough topics have been
// created (or enough time has passed) since the last one, or when the
// map is at its ceiling. Caller holds h.mu for writing.
func (h *Hub) maybeReapLocked(now time.Time) {
	h.sinceSweep++
	if h.sinceSweep < topicSweepGrowth &&
		len(h.topics) < h.maxTopics &&
		now.Sub(h.lastSweep) < topicSweepInterval {
		return
	}
	h.reapLocked(now)
}

// reapLocked drops every topic that no longer earns its memory:
//
//   - no subscribers AND an empty buffer — nothing to replay, so
//     recreating it on the next use is free (this is the shape a
//     made-up pair leaves behind);
//   - no subscribers AND idle past idleTTL — the replay window a
//     reconnecting client could still have used has expired.
//
// If the map is still at the ceiling afterwards, subscriber-less
// topics are evicted least-recently-used first. Topics with a live
// subscriber are NEVER dropped — that would silently detach an open
// stream from its fanout.
//
// Caller holds h.mu for writing. Topic locks are taken underneath it,
// which is the one place the two are held together; no other path
// takes h.mu while holding a topic lock, so the order can't invert.
func (h *Hub) reapLocked(now time.Time) {
	h.lastSweep = now
	h.sinceSweep = 0

	type candidate struct {
		name     string
		lastUsed time.Time
	}
	var idle []candidate
	for name, t := range h.topics {
		t.mu.Lock()
		unused := len(t.subs) == 0
		expired := unused && (t.buffer.empty() || now.Sub(t.lastUsed) >= h.idleTTL)
		if expired {
			t.evicted = true
		}
		lastUsed := t.lastUsed
		t.mu.Unlock()
		switch {
		case expired:
			delete(h.topics, name)
			h.reaped.Add(1)
		case unused:
			idle = append(idle, candidate{name: name, lastUsed: lastUsed})
		}
	}
	if len(h.topics) < h.maxTopics {
		return
	}

	// Still at the ceiling: give up replay buffers oldest-first until
	// there's room. A topic evicted here is recreated (empty) on its
	// next publish, so the cost is a lost replay window, never a lost
	// live event.
	sort.Slice(idle, func(i, j int) bool { return idle[i].lastUsed.Before(idle[j].lastUsed) })
	for _, c := range idle {
		if len(h.topics) < h.maxTopics {
			return
		}
		t, ok := h.topics[c.name]
		if !ok {
			continue
		}
		t.mu.Lock()
		unused := len(t.subs) == 0
		if unused {
			t.evicted = true
		}
		t.mu.Unlock()
		if unused {
			delete(h.topics, c.name)
			h.reaped.Add(1)
		}
	}
}

// subscription is one active stream's per-Hub state.
//
// mu serializes trySend against close so a publisher can never send on a
// channel a concurrent cancel/drop has closed (CS-012 — send-on-closed
// panics and crashes the process). It is held only for a non-blocking
// select, so contention is negligible.
type subscription struct {
	ch     chan Event
	topics []string

	mu     sync.Mutex
	closed bool
}

// trySend delivers ev to the subscriber without blocking. It returns
// full=true when the subscriber's queue is full (caller should drop it).
// A closed subscription is a no-op (returns full=false) — never a panic.
func (s *subscription) trySend(ev Event) (full bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return false
	}
	select {
	case s.ch <- ev:
		return false
	default:
		return true
	}
}

// sendReplay queues one buffered-replay event during Subscribe.
// Returns false when the queue is full OR the subscription has already
// been closed — the caller drops the subscription and the client
// reconnects with a fresher Last-Event-ID.
//
// Guarded by the same mutex as trySend/close: replay now runs while
// the subscription is registered on earlier topics, so a concurrent
// drop could otherwise close sub.ch mid-send and crash the process
// (CS-012).
func (s *subscription) sendReplay(ev Event) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return false
	}
	select {
	case s.ch <- ev:
		return true
	default:
		return false
	}
}

// close closes the subscriber channel exactly once. Idempotent and
// mutually exclusive with trySend.
func (s *subscription) close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.closed {
		s.closed = true
		close(s.ch)
	}
}
