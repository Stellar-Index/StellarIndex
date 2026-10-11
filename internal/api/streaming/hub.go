package streaming

import (
	"encoding/json"
	"errors"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/obs"
)

// DefaultBufferSize is the per-topic ring capacity: ~4 hours of replay at the 1m closed-bucket
// cadence and ~21 minutes at the 5s tip cadence, about 1 MiB per topic at 4 KiB per Event.
const DefaultBufferSize = 256

// subscriberQueueDepth is the per-subscriber channel capacity. Once
// the channel fills up the subscription is dropped (see Hub.Publish).
// 32 events is enough for short bursts but small enough that a stuck
// subscriber gets evicted quickly rather than ballooning memory.
const subscriberQueueDepth = 32

// maxSplitReplayTopics is the widest subscription at which Hub.Subscribe still splits the replay
// budget evenly per topic. Alias fan-out (canonical.AssetAliases) is a handful of topics, so this
// is headroom, not a tuned limit.
const maxSplitReplayTopics = 8

// DefaultTopicIdleTTL is how long a subscriber-less topic with buffered events is kept, so a
// client that disconnects can reconnect with Last-Event-ID and still get its replay. Topics with an
// EMPTY buffer (what a client mints by streaming an arbitrary pair) have nothing to replay and are
// dropped as soon as their last subscriber leaves.
const DefaultTopicIdleTTL = 15 * time.Minute

// DefaultMaxTopics is the topic-map ceiling per Hub. Over it the reaper evicts SUBSCRIBER-LESS
// topics oldest-first; a subscribed topic is never evicted, because that would silently detach an
// open stream. When every topic is subscribed, [Hub.Subscribe] refuses with [ErrTopicCapacity], so
// client-supplied keys cannot grow the map past the ceiling; only Publish may briefly exceed it.
//
// Rings are allocated on first PUBLISH (see [topicState.buffer]), so memory scales with topics that
// carry events (~80 MiB at this bound) while a subscriber-only topic costs just a map entry.
// [Hub.BufferedTopicCount] reports the count that carries the memory.
const DefaultMaxTopics = 4096

// ErrTopicCapacity is returned by [Hub.Subscribe] when a topic it would
// create cannot fit under the ceiling because every held topic has a
// live subscriber. The caller should refuse the stream (503).
var ErrTopicCapacity = errors.New("streaming: hub topic capacity exhausted")

const (
	// topicSweepGrowth reaps once this many topics were created since the last pass, which
	// bounds a burst of one-topic-per-connection floods.
	topicSweepGrowth = 64

	// topicSweepInterval is the slow-trickle floor. Reaping runs on topic CREATION only, which
	// keeps the Hub free of a background goroutine (it has no Close to stop one) and is enough
	// because the map can only grow through that same path.
	topicSweepInterval = 30 * time.Second
)

// Hub is the goroutine-safe pub/sub primitive backing the SSE stream handlers, shared across all
// stream endpoints (topics are typically pair-keyed, e.g. "tip:native/fiat:USD").
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
	// ceilingFutile: the last sweep left the map at the ceiling, so every
	// topic held a subscriber and re-sweeping on each insert frees nothing.
	ceilingFutile bool
	// unsubscribed is set when a topic loses its last subscriber, the one
	// event that can make a futile ceiling reapable again.
	unsubscribed atomic.Bool

	// reaped is a cumulative diagnostic counter; atomic so
	// [Hub.TopicsReaped] can be read without taking mu.
	reaped atomic.Uint64
}

// topicState is the per-topic ring buffer + subscriber list, held by pointer so Hub.Publish locks
// the topic alone rather than the whole Hub.
type topicState struct {
	mu sync.Mutex
	// buffer is the replay ring, nil until the topic's first publish. The topic key is
	// client-supplied and /v1/price/stream subscribes to up to 9 alias topics per connection, so
	// eager allocation (~20 KiB each) would reserve gigabytes of rings that never hold an event.
	// Reads go through [topicState.replayAfter] / [topicState.bufferEmpty]; only Hub.Publish
	// allocates it.
	buffer *ring
	subs   map[*subscription]struct{}

	// lastUsed drives idle reaping. evicted marks a state the reaper detached from Hub.topics;
	// using a detached state would silently lose the event, so every user re-checks it under mu
	// (see Hub.withTopic).
	lastUsed time.Time
	evicted  bool
}

