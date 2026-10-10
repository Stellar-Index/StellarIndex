package streaming_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/Stellar-Index/StellarIndex/internal/api/streaming"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
)

// TestHub_SlowSubscriberDropIsMetered — a slow-consumer disconnect is
// counted exactly once, even for a subscriber on two topics, and an
// ordinary client cancel is not counted at all.
func TestHub_SlowSubscriberDropIsMetered(t *testing.T) {
	hub := streaming.NewHub(0)
	before := testutil.ToFloat64(obs.APIStreamSubscriberDropsTotal)

	slow := mustSubscribe(t, hub, []string{"a", "b"}, "")
	_, cancelQuiet, err := hub.Subscribe([]string{"c"}, "")
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	cancelQuiet()

	for i := 0; i < 64; i++ {
		hub.Publish("a", "x", []byte("payload"))
		hub.Publish("b", "x", []byte("payload"))
	}
	for range slow {
	}

	if got := testutil.ToFloat64(obs.APIStreamSubscriberDropsTotal) - before; got != 1 {
		t.Fatalf("stellarindex_api_stream_subscriber_drops_total delta = %v, want 1 "+
			"(one slow subscriber, dropped once)", got)
	}
}

// TestHub_TopicMetricsTrackReaping — the topic gauge and reaped counter
// are exported, not only readable through the Hub's accessors.
func TestHub_TopicMetricsTrackReaping(t *testing.T) {
	hub := streaming.NewHub(0)
	before := testutil.ToFloat64(obs.APIStreamHubTopicsReapedTotal)

	churnTopics(t, hub, 300)

	if got := testutil.ToFloat64(obs.APIStreamHubTopicsReapedTotal) - before; got != float64(hub.TopicsReaped()) || got == 0 {
		t.Fatalf("reaped counter delta = %v, want TopicsReaped() = %d (> 0)", got, hub.TopicsReaped())
	}
	if got := testutil.ToFloat64(obs.APIStreamHubTopics); got != float64(hub.TopicCount()) {
		t.Fatalf("topics gauge = %v, want TopicCount() = %d", got, hub.TopicCount())
	}
}

