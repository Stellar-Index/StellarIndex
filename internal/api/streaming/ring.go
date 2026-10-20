package streaming

// ring is a fixed-capacity event buffer; the oldest present event is the floor for Last-Event-ID
// resume. All access is serialised by the owning topicState's mutex; ring does NOT lock.
type ring struct {
	cap    int
	events []Event // len(events) ≤ cap
	// evicted is set the first time push discards an event; len(events) alone cannot tell a ring
	// that never wrapped from one that lost older events. See [ring.hasEvicted].
	evicted bool
}

func newRing(capacity int) *ring {
	return &ring{
		cap:    capacity,
		events: make([]Event, 0, capacity),
	}
}

// empty reports whether the ring holds no events: the topic was never published to (or was reaped and recreated), so a reconnecting client
// has nothing to replay.
func (r *ring) empty() bool { return len(r.events) == 0 }

// push appends ev, discarding the oldest at capacity. The slice stays ordered ascending by ID so snapshotAfter can do a linear scan.
func (r *ring) push(ev Event) {
	if len(r.events) < r.cap {
		r.events = append(r.events, ev)
		return
	}
	// Full: shift left by 1 and append. O(cap) is fine at cap 256 and about one push per second.
	r.evicted = true
	copy(r.events, r.events[1:])
	r.events[len(r.events)-1] = ev
}

// oldestID returns the oldest held event's ID, or "" if empty: the floor a client's replay can be trusted against, since anything strictly
// older was evicted.
func (r *ring) oldestID() string {
	if len(r.events) == 0 {
		return ""
	}
	return r.events[0].ID
}

// hasEvicted reports whether this ring has ever discarded an event. A cursor older than [ring.oldestID] is a genuine gap only
// then; otherwise the oldest event is the topic's first-ever publish, which a stale or synthetic cursor can predate with nothing
// lost.
func (r *ring) hasEvicted() bool { return r.evicted }

// snapshotAfter returns a copy, in ID order, of every buffered event with ID strictly greater than lastEventID (nil for an
// empty one; all held events if the cursor predates the oldest). IDs are timestamp-packed ([Generator]), so a gap is
// indistinguishable from a quiet period; [Hub.Subscribe] compares against [ring.oldestID] itself and emits an
// [EventTypeStreamGap] marker.
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