// replayAfter is [ring.snapshotAfter] over a topic whose ring may not
// exist yet: a topic that has never been published to has nothing to
// replay. Caller holds t.mu.
func (t *topicState) replayAfter(lastEventID string) []Event {
	if t.buffer == nil {
		return nil
	}
	return t.buffer.snapshotAfter(lastEventID)
}

// bufferEmpty reports whether the topic holds nothing a reconnecting
// client could replay — true both for a never-published topic (no ring)
// and for one whose ring is empty. Caller holds t.mu.
func (t *topicState) bufferEmpty() bool {
	return t.buffer == nil || t.buffer.empty()
}

// oldestBufferedID is [ring.oldestID] over a topic whose ring may not
// exist yet. Caller holds t.mu.
func (t *topicState) oldestBufferedID() string {
	if t.buffer == nil {
		return ""
	}
	return t.buffer.oldestID()
}

// hasEvictedBuffered is [ring.hasEvicted] over a topic whose ring may
// not exist yet. Caller holds t.mu.
func (t *topicState) hasEvictedBuffered() bool {
	return t.buffer != nil && t.buffer.hasEvicted()
}

// NewHub returns a Hub with bufferSize events per topic (0 takes [DefaultBufferSize]). Retention
// defaults to [DefaultTopicIdleTTL] and [DefaultMaxTopics]; see [Hub.SetTopicIdleTTL] and
// [Hub.SetMaxTopics].
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
	h.ceilingFutile = false
	h.mu.Unlock()
}

// TopicCount reports how many topics the Hub currently holds — the
// gauge for the reap threshold [DefaultMaxTopics] works against.
func (h *Hub) TopicCount() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.topics)
}

// BufferedTopicCount reports how many topics have allocated a replay ring (were published to at
// least once). This is the count that carries the Hub's memory; the difference from
// [Hub.TopicCount] is subscribed-only topics, which cost a map entry rather than ~20 KiB.
func (h *Hub) BufferedTopicCount() int {
	h.mu.RLock()
	topics := make([]*topicState, 0, len(h.topics))
	for _, t := range h.topics {
		topics = append(topics, t)
	}
	h.mu.RUnlock()

	n := 0
	for _, t := range topics {
		t.mu.Lock()
		if t.buffer != nil {
			n++
		}
		t.mu.Unlock()
	}
	return n
}

// TopicsReaped reports the cumulative number of topics the reaper has
// dropped since the Hub was created — the counter that shows the bound
// doing work (a flat zero next to a high [Hub.TopicCount] means the
// retention policy is mistuned).
func (h *Hub) TopicsReaped() uint64 { return h.reaped.Load() }

// Publish broadcasts a fresh Event on the given topic and returns the assigned ID; the Hub
// populates ID and Timestamp, so callers MUST leave both zero.
//
// A subscriber whose queue is full is dropped (channel closed, subscription removed); the client
// sees the connection close and reconnects with Last-Event-ID for buffered replay.
func (h *Hub) Publish(topic, eventType string, data []byte) string {
	ev := Event{
		Type:      eventType,
		Data:      data,
		Timestamp: time.Now(),
	}

	var subs []*subscription
	// Publish is never refused: its topics are server-chosen, and one it
	// creates has no subscriber, so the reaper can reclaim it.
	_ = h.withTopic(topic, false, func(t *topicState) {
		// Draw the ID INSIDE the topic lock: if ID assignment and ring insertion were not atomic,
		// concurrent publishes could enter the ring out of ID order, breaking the ascending-ID
		// invariant that snapshotAfter's strict `>` filter and ring.push eviction rely on. The
		// failures would be silent (B delivered before A, or A never replayed to a client
		// tracking the max id).
		ev.ID = h.gen.Next()
		// The ring is allocated on first publish, never on subscribe (see [topicState.buffer]).
		if t.buffer == nil {
			t.buffer = newRing(h.bufferSize)
		}
		t.buffer.push(ev)
		// Snapshot subscribers so the sends happen off the topic lock.
		subs = make([]*subscription, 0, len(t.subs))
		for s := range t.subs {
			subs = append(subs, s)
		}
	})

	for _, s := range subs {
		// trySend holds the subscription's own mutex because this send is off the topic lock: a
		// cancel() or drop on another goroutine (or topic) could otherwise close sub.ch mid-send,
		// and a send on a closed channel panics the whole process.
		if full := s.trySend(ev); full && h.dropSubscriber(topic, s) {
			obs.APIStreamSubscriberDropsTotal.Inc()
		}
	}
	return ev.ID
}

