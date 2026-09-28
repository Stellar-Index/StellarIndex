package streaming

// ring is a fixed-capacity event ring buffer. Newer events overwrite
// older ones once full — the oldest still-present event is the
// floor for Last-Event-ID resume.
//
// Single-writer, multi-reader: all access is serialised by the
// owning topicState's mutex; ring itself does NOT lock.
type ring struct {
	cap    int
	events []Event // len(events) ≤ cap
	// evicted is set the first time push discards an event. len(events)
	// alone can't tell a ring that has always fit under cap (its oldest
	// event is simply the topic's first-ever publish — no loss) apart
	// from one that has wrapped (its oldest event was NOT the first —
	// something older existed and is gone). See [ring.hasEvicted].
	evicted bool
}

func newRing(capacity int) *ring {
	return &ring{
		cap:    capacity,
		events: make([]Event, 0, capacity),
	}
}

// empty reports whether the ring holds no events — i.e. the topic has
// never been published to (or was reaped and recreated), so there is
// nothing a reconnecting client could replay from it.
func (r *ring) empty() bool { return len(r.events) == 0 }

// push appends ev. When the ring is at capacity, the oldest event
// is discarded. The slice is kept ordered ascending by ID so that
// snapshotAfter can do a linear scan.
func (r *ring) push(ev Event) {
	if len(r.events) < r.cap {
		r.events = append(r.events, ev)
		return
	}
	// Full — shift left by 1, append at tail. O(cap), but cap is
	// small (256 default) and pushes happen at the publisher's
	// rate (~ once per second worst-case for tip), so this is well
	// inside the per-publish budget.
	r.evicted = true
	copy(r.events, r.events[1:])
	r.events[len(r.events)-1] = ev
}

// oldestID returns the ID of the oldest event still held, or "" if the
// ring is empty. The floor a client's replay can be trusted against —
// anything strictly older was evicted and is gone (Refs #1035).
func (r *ring) oldestID() string {
	if len(r.events) == 0 {
		return ""
	}
	return r.events[0].ID
}

// hasEvicted reports whether this ring has ever discarded an event.
// A cursor older than [ring.oldestID] only signals a genuine gap when
// this is true — otherwise the ring simply hasn't filled yet and its
// oldest event is the topic's first-ever publish, which a stale or
// synthetic cursor can legitimately predate without anything having
// been lost.
func (r *ring) hasEvicted() bool { return r.evicted }

// snapshotAfter returns a copy of every buffered event with ID
// strictly greater than lastEventID. The returned slice is in
// publish (and therefore ID) order.
//
// When lastEventID is empty, no replay is wanted — returns nil.
// When lastEventID is older than the buffer's oldest event, returns
// EVERY buffered event still held. Events strictly between
// lastEventID and the oldest surviving one were dropped by eviction
// and are gone; IDs alone don't signal that loss (they are
// timestamp-packed — see [Generator] — not a per-topic sequence, so a
// gap in ID values is indistinguishable from a quiet period with no
// publishes), which is why [Hub.Subscribe] compares lastEventID
// against [ring.oldestID] itself and emits an [EventTypeStreamGap]
// marker when it detects the loss (Refs #1035).
func (r *ring) snapshotAfter(lastEventID string) []Event {
	if lastEventID == "" {
		return nil
	}
	out := make([]Event, 0, len(r.events))
	for i := range r.events {
		if r.events[i].ID > lastEventID {
			out = append(out, r.events[i])
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