// TestStream_ActiveAndRejectedStreamsAreMetered — the concurrency caps'
// state is exported: an open stream holds the active gauge up, a
// per-IP refusal is counted under its reason, and closing the stream
// releases the gauge.
func TestStream_ActiveAndRejectedStreamsAreMetered(t *testing.T) {
	streaming.SetMaxStreamsPerIP(1)
	defer streaming.SetMaxStreamsPerIP(0)
	streaming.SetStreamClientIPResolver(func(*http.Request) string { return "metered-client" })
	defer streaming.SetStreamClientIPResolver(nil)

	hub := streaming.NewHub(0)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		streaming.Stream(w, r, hub, []string{"topic"}, streaming.StreamOptions{HeartbeatInterval: 30 * time.Second})
	}))
	defer srv.Close()

	activeBefore := testutil.ToFloat64(obs.APISSEStreamsActive)
	rejected := obs.APISSEStreamsRejectedTotal.WithLabelValues("per_ip_cap")
	rejectedBefore := testutil.ToFloat64(rejected)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	held, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("open first stream: %v", err)
	}
	defer held.Body.Close()
	if got := testutil.ToFloat64(obs.APISSEStreamsActive) - activeBefore; got != 1 {
		t.Fatalf("active streams gauge delta with one open stream = %v, want 1", got)
	}

	resp, err := srv.Client().Get(srv.URL)
	if err != nil {
		t.Fatalf("open over-cap stream: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("over-cap status = %d, want 503", resp.StatusCode)
	}
	if got := testutil.ToFloat64(rejected) - rejectedBefore; got != 1 {
		t.Fatalf("rejected{per_ip_cap} delta = %v, want 1", got)
	}

	cancel()
	held.Body.Close()
	deadline := time.Now().Add(2 * time.Second)
	for testutil.ToFloat64(obs.APISSEStreamsActive) != activeBefore {
		if time.Now().After(deadline) {
			t.Fatalf("active streams gauge = %v after close, want %v",
				testutil.ToFloat64(obs.APISSEStreamsActive), activeBefore)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// mustSubscribe subscribes and cancels on test cleanup.
func mustSubscribe(t *testing.T, hub *streaming.Hub, topics []string, lastEventID string) <-chan streaming.Event {
	t.Helper()
	ch, cancel, err := hub.Subscribe(topics, lastEventID)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	t.Cleanup(cancel)
	return ch
}

// drainNonblocking returns events from ch until either it has
// accumulated `want` items or `timeout` elapses. Used to assert
// fanout under timing without busy-waiting in test code.
func drainNonblocking(t *testing.T, ch <-chan streaming.Event, want int, timeout time.Duration) []streaming.Event {
	t.Helper()
	out := make([]streaming.Event, 0, want)
	deadline := time.After(timeout)
	for len(out) < want {
		select {
		case ev, ok := <-ch:
			if !ok {
				return out
			}
			out = append(out, ev)
		case <-deadline:
			return out
		}
	}
	return out
}

// TestHub_Fanout — two subscribers on the same topic both receive
// every published event in order, with monotonically increasing IDs.
func TestHub_Fanout(t *testing.T) {
	hub := streaming.NewHub(0)
	subA := mustSubscribe(t, hub, []string{"price:XLM/USD"}, "")
	subB := mustSubscribe(t, hub, []string{"price:XLM/USD"}, "")

	for _, p := range []string{"0.10", "0.11", "0.12"} {
		hub.Publish("price:XLM/USD", "price_update", []byte(`{"p":"`+p+`"}`))
	}

	gotA := drainNonblocking(t, subA, 3, time.Second)
	gotB := drainNonblocking(t, subB, 3, time.Second)
	if len(gotA) != 3 || len(gotB) != 3 {
		t.Fatalf("fanout = (%d, %d), want (3, 3)", len(gotA), len(gotB))
	}
	for _, set := range [][]streaming.Event{gotA, gotB} {
		for i := 1; i < len(set); i++ {
			if set[i-1].ID >= set[i].ID {
				t.Errorf("IDs not strictly increasing: %q !< %q", set[i-1].ID, set[i].ID)
			}
		}
	}
}

// TestHub_TopicIsolation — a publisher on topic A doesn't reach a
// subscriber on topic B.
func TestHub_TopicIsolation(t *testing.T) {
	hub := streaming.NewHub(0)
	subA := mustSubscribe(t, hub, []string{"topicA"}, "")
	subB := mustSubscribe(t, hub, []string{"topicB"}, "")

	hub.Publish("topicA", "x", []byte("hello"))

	gotA := drainNonblocking(t, subA, 1, 500*time.Millisecond)
	gotB := drainNonblocking(t, subB, 1, 200*time.Millisecond)
	if len(gotA) != 1 {
		t.Errorf("topicA expected 1 event, got %d", len(gotA))
	}
	if len(gotB) != 0 {
		t.Errorf("topicB expected 0 events, got %d", len(gotB))
	}
}

// TestHub_MultiTopicSubscription — one Subscribe call covering two
// topics receives events from both.
func TestHub_MultiTopicSubscription(t *testing.T) {
	hub := streaming.NewHub(0)
	sub := mustSubscribe(t, hub, []string{"topicA", "topicB"}, "")

	hub.Publish("topicA", "x", []byte("a"))
	hub.Publish("topicB", "x", []byte("b"))

	got := drainNonblocking(t, sub, 2, time.Second)
	if len(got) != 2 {
		t.Fatalf("got %d events, want 2", len(got))
	}
	seen := map[string]bool{}
	for _, ev := range got {
		seen[string(ev.Data)] = true
	}
	if !seen["a"] || !seen["b"] {
		t.Errorf("missing events: %v", seen)
	}
}

// TestHub_ResumeReplaysEventsAfterCursor — events published before Subscribe
// are replayed, in order, when the Last-Event-ID is older than the newest
// buffered event. When the ring is full the oldest event is evicted, and
// resuming from the evicted ID returns whatever remains.
func TestHub_ResumeReplaysEventsAfterCursor(t *testing.T) {
	tests := []struct {
		name       string
		bufferSize int
		publish    []string
		want       []string
		wantGap    bool // the evicted cursor is announced by a stream-gap marker first
	}{
		{"replay from buffer", 0, []string{"first", "second", "third"}, []string{"second", "third"}, false},
		{"evicted cursor replays the remainder", 2, []string{"e1", "e2", "e3"}, []string{"e2", "e3"}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hub := streaming.NewHub(tt.bufferSize)
			var cursor string
			for i, data := range tt.publish {
				id := hub.Publish("topic", "x", []byte(data))
				if i == 0 {
					cursor = id
				}
			}

			sub := mustSubscribe(t, hub, []string{"topic"}, cursor)

			wantN := len(tt.want)
			if tt.wantGap {
				wantN++
			}
			got := drainNonblocking(t, sub, wantN, time.Second)
			if len(got) != wantN {
				t.Fatalf("replay returned %d events, want %d: %v", len(got), wantN, got)
			}
			if tt.wantGap {
				if got[0].Type != streaming.EventTypeStreamGap {
					t.Fatalf("first event type = %q, want %q", got[0].Type, streaming.EventTypeStreamGap)
				}
				got = got[1:]
			}
			for i, ev := range got {
				if string(ev.Data) != tt.want[i] {
					t.Errorf("replay[%d] = %q, want %q", i, ev.Data, tt.want[i])
				}
			}
		})
	}
}

// TestHub_EmptyLastEventIDSkipsReplay — an empty resume cursor
// means "I want only future events" and should NOT replay anything
// from the buffer.
func TestHub_EmptyLastEventIDSkipsReplay(t *testing.T) {
	hub := streaming.NewHub(0)
	hub.Publish("topic", "x", []byte("buffered-1"))
	hub.Publish("topic", "x", []byte("buffered-2"))

	sub := mustSubscribe(t, hub, []string{"topic"}, "")

	// Nothing should arrive from replay — only the live event below.
	got := drainNonblocking(t, sub, 1, 200*time.Millisecond)
	if len(got) != 0 {
		t.Errorf("empty cursor replayed %d events, want 0", len(got))
	}

	hub.Publish("topic", "x", []byte("live"))
	got = drainNonblocking(t, sub, 1, time.Second)
	if len(got) != 1 || string(got[0].Data) != "live" {
		t.Errorf("live event missed: %v", got)
	}
}

// TestHub_SlowSubscriberDropped — a subscriber that never reads is
// evicted once its queue fills, freeing the publish path. Other
// subscribers continue to receive events.
func TestHub_SlowSubscriberDropped(t *testing.T) {
	hub := streaming.NewHub(0)
	// Slow sub: never reads. Its 32-deep queue will fill.
	slow := mustSubscribe(t, hub, []string{"topic"}, "")

	fast := mustSubscribe(t, hub, []string{"topic"}, "")

	// Publish 64 events — twice the per-subscriber queue depth.
	for i := 0; i < 64; i++ {
		hub.Publish("topic", "x", []byte("payload"))
	}

	// Slow sub's channel should now be closed. Signal completion
	// over a channel rather than a shared bool — the race detector
	// flags the latter (the drain goroutine writes while the main
	// goroutine reads).
	slowDrained := make(chan struct{})
	go func() {
		for range slow {
		}
		close(slowDrained)
	}()

	gotFast := drainNonblocking(t, fast, 64, 2*time.Second)
	if len(gotFast) < 32 {
		t.Errorf("fast subscriber starved: got %d events", len(gotFast))
	}

	select {
	case <-slowDrained:
		// channel closed → drain goroutine exited → drop confirmed.
	case <-time.After(time.Second):
		t.Error("slow subscriber's channel was not closed after overflow")
	}
}

// TestHub_PublishRaceFreeIDs — concurrent Publish calls produce
// strictly distinct IDs. Sanity check on the ID generator's CAS
// loop.
func TestHub_PublishRaceFreeIDs(t *testing.T) {
	hub := streaming.NewHub(0)
	const goroutines = 8
	const perG = 100

	var wg sync.WaitGroup
	ids := make(chan string, goroutines*perG)
	wg.Add(goroutines)
	for g := 0; g < goroutines; g++ {
		go func() {
			defer wg.Done()
			for i := 0; i < perG; i++ {
				ids <- hub.Publish("topic", "x", []byte("p"))
			}
		}()
	}
	wg.Wait()
	close(ids)

	seen := make(map[string]struct{}, goroutines*perG)
	for id := range ids {
		if _, dup := seen[id]; dup {
			t.Fatalf("duplicate ID under concurrency: %q", id)
		}
		seen[id] = struct{}{}
		if len(id) != 16 {
			t.Errorf("ID length = %d, want 16", len(id))
		}
		if strings.ContainsAny(id, "ghijklmnopqrstuvwxyz") {
			t.Errorf("ID contains non-hex chars: %q", id)
		}
	}
	if len(seen) != goroutines*perG {
		t.Errorf("unique IDs = %d, want %d", len(seen), goroutines*perG)
	}
}

// TestHub_PublishVsCancelRace pins that a subscriber cancelling (which
// closes its channel) concurrently with Publish (which sends off the topic
// lock) must never panic with "send on closed channel". Without the guard,
// the select send-case on a closed channel would be "ready" and chosen over
// default, crashing the whole process. Run under -race.
func TestHub_PublishVsCancelRace(t *testing.T) {
	hub := streaming.NewHub(0)
	const workers = 8
	const iters = 500

	var wg sync.WaitGroup
	// Publishers hammer the topic continuously.
	stop := make(chan struct{})
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					hub.Publish("race:topic", "x", []byte("payload"))
				}
			}
		}()
	}

	// Churners subscribe + cancel repeatedly, racing the close against the
	// publishers' sends. A drained-slowly channel also exercises the
	// full->drop path.
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < iters; j++ {
				ch, cancel, err := hub.Subscribe([]string{"race:topic"}, "")
				if err != nil {
					t.Errorf("Subscribe: %v", err)
					return
				}
				// Read a couple so the queue can fill on some iterations.
				select {
				case <-ch:
				default:
				}
				cancel()
				// Draining after cancel must not panic either.
				for range ch {
				}
			}
		}()
	}

	// Let the churners finish, then stop publishers.
	go func() {
		// crude join: wait until all churner goroutines are done by
		// sleeping proportional to work, then stop.
		time.Sleep(2 * time.Second)
		close(stop)
	}()
	wg.Wait()
	// If we got here without a panic, the race is fixed.
}