// Subscribe registers a subscriber across one or more topics, with
// an optional Last-Event-ID resume cursor. The returned chan receives
// any [EventTypeStreamGap] markers first, then buffered-replay events
// in ID order ACROSS every subscribed topic (not just within one),
// then live events. Unsubscribe by calling cancel().
//
// An empty lastEventID means no replay. If the backlog exceeds the subscriber queue, the OLDEST
// events are dropped (see the loop below) and a stream_gap marker precedes the replay only when
// the ring itself lost events. It returns [ErrTopicCapacity], holding no registration, when a
// topic it would create does not fit under the ceiling.
func (h *Hub) Subscribe(topics []string, lastEventID string) (<-chan Event, func(), error) {
	sub := &subscription{
		ch:     make(chan Event, subscriberQueueDepth),
		topics: append([]string(nil), topics...),
	}
	cancel := func() {
		for _, topic := range topics {
			h.dropSubscriber(topic, sub)
		}
	}

	// Replay is budgeted at HALF the channel capacity, shared across topics via a running total.
	// Filling the whole channel on resume would make the next live Publish find it full and evict
	// the subscriber it just resumed; the headroom lets it survive at least one live tick.
	//
	// Up to maxSplitReplayTopics the budget is also split evenly per topic, so a later alias topic
	// is not starved by an earlier one with a deeper backlog. Above that, each topic is offered
	// whatever of the shared budget is unspent.
	remaining := subscriberQueueDepth / 2
	var merged []Event
	var gaps []Event
	for i, topic := range topics {
		share := remaining
		if len(topics) <= maxSplitReplayTopics {
			topicsLeft := len(topics) - i
			share = (remaining + topicsLeft - 1) / topicsLeft // ceil(remaining/topicsLeft)
		}

		// Replay snapshot and live registration happen in ONE topic-locked section. Publish takes
		// the same lock to push and to snapshot subscribers, so an event landing between two
		// separate steps would be in neither the replay nor the fanout. This way every event is in
		// exactly one of them. The send to sub.ch is deferred until all topics are processed so
		// replay can be merge-sorted by ID; queuing topic-by-topic would walk the wire `id:` line
		// backwards at a topic boundary.
		err := h.withTopic(topic, true, func(t *topicState) {
			// Replay the NEWEST events that fit, then register for live. Replaying oldest-first and
			// closing when the queue fills hands the client the stalest prices and disconnects it,
			// so a lagging client would reconnect repeatedly and render stale prices as live
			// (nothing on the wire distinguishes replay from live). Dropping the oldest lands it on
			// the CURRENT price immediately and keeps it connected.
			oldestHeld := t.oldestBufferedID()
			full := t.replayAfter(lastEventID)
			trimmed := full
			if len(trimmed) > share {
				trimmed = trimmed[len(trimmed)-share:]
			}
			// A gap is reported only when the ring itself discarded an event the cursor implies it
			// held, NOT when this subscription's replay budget trimmed a deep backlog (that is the
			// ordinary "newest N" resume and would flag nearly every large resume). A cursor from a
			// foreign ID space (another region whose clock is ahead) sorts above the whole ring, so
			// replay is empty and would otherwise look like a clean resume.
			if h.resumeGap(t, lastEventID, oldestHeld) {
				gaps = append(gaps, newStreamGapEvent(topic, lastEventID, oldestHeld))
			}
			merged = append(merged, trimmed...)
			remaining -= len(trimmed)
			t.subs[sub] = struct{}{}
		})
		if err != nil {
			cancel()
			return nil, nil, err
		}
	}

	sort.SliceStable(merged, func(i, j int) bool { return merged[i].ID < merged[j].ID })
	for _, ev := range gaps {
		if !sub.sendReplay(ev) {
			break
		}
	}
	for _, ev := range merged {
		if !sub.sendReplay(ev) {
			break
		}
	}

	return sub.ch, cancel, nil
}

