package v1

import (
	"context"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/api/streaming"
)

// TestForwardTipStream_ResumeSkipsPreflightSnapshot pins Q170/T161: on a
// Last-Event-ID reconnect, forwardTipStream must not prepend the
// connection's freshly-computed pre-flight snapshot ahead of the Hub's
// replayed backlog.
//
// firstEv is built under a connection-local [streaming.Generator] at
// reconnect time, so its ID is newer than anything already buffered.
// Sending it first and the (older) replay after it makes the SSE `id:`
// sequence go backwards, and — because the LAST frame received is what a
// client renders as current — leaves the fresher-looking-but-actually-
// buffered event as the one displayed, i.e. a stale price rendered as
// live.
//
// Unfixed code sends firstEv unconditionally; this asserts the resuming
// connection receives ONLY the replayed backlog event, preserving ID
// order.
func TestForwardTipStream_ResumeSkipsPreflightSnapshot(t *testing.T) {
	var s Server

	replay := streaming.Event{
		Type: "tip_update",
		Data: []byte(`{"price":"buffered"}`),
		ID:   "0000000000000001",
	}
	firstEv := streaming.Event{
		Type: "tip_update",
		Data: []byte(`{"price":"fresh"}`),
		ID:   "00000000000000ff",
	}

	sub := make(chan streaming.Event, 1)
	sub <- replay
	close(sub)

	ch := make(chan streaming.Event, 4)
	s.forwardTipStream(context.Background(), ch, sub, firstEv, true /* isResume */)

	var got []streaming.Event
	for ev := range ch {
		got = append(got, ev)
	}

	if len(got) != 1 {
		t.Fatalf("resume: got %d events %+v, want exactly the one replayed backlog event", len(got), got)
	}
	if got[0].ID != replay.ID {
		t.Fatalf("resume: first (and only) frame ID = %q, want the replayed backlog event %q — "+
			"a freshly-computed snapshot must not jump ahead of buffered events on reconnect", got[0].ID, replay.ID)
	}
}

// TestForwardTipStream_FreshConnectStillLeadsWithSnapshot is the sibling
// case: a fresh connect (no Last-Event-ID) has nothing buffered to
// replay ([streaming.Hub.Subscribe] skips replay when lastEventID is
// empty), so the pre-flight snapshot must still lead — the fix must not
// regress the "first frame doesn't wait for the next tick" behaviour the
// snapshot exists for.
func TestForwardTipStream_FreshConnectStillLeadsWithSnapshot(t *testing.T) {
	var s Server

	live := streaming.Event{
		Type: "tip_update",
		Data: []byte(`{"price":"live"}`),
		ID:   "00000000000000ff",
	}
	firstEv := streaming.Event{
		Type: "tip_update",
		Data: []byte(`{"price":"fresh"}`),
		ID:   "0000000000000001",
	}

	sub := make(chan streaming.Event, 1)
	sub <- live
	close(sub)

	ch := make(chan streaming.Event, 4)
	done := make(chan struct{})
	go func() {
		s.forwardTipStream(context.Background(), ch, sub, firstEv, false /* isResume */)
		close(done)
	}()

	select {
	case ev := <-ch:
		if ev.ID != firstEv.ID {
			t.Fatalf("fresh connect: first frame ID = %q, want the pre-flight snapshot %q", ev.ID, firstEv.ID)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for the pre-flight snapshot frame")
	}

	select {
	case ev, open := <-ch:
		if !open {
			t.Fatal("channel closed before the forwarded live event")
		}
		if ev.ID != live.ID {
			t.Fatalf("fresh connect: second frame ID = %q, want the forwarded live event %q", ev.ID, live.ID)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for the forwarded live event")
	}

	<-done
}