// TestHub_SubscribeKeepsEventsPublishedDuringSubscribe pins the
// subscribe/publish gap: an event published while Subscribe is running
// must end up in EITHER the replay OR the live fanout — never neither.
//
// A Subscribe that snapshotted every topic's buffer first and
// only then registered as a live listener would lose an event that landed in
// between: it is in no snapshot and has no listener, so silently dropped,
// and indistinguishable at the client from a buffer overrun.
//
// The subscription covers many topics because that is what makes the
// window wide enough to hit reliably (a snapshot-first Subscribe does ALL the
// replay work before ANY registration). The same gap exists on the
// single-topic production path — it is just microseconds wide, which
// is a flaky test, not a safe one. The invariant is
// structural, so the assertion is an exact zero.
func TestHub_SubscribeKeepsEventsPublishedDuringSubscribe(t *testing.T) {
	const iterations = 200
	const perIteration = 5

	lost := 0
	for i := 0; i < iterations; i++ {
		hub := streaming.NewHub(0)

		// Seed a buffer so Subscribe has replay work to do, and resume
		// from the newest seeded ID so replay carries only the events
		// the publisher below races in.
		var resumeFrom string
		for j := 0; j < 8; j++ {
			resumeFrom = hub.Publish("topic-0", "x", []byte("seed"))
		}
		topics := []string{"topic-0"}
		for j := 1; j < 64; j++ {
			topics = append(topics, fmt.Sprintf("topic-%d", j))
		}

		var (
			wg        sync.WaitGroup
			mu        sync.Mutex
			published []string
		)
		wg.Add(1)
		go func() {
			defer wg.Done()
			for k := 0; k < perIteration; k++ {
				id := hub.Publish("topic-0", "x", []byte("live"))
				mu.Lock()
				published = append(published, id)
				mu.Unlock()
			}
		}()

		ch, cancel, err := hub.Subscribe(topics, resumeFrom)
		if err != nil {
			t.Fatalf("Subscribe: %v", err)
		}
		wg.Wait() // every Publish has returned → every delivery is done

		got := make(map[string]bool, perIteration)
	drain:
		for {
			select {
			case ev, ok := <-ch:
				if !ok {
					break drain
				}
				got[ev.ID] = true
			default:
				break drain
			}
		}
		mu.Lock()
		for _, id := range published {
			if !got[id] {
				lost++
			}
		}
		mu.Unlock()
		cancel()
	}

	if lost != 0 {
		t.Fatalf("%d of %d events published during Subscribe were delivered neither as replay "+
			"nor live", lost, iterations*perIteration)
	}
}