// resumeGap reports whether a resume at lastEventID lost events on t:
// the ring evicted past the cursor, or the cursor is from a foreign ID
// space. Caller holds t.mu.
func (h *Hub) resumeGap(t *topicState, lastEventID, oldestHeld string) bool {
	if lastEventID == "" {
		return false
	}
	evictedPast := oldestHeld != "" && oldestHeld > lastEventID && t.hasEvictedBuffered()
	return evictedPast || h.gen.Ahead(lastEventID)
}

// newStreamGapEvent builds the diagnostic marker Subscribe sends ahead
// of a replay that could not cover the requested cursor. No ID of its
// own — it must not perturb the client's Last-Event-ID tracking.
func newStreamGapEvent(topic, requestedAfter, resumedFrom string) Event {
	data, err := json.Marshal(struct {
		Topic          string `json:"topic"`
		RequestedAfter string `json:"requested_after"`
		ResumedFrom    string `json:"resumed_from"`
	}{Topic: topic, RequestedAfter: requestedAfter, ResumedFrom: resumedFrom})
	if err != nil {
		// Marshaling a struct of three strings cannot fail; keep the
		// marker informative rather than panic if it somehow does.
		data = []byte(`{}`)
	}
	return Event{Type: EventTypeStreamGap, Data: data, Timestamp: time.Now()}
}

// dropSubscriber removes sub from the named topic's subscriber set
// and closes its channel exactly once. Called when a subscription
// is cancelled or its queue fills up. Reports whether THIS call closed
// the subscription, so a multi-topic subscriber is counted once.
func (h *Hub) dropSubscriber(topic string, sub *subscription) bool {
	h.mu.RLock()
	t, ok := h.topics[topic]
	h.mu.RUnlock()
	if !ok {
		return false
	}
	t.mu.Lock()
	_, present := t.subs[sub]
	if present {
		delete(t.subs, sub)
		if len(t.subs) == 0 {
			h.unsubscribed.Store(true)
		}
	}
	// Unsubscribing is activity: it starts the idle clock the reaper
	// measures against, so a topic that just lost its last listener
	// keeps its replay buffer for the full idle TTL.
	t.lastUsed = time.Now()
	t.mu.Unlock()
	// Close outside the topic lock; sub.close() is idempotent and
	// mutually exclusive with trySend via the subscription's own mutex.
	return present && sub.close()
}

// withTopic runs fn under the named topic's lock, creating the topic on first use and refreshing
// its idle clock. It retries when the reaper evicted the state between lookup and lock, since a
// push or registration on a detached state would be invisible to everyone else; each retry costs
// another goroutine a whole sweep, so it cannot spin. With bounded set, creating the topic may
// fail with [ErrTopicCapacity].
func (h *Hub) withTopic(name string, bounded bool, fn func(t *topicState)) error {
	for {
		t, err := h.getOrCreateTopic(name, bounded)
		if err != nil {
			return err
		}
		t.mu.Lock()
		if t.evicted {
			t.mu.Unlock()
			continue
		}
		t.lastUsed = time.Now()
		fn(t)
		t.mu.Unlock()
		return nil
	}
}