// TestHub_ReplayBeyondQueueDepthKeepsConnection pins replay-overflow handling.
//
// A Subscribe that pushed the whole replay set into a 32-deep channel before
// any reader existed, and CLOSED the subscription on overflow, would give a
// client resuming more than 32 events behind the 32 OLDEST
// buffered events — the stalest prices in the ring — and then
// disconnect it: every reconnect would return exactly 32
// events and close in 6-8ms, so a client 20 minutes behind would grind
// through ~8 reconnect rounds rendering stale prices as live.
//
// The replay must deliver the NEWEST events that fit and keep the
// subscription open for live delivery. It is also budgeted at HALF the
// channel capacity a full-capacity replay leaves no headroom,
// so the very next live Publish would find the channel full and evict the
// subscriber it had just resumed.
func TestHub_ReplayBeyondQueueDepthKeepsConnection(t *testing.T) {
	h := streaming.NewHub(256)
	const published = 32 * 3
	for i := 0; i < published; i++ {
		h.Publish("t", "price_update", []byte(`{"n":`+strconv.Itoa(i)+`}`))
	}

	ch := mustSubscribe(t, h, []string{"t"}, "0")

	const wantReplay = 16 // subscriberQueueDepth/2, single topic
	var got []streaming.Event
drain:
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				t.Fatal("subscription was CLOSED during replay — a client resuming beyond the queue depth must be truncated, not disconnected")
			}
			got = append(got, ev)
			if len(got) == wantReplay {
				break drain
			}
		case <-time.After(2 * time.Second):
			break drain
		}
	}

	if len(got) != wantReplay {
		t.Fatalf("replayed %d events, want %d", len(got), wantReplay)
	}
	// The retained window must be the NEWEST slice, not the oldest.
	if !strings.Contains(string(got[len(got)-1].Data), strconv.Itoa(published-1)) {
		t.Errorf("last replayed event = %s, want the most recent (n=%d) — replaying the oldest hands the client the stalest prices in the ring",
			got[len(got)-1].Data, published-1)
	}

	// And the subscription must still be live.
	liveID := h.Publish("t", "price_update", []byte(`{"live":true}`))
	select {
	case ev, ok := <-ch:
		if !ok {
			t.Fatal("channel closed instead of delivering the live event")
		}
		if ev.ID != liveID {
			t.Errorf("live event ID = %q, want %q", ev.ID, liveID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no live event after replay — the subscriber was not registered for fanout")
	}
}

// TestHub_ResumeReplayLeavesHeadroom pins the replay headroom:
// a resume clamped at the FULL channel capacity leaves no room for the
// next live Publish, which finds the channel full and evicts the
// subscriber it had just resumed. The channel must come back with
// spare capacity, single-topic or multi-topic, and every subscribed
// topic must contribute at least one replayed event — not just the
// first.
func TestHub_ResumeReplayLeavesHeadroom(t *testing.T) {
	t.Run("single topic", func(t *testing.T) {
		h := streaming.NewHub(256)
		for i := 0; i < 64; i++ {
			h.Publish("t", "price_update", []byte("seed"))
		}
		ch := mustSubscribe(t, h, []string{"t"}, "0")

		// Drain nothing — check queue occupancy against capacity right
		// after Subscribe, as a live Publish would race against it.
		if len(ch) >= cap(ch) {
			t.Fatalf("replay filled the channel (%d/%d) — a live publish right after resume evicts the subscriber", len(ch), cap(ch))
		}
	})

	t.Run("multi topic replays every topic", func(t *testing.T) {
		h := streaming.NewHub(256)
		// Topic "a" alone has more buffered events than the whole
		// channel can hold, so an unbudgeted per-topic clamp lets it
		// fill the channel by itself before topic "b" is ever
		// registered.
		for i := 0; i < 40; i++ {
			h.Publish("a", "price_update", []byte("seed-a"))
		}
		h.Publish("b", "price_update", []byte("seed-b"))
		ch := mustSubscribe(t, h, []string{"a", "b"}, "0")

		if len(ch) >= cap(ch) {
			t.Fatalf("replay filled the channel (%d/%d) across topics", len(ch), cap(ch))
		}

		topics := map[string]bool{}
	drain:
		for {
			select {
			case ev := <-ch:
				if strings.Contains(string(ev.Data), "seed-a") {
					topics["a"] = true
				}
				if strings.Contains(string(ev.Data), "seed-b") {
					topics["b"] = true
				}
			default:
				break drain
			}
		}
		if !topics["a"] || !topics["b"] {
			t.Fatalf("expected replay from both subscribed topics, got %v — the first topic must not consume the whole replay budget", topics)
		}
	})
}

// TestHub_ReconnectIDsAreMonotonic pins reconnect ordering:
// a resuming subscriber must never see an event ID
// smaller than or equal to the resume cursor's own ID, and never out
// of order across the replayed set.
func TestHub_ReconnectIDsAreMonotonic(t *testing.T) {
	h := streaming.NewHub(256)
	var resumeFrom string
	for i := 0; i < 8; i++ {
		resumeFrom = h.Publish("t", "price_update", []byte("seed"))
	}

	ch := mustSubscribe(t, h, []string{"t"}, resumeFrom)

	liveID := h.Publish("t", "price_update", []byte("live"))

	var lastID string
drain:
	for {
		select {
		case ev := <-ch:
			if lastID != "" && ev.ID <= lastID {
				t.Fatalf("non-monotonic id sequence: %q then %q", lastID, ev.ID)
			}
			lastID = ev.ID
		case <-time.After(time.Second):
			break drain
		}
	}
	if lastID != liveID {
		t.Fatalf("last delivered id = %q, want live event id %q", lastID, liveID)
	}
}

// TestHub_MultiTopicReplayIsMergedByID pins merged replay order:
// ids come from one Hub-wide generator but a
// multi-topic Subscribe that queued each topic's whole replay before
// the next would make the wire `id:` line walk backwards at the topic
// boundary. Interleaving publishes across two topics and resuming from
// before all of them must come back in strict id order, not grouped by
// topic.
func TestHub_MultiTopicReplayIsMergedByID(t *testing.T) {
	hub := streaming.NewHub(0)
	cursor := hub.Publish("seed", "x", []byte("seed"))

	idA1 := hub.Publish("topicA", "x", []byte("a1"))
	idB1 := hub.Publish("topicB", "x", []byte("b1"))
	idA2 := hub.Publish("topicA", "x", []byte("a2"))
	idB2 := hub.Publish("topicB", "x", []byte("b2"))

	sub := mustSubscribe(t, hub, []string{"topicA", "topicB"}, cursor)

	got := drainNonblocking(t, sub, 4, time.Second)
	if len(got) != 4 {
		t.Fatalf("replay returned %d events, want 4: %+v", len(got), got)
	}
	wantOrder := []string{idA1, idB1, idA2, idB2}
	for i, ev := range got {
		if ev.ID != wantOrder[i] {
			t.Fatalf("event %d: id = %q, want %q (grouped-by-topic order instead of merged-by-id); full sequence: %v",
				i, ev.ID, wantOrder[i], idsOf(got))
		}
	}
}

func idsOf(evs []streaming.Event) []string {
	out := make([]string, len(evs))
	for i, ev := range evs {
		out[i] = ev.ID
	}
	return out
}

// TestHub_ReplayGapEmitsStreamGapMarker pins the gap marker:
// a resume whose cursor names an event the ring has already
// evicted must be preceded by an [streaming.EventTypeStreamGap] marker
// naming the requested cursor and the oldest id still available —
// without it nothing on the wire distinguishes a truncated replay from a
// complete one.
func TestHub_ReplayGapEmitsStreamGapMarker(t *testing.T) {
	hub := streaming.NewHub(2) // tiny ring: forces eviction
	cursor := hub.Publish("seed", "x", []byte("seed"))
	hub.Publish("topic", "x", []byte("first")) // evicted by "third"
	idSecond := hub.Publish("topic", "x", []byte("second"))
	hub.Publish("topic", "x", []byte("third"))

	sub := mustSubscribe(t, hub, []string{"topic"}, cursor)

	got := drainNonblocking(t, sub, 3, time.Second)
	if len(got) != 3 {
		t.Fatalf("got %d events, want 3 (1 gap marker + second + third): %+v", len(got), got)
	}
	if got[0].Type != streaming.EventTypeStreamGap {
		t.Fatalf("first event type = %q, want %q", got[0].Type, streaming.EventTypeStreamGap)
	}
	if got[0].ID != "" {
		t.Errorf("gap marker carries an id (%q) — it must not perturb the client's resume cursor", got[0].ID)
	}
	var payload struct {
		Topic          string `json:"topic"`
		RequestedAfter string `json:"requested_after"`
		ResumedFrom    string `json:"resumed_from"`
	}
	if err := json.Unmarshal(got[0].Data, &payload); err != nil {
		t.Fatalf("gap marker data not JSON: %v (%s)", err, got[0].Data)
	}
	if payload.Topic != "topic" || payload.RequestedAfter != cursor || payload.ResumedFrom != idSecond {
		t.Errorf("gap payload = %+v, want topic=topic requested_after=%s resumed_from=%s", payload, cursor, idSecond)
	}
	if string(got[1].Data) != "second" || string(got[2].Data) != "third" {
		t.Errorf("replay after gap = %q, %q, want second, third", got[1].Data, got[2].Data)
	}
}

// TestDoc_MaxTopicsIsNotAHardCeiling pins doc.go's package overview to
// the reap-threshold behaviour hub.go's DefaultMaxTopics documents
// — a stale "caps the map regardless" claim would send an
// operator sizing memory against a bound that does not exist. The
// comment markers are stripped and the text re-joined on whitespace
// before matching, so a phrase that got re-wrapped across lines can't
// hide from the check.
func TestDoc_MaxTopicsIsNotAHardCeiling(t *testing.T) {
	src, err := os.ReadFile("doc.go")
	if err != nil {
		t.Fatalf("read doc.go: %v", err)
	}
	var stripped []string
	for _, line := range strings.Split(string(src), "\n") {
		stripped = append(stripped, strings.TrimPrefix(strings.TrimSpace(line), "//"))
	}
	text := strings.Join(strings.Fields(strings.Join(stripped, " ")), " ")
	if strings.Contains(text, "caps the map regardless") {
		t.Fatalf("doc.go still claims DefaultMaxTopics caps the map regardless; hub.go documents it as a reap threshold, not a hard ceiling")
	}
	if !strings.Contains(text, "BufferedTopicCount") {
		t.Fatalf("doc.go accessor list omits Hub.BufferedTopicCount, the accessor that now tracks memory since rings allocate lazily")
	}
}

func TestHub_ForeignFutureCursorEmitsStreamGap(t *testing.T) {
	hub := streaming.NewHub(8)
	hub.Publish("topic", "x", []byte("one"))

	// A cursor minted by a process whose clock is far ahead sorts above
	// the whole ring: without a marker the resume looks clean.
	future := fmt.Sprintf("%016x", uint64(time.Now().Add(time.Hour).UnixMilli())<<16)
	sub := mustSubscribe(t, hub, []string{"topic"}, future)

	got := drainNonblocking(t, sub, 1, time.Second)
	if len(got) != 1 || got[0].Type != streaming.EventTypeStreamGap {
		t.Fatalf("want one %q marker, got %+v", streaming.EventTypeStreamGap, got)
	}
}

func TestHub_OwnCursorEmitsNoStreamGap(t *testing.T) {
	hub := streaming.NewHub(8)
	cursor := hub.Publish("topic", "x", []byte("one"))

	sub := mustSubscribe(t, hub, []string{"topic"}, cursor)

	if got := drainNonblocking(t, sub, 1, 100*time.Millisecond); len(got) != 0 {
		t.Fatalf("own-space cursor produced events: %+v", got)
	}
}

// Replay rings are allocated on first PUBLISH, not on subscribe.
//
// The reaper evicts only SUBSCRIBER-LESS topics, so the map can hold up
// to maxTopics subscribed-but-silent topics. With the ring allocated
// eagerly, that made resident memory scale with
// concurrent streams × alias fan-out — /v1/price/stream subscribes one
// connection to assetAliases(base) × assetAliases(quote), up to 9
// topics, of which the aggregator publishes to at most a few — so the
// never-published remainder alone reserved ~20 KiB apiece for rings that
// could never hold an event.
//
// Proven red against the unfixed Hub (getOrCreateTopic allocating
// `buffer: newRing(h.bufferSize)`): BufferedTopicCount reached the full
// subscribed count and the per-topic cost measured ~20.7 KiB against the
// 2 KiB budget below.

// subscriberOnlyTopics is large enough that a 20 KiB-per-topic ring is
// unmistakable against measurement noise (~400 MiB eager vs ~7 MiB
// lazy), and small enough to stay cheap once fixed.
const subscriberOnlyTopics = 20000

// lazyRingBudgetBytes is the per-subscribed-topic memory budget. A
// topicState plus its map entry and key is a few hundred bytes; an
// eagerly-allocated 256-event ring is ~20 KiB. 2 KiB sits an order of
// magnitude clear of both, so this fails on the defect without being
// sensitive to allocator noise.
const lazyRingBudgetBytes = 2048

func TestHub_SubscribedButUnpublishedTopicsAllocateNoRing(t *testing.T) {
	hub := streaming.NewHub(0)
	// Admit the whole measured set; the ceiling itself is pinned elsewhere.
	hub.SetMaxTopics(subscriberOnlyTopics)

	topics := make([]string, 0, subscriberOnlyTopics)
	for i := range subscriberOnlyTopics {
		// The client-supplied key shape: an arbitrary pair/window that
		// no publisher will ever write to.
		topics = append(topics, fmt.Sprintf("closed:native/fiat:USD/%d", i+1))
	}

	before := heapInUse()
	_, cancel, err := hub.Subscribe(topics, "")
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer cancel()
	after := heapInUse()

	if got := hub.TopicCount(); got != subscriberOnlyTopics {
		t.Fatalf("TopicCount() = %d, want %d — the subscription did not mint the "+
			"topics this test is measuring", got, subscriberOnlyTopics)
	}
	if got := hub.BufferedTopicCount(); got != 0 {
		t.Errorf("BufferedTopicCount() = %d after subscribing to %d topics with no "+
			"publisher, want 0 — a topic nothing has published to has nothing to "+
			"replay, so its ring is pure cost", got, subscriberOnlyTopics)
	}

	perTopic := int64(after-before) / int64(subscriberOnlyTopics)
	if perTopic >= lazyRingBudgetBytes {
		t.Errorf("subscriber-only topics cost %d bytes each (%d topics, %d bytes total), "+
			"want under %d — at the shipped 8192-stream cap × 9 alias topics that is "+
			"%.1f GiB of rings that can never hold an event",
			perTopic, subscriberOnlyTopics, int64(after-before), lazyRingBudgetBytes,
			float64(perTopic)*8192*9/(1<<30))
	}
	t.Logf("subscriber-only topic cost: %d bytes each", perTopic)
}

// The ring must still exist — and still replay — the moment a topic
// actually carries an event. Lazy must not mean absent.
func TestHub_PublishAllocatesTheRingAndReplayStillWorks(t *testing.T) {
	hub := streaming.NewHub(0)

	quiet, cancelQuiet, err := hub.Subscribe([]string{"closed:native/fiat:USD/7"}, "")
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer cancelQuiet()
	if got := hub.BufferedTopicCount(); got != 0 {
		t.Fatalf("BufferedTopicCount() = %d before any publish, want 0", got)
	}

	first := hub.Publish("tip:native/fiat:USD/5", "tip_update", []byte(`{"v":"1"}`))
	hub.Publish("tip:native/fiat:USD/5", "tip_update", []byte(`{"v":"2"}`))

	if got := hub.BufferedTopicCount(); got != 1 {
		t.Errorf("BufferedTopicCount() = %d after publishing to one topic, want 1 — "+
			"a published topic MUST hold a replay ring", got)
	}
	if got := hub.TopicCount(); got != 2 {
		t.Errorf("TopicCount() = %d, want 2", got)
	}

	// Resume from the first event: the second must replay.
	sub, cancel, err := hub.Subscribe([]string{"tip:native/fiat:USD/5"}, first)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer cancel()
	select {
	case ev := <-sub:
		if string(ev.Data) != `{"v":"2"}` {
			t.Errorf("replayed event data = %q, want %q", ev.Data, `{"v":"2"}`)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no replay after resuming from the first event id — the lazily " +
			"allocated ring did not retain the published event")
	}
	_ = quiet
}

// A never-published topic still reaps on its last unsubscribe: the
// "nothing to replay" arm must read a missing ring as empty, not panic
// and not treat it as a buffer worth keeping for the idle TTL.
func TestHub_NeverPublishedTopicStillReapsOnLastUnsubscribe(t *testing.T) {
	hub := streaming.NewHub(0)
	hub.SetTopicIdleTTL(time.Hour)
	// The reaper runs opportunistically on topic CREATION; a threshold of
	// 1 makes the next creation trigger a pass deterministically.
	hub.SetMaxTopics(1)

	_, cancel, err := hub.Subscribe([]string{"closed:native/fiat:USD/11"}, "")
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	if got := hub.TopicCount(); got != 1 {
		t.Fatalf("TopicCount() = %d, want 1", got)
	}
	cancel()

	// Minting a topic triggers the pass that should drop it.
	hub.Publish("tip:native/fiat:USD/5", "tip_update", []byte(`{"v":"1"}`))
	if got := hub.TopicCount(); got != 1 {
		t.Errorf("TopicCount() = %d after the subscriber-less unpublished topic "+
			"should have been reaped, want 1 (the published topic only)", got)
	}
	if got := hub.BufferedTopicCount(); got != 1 {
		t.Errorf("BufferedTopicCount() = %d, want 1", got)
	}
}

// heapInUse returns live heap bytes after settling the collector.
func heapInUse() uint64 {
	runtime.GC()
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return m.HeapAlloc
}

// churnTopics opens and immediately cancels a subscription on n
// distinct never-published topics — the shape of a client cycling
// through made-up pairs.
func churnTopics(t *testing.T, hub *streaming.Hub, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		_, cancel, err := hub.Subscribe([]string{fmt.Sprintf("closed:CHURN%d/USD", i)}, "")
		if err != nil {
			t.Fatalf("Subscribe: %v", err)
		}
		cancel()
	}
}