// getOrCreateTopic returns the topicState for `name`, creating it on first use. It runs outside any
// topic lock so publishers on different topics do not serialise. With bounded set it refuses,
// rather than grow the map past maxTopics, once a reap leaves no room.
func (h *Hub) getOrCreateTopic(name string, bounded bool) (*topicState, error) {
	h.mu.RLock()
	t, ok := h.topics[name]
	h.mu.RUnlock()
	if ok {
		return t, nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	// Double-check under write lock — another goroutine may have
	// won the race.
	if t, ok = h.topics[name]; ok {
		return t, nil
	}
	now := time.Now()
	// Bound the map BEFORE inserting: the key is caller-supplied and,
	// on the /v1/price/stream path, client-supplied — without this a
	// stream of made-up pairs grows h.topics forever.
	h.maybeReapLocked(now)
	if bounded && len(h.topics) >= h.maxTopics {
		return nil, ErrTopicCapacity
	}
	// buffer is left nil: the ring is allocated on first PUBLISH, not
	// here. See [topicState.buffer].
	t = &topicState{
		subs:     make(map[*subscription]struct{}),
		lastUsed: now,
	}
	h.topics[name] = t
	obs.APIStreamHubTopics.Set(float64(len(h.topics)))
	return t, nil
}

// maybeReapLocked runs a reap pass when enough topics have been
// created (or enough time has passed) since the last one, or when the
// map is at its ceiling. Caller holds h.mu for writing.
func (h *Hub) maybeReapLocked(now time.Time) {
	h.sinceSweep++
	// A futile ceiling waits for an unsubscribe or the growth/interval
	// trigger: sweeping on every insert is O(n) under h.mu per new topic.
	atCeiling := len(h.topics) >= h.maxTopics &&
		(!h.ceilingFutile || h.unsubscribed.Load())
	if h.sinceSweep < topicSweepGrowth &&
		!atCeiling &&
		now.Sub(h.lastSweep) < topicSweepInterval {
		return
	}
	h.reapLocked(now)
	h.ceilingFutile = len(h.topics) >= h.maxTopics
}

// reapLocked drops subscriber-less topics with an empty buffer (nothing to replay) or idle past
// idleTTL (replay window expired). If the map is still at the ceiling, subscriber-less topics go
// least-recently-used first. Topics with a live subscriber are NEVER dropped.
//
// Caller holds h.mu for writing. Topic locks are taken underneath it, the one place the two are
// held together; no other path takes h.mu while holding a topic lock, so the order cannot invert.
func (h *Hub) reapLocked(now time.Time) {
	h.lastSweep = now
	h.sinceSweep = 0
	h.unsubscribed.Store(false)

	type candidate struct {
		name     string
		lastUsed time.Time
	}
	var idle []candidate
	for name, t := range h.topics {
		t.mu.Lock()
		unused := len(t.subs) == 0
		expired := unused && (t.bufferEmpty() || now.Sub(t.lastUsed) >= h.idleTTL)
		if expired {
			t.evicted = true
		}
		lastUsed := t.lastUsed
		t.mu.Unlock()
		switch {
		case expired:
			h.evictLocked(name)
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
			h.evictLocked(c.name)
		}
	}
}

// evictLocked removes a reaped topic and counts it. Caller holds h.mu
// for writing.
func (h *Hub) evictLocked(name string) {
	delete(h.topics, name)
	h.reaped.Add(1)
	obs.APIStreamHubTopicsReapedTotal.Inc()
}

// subscription is one active stream's per-Hub state. mu serializes trySend against close so a
// publisher never sends on a channel a concurrent cancel/drop closed (that panics the process);
// it is held only for a non-blocking select.
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

// sendReplay queues one buffered-replay event during Subscribe, returning false when the queue is
// full or the subscription is closed (the client then reconnects with a fresher Last-Event-ID).
// It shares trySend's mutex because replay runs while the subscription is already registered on
// earlier topics, so a concurrent drop could close sub.ch mid-send.
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

// close closes the subscriber channel exactly once, reporting whether
// this call was the one that closed it. Idempotent and mutually
// exclusive with trySend.
func (s *subscription) close() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return false
	}
	s.closed = true
	close(s.ch)
	return true
}