// TestHub_ReapDropsMintedTopicsButKeepsReplayBuffers pins the halves
// of the retention policy against each other: topics minted
// by a client and never published to are dropped as soon as they have
// no subscriber, while a real topic's ring buffer survives the churn
// so a reconnecting client still gets its Last-Event-ID replay.
//
// The cruder "delete the topic whenever its subscriber count hits 0"
// fix also bounds the map, but fails the replay half of this test.
func TestHub_ReapDropsMintedTopicsButKeepsReplayBuffers(t *testing.T) {
	hub := streaming.NewHub(0)

	id1 := hub.Publish("closed:XLM/USD", "price_update", []byte("first"))
	hub.Publish("closed:XLM/USD", "price_update", []byte("second"))

	churnTopics(t, hub, 300)

	if got := hub.TopicCount(); got > 128 {
		t.Fatalf("TopicCount = %d after 300 minted topics, want <= 128", got)
	}
	if got := hub.TopicsReaped(); got < 128 {
		t.Fatalf("TopicsReaped = %d, want >= 128 (the reaper should have run several sweeps)", got)
	}

	// The real topic kept its buffer: resuming from id1 replays the
	// event that followed it.
	sub, cancel, err := hub.Subscribe([]string{"closed:XLM/USD"}, id1)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer cancel()
	got := drainNonblocking(t, sub, 1, time.Second)
	if len(got) != 1 || string(got[0].Data) != "second" {
		t.Fatalf("replay after churn = %v, want the buffered \"second\" event "+
			"(a published topic's replay window must survive topic reaping)", got)
	}
}

// TestHub_ReapDropsBufferedTopicPastIdleTTL — once a published topic
// has been subscriber-less for longer than the idle TTL, its replay
// buffer is released too. Otherwise a pair that trades once and goes
// quiet holds its ring for the life of the process.
func TestHub_ReapDropsBufferedTopicPastIdleTTL(t *testing.T) {
	hub := streaming.NewHub(0)
	hub.SetTopicIdleTTL(time.Nanosecond)

	id1 := hub.Publish("closed:XLM/USD", "price_update", []byte("first"))
	hub.Publish("closed:XLM/USD", "price_update", []byte("second"))

	churnTopics(t, hub, 128) // forces at least one sweep

	if got := hub.TopicsReaped(); got == 0 {
		t.Fatal("TopicsReaped = 0, want > 0 (the reaper never ran)")
	}
	sub, cancel, err := hub.Subscribe([]string{"closed:XLM/USD"}, id1)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer cancel()
	if got := drainNonblocking(t, sub, 1, 200*time.Millisecond); len(got) != 0 {
		t.Fatalf("replay after idle TTL = %v, want none (the buffer should have been released)", got)
	}
}

// TestHub_ReapNeverDropsSubscribedTopic — reaping must never detach a
// live stream from its fanout. Run with the reaper at maximum pressure
// (tiny ceiling, nanosecond TTL): the subscribed topic still delivers.
func TestHub_ReapNeverDropsSubscribedTopic(t *testing.T) {
	hub := streaming.NewHub(0)
	hub.SetTopicIdleTTL(time.Nanosecond)
	hub.SetMaxTopics(4)

	sub, cancel, err := hub.Subscribe([]string{"closed:XLM/USD"}, "")
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer cancel()

	churnTopics(t, hub, 300)

	hub.Publish("closed:XLM/USD", "price_update", []byte("live"))
	got := drainNonblocking(t, sub, 1, time.Second)
	if len(got) != 1 || string(got[0].Data) != "live" {
		t.Fatalf("subscribed topic delivered %v after churn, want the live event "+
			"(a topic with subscribers must never be reaped)", got)
	}
	if n := hub.TopicCount(); n > 8 {
		t.Errorf("TopicCount = %d with MaxTopics(4), want the ceiling to hold it small", n)
	}
}

// TestStream_RejectedStreamsCounter — a connection refused by the caps
// is counted, so a flood is visible in diagnostics rather than silent.
func TestStream_RejectedStreamsCounter(t *testing.T) {
	streaming.SetMaxStreamsPerIP(1)
	defer streaming.SetMaxStreamsPerIP(0)
	streaming.SetStreamClientIPResolver(func(*http.Request) string { return "counter-client" })
	defer streaming.SetStreamClientIPResolver(nil)

	hub := streaming.NewHub(0)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		streaming.Stream(w, r, hub, []string{"topic"}, streaming.StreamOptions{
			HeartbeatInterval: 30 * time.Second,
		})
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	before := streaming.StreamsRejected()

	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	held, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("open first stream: %v", err)
	}
	defer held.Body.Close()

	req2, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	resp, err := srv.Client().Do(req2)
	if err != nil {
		t.Fatalf("open over-cap stream: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("over-cap status = %d, want 503", resp.StatusCode)
	}

	if got := streaming.StreamsRejected() - before; got != 1 {
		t.Errorf("StreamsRejected delta = %d, want 1", got)
	}
}
