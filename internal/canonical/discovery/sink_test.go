package discovery_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical/discovery"
)

// fakeRecorder counts Record + IsKnown calls and lets tests inject
// errors / slow paths without spinning up Postgres.
type fakeRecorder struct {
	records    atomic.Int64
	totalCount atomic.Int64  // sum of hit.Count (0 treated as 1), across every Record call
	hold       chan struct{} // when non-nil, Record blocks on it
	err        error

	mu   sync.Mutex
	last map[string]discovery.Hit // last Record call per ContractID
}

func (r *fakeRecorder) Record(_ context.Context, hit discovery.Hit) error {
	r.records.Add(1)
	c := hit.Count
	if c <= 0 {
		c = 1
	}
	r.totalCount.Add(c)
	r.mu.Lock()
	if r.last == nil {
		r.last = make(map[string]discovery.Hit)
	}
	r.last[hit.ContractID] = hit
	r.mu.Unlock()
	if r.hold != nil {
		<-r.hold
	}
	return r.err
}

func (r *fakeRecorder) lastHit(contractID string) discovery.Hit {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.last[contractID]
}

func (r *fakeRecorder) IsKnown(_ context.Context, _ string) (bool, error) {
	return false, nil
}

// TestAsyncSink_DrainsToRecorder: Push -> channel -> worker -> Recorder.Record, with Stop draining pending records; each Push
// uses a distinct (ContractID, EventType) so in-process dedup doesn't collapse them.
func TestAsyncSink_DrainsToRecorder(t *testing.T) {
	rec := &fakeRecorder{}
	sink := discovery.NewAsyncSink(rec, discovery.AsyncSinkOptions{BufferSize: 16})
	sink.Start()

	contracts := []string{"C-1", "C-2", "C-3", "C-4", "C-5"}
	for i, c := range contracts {
		sink.Push(discovery.Hit{
			ContractID:        c,
			EventType:         discovery.EventTransfer,
			Ledger:            uint32(i + 1),
			ObservedAtRFC3339: "2026-04-28T12:00:00Z",
		})
	}
	sink.Stop() // flushes pending

	if got := rec.records.Load(); got != 5 {
		t.Errorf("Recorder.Record called %d times, want 5", got)
	}
}

// TestAsyncSink_DedupsRepeatedHits: repeat pushes for the same (ContractID, EventType) don't each take a channel slot (counted
// in SkippedCount), and Stop flushes one extra Record per key with the true accumulated count, so totalCount accounts for every
// push.
func TestAsyncSink_DedupsRepeatedHits(t *testing.T) {
	rec := &fakeRecorder{}
	sink := discovery.NewAsyncSink(rec, discovery.AsyncSinkOptions{BufferSize: 16})
	sink.Start()

	for i := 0; i < 1000; i++ {
		sink.Push(discovery.Hit{
			ContractID: "C-hot",
			EventType:  discovery.EventTransfer,
			Ledger:     uint32(i + 1),
		})
	}
	// A second EventType for the same contract is a separate key.
	sink.Push(discovery.Hit{ContractID: "C-hot", EventType: discovery.EventMint})

	sink.Stop()

	// 2 initial enqueues (one per unique key) + 1 flushed delta for the
	// 999 skipped "transfer" repeats.
	if got := rec.records.Load(); got != 3 {
		t.Errorf("Recorder.Record called %d times, want 3 (2 initial + 1 flushed delta)", got)
	}
	if got := sink.SkippedCount(); got != 999 {
		t.Errorf("SkippedCount = %d, want 999", got)
	}
	if got := sink.DroppedCount(); got != 0 {
		t.Errorf("DroppedCount = %d, want 0 (dedup keeps the channel from filling)", got)
	}
	if got := rec.totalCount.Load(); got != 1001 {
		t.Errorf("totalCount = %d, want 1001 (1000 transfer pushes + 1 mint push, none lost to dedup)", got)
	}
}

// TestAsyncSink_DropsOnFullBuffer: Push never blocks; once the channel fills, extra Hits bump DroppedCount.
// Each Push uses a distinct ContractID so in-process dedup does not collapse them first.
func TestAsyncSink_DropsOnFullBuffer(t *testing.T) {
	hold := make(chan struct{})
	rec := &fakeRecorder{hold: hold} // worker blocks indefinitely
	sink := discovery.NewAsyncSink(rec, discovery.AsyncSinkOptions{BufferSize: 4})
	sink.Start()

	// First Push pulls into the worker (blocks on `hold`); next 4
	// fill the channel; 6th onward drop.
	for i := 0; i < 10; i++ {
		sink.Push(discovery.Hit{
			ContractID: fmt.Sprintf("C-%d", i),
			EventType:  discovery.EventTransfer,
		})
	}

	// Allow the worker to finish so Stop returns.
	close(hold)
	sink.Stop()

	if got := sink.DroppedCount(); got == 0 {
		t.Errorf("DroppedCount = 0; expected drops once buffer + in-flight saturated")
	}
}

// TestAsyncSink_DropRollsBackSeen: a Push dropped on a full buffer rolls back its seen-mark so a
// later Push for the key can retry; otherwise a transient Postgres outage at cold start would
// permanently lose every newly discovered contract.
func TestAsyncSink_DropRollsBackSeen(t *testing.T) {
	hold := make(chan struct{})
	rec := &fakeRecorder{hold: hold}
	sink := discovery.NewAsyncSink(rec, discovery.AsyncSinkOptions{BufferSize: 1})
	sink.Start()

	// Push enough distinct contracts to guarantee a drop regardless of
	// whether the worker has scheduled yet — with BufferSize=1 and a
	// blocked recorder, only 1–2 hits can be in flight at once.
	for i := 0; i < 8; i++ {
		sink.Push(discovery.Hit{
			ContractID: fmt.Sprintf("C-saturate-%d", i),
			EventType:  discovery.EventMint,
		})
	}
	// And the contract we want to retry: also dropped because buffer
	// is still saturated.
	sink.Push(discovery.Hit{ContractID: "C-retry", EventType: discovery.EventMint})

	if got := sink.DroppedCount(); got == 0 {
		t.Fatalf("expected at least one drop, got 0")
	}
	droppedBefore := sink.DroppedCount()

	// Unblock the worker so the channel drains.
	close(hold)

	// Retry the dropped contract; with seen rolled back
	// it should eventually land. Poll because drain is async.
	recorded := false
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		sink.Push(discovery.Hit{ContractID: "C-retry", EventType: discovery.EventMint})
		// Look at skipped instead of records: once C-retry has been
		// enqueued (and recorded), subsequent Pushes will skip it,
		// proving the dedup mark is set again.
		if sink.SkippedCount() > 0 {
			recorded = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	sink.Stop()

	if !recorded {
		t.Errorf("C-retry never landed after drop rollback (dropped=%d, skipped=%d, recorded=%d)",
			sink.DroppedCount()-droppedBefore, sink.SkippedCount(), rec.records.Load())
	}
}

// TestAsyncSink_LogsRecordError — a Record returning an error
// doesn't crash the worker; subsequent Pushes still drain.
func TestAsyncSink_LogsRecordError(t *testing.T) {
	rec := &fakeRecorder{err: errors.New("postgres down")}
	sink := discovery.NewAsyncSink(rec, discovery.AsyncSinkOptions{BufferSize: 4})
	sink.Start()

	sink.Push(discovery.Hit{ContractID: "C1", EventType: discovery.EventMint})
	sink.Push(discovery.Hit{ContractID: "C2", EventType: discovery.EventBurn})
	sink.Stop()

	if got := rec.records.Load(); got != 2 {
		t.Errorf("Record called %d times, want 2 (errors don't stop the worker)", got)
	}
}

// TestAsyncSink_StopIsIdempotent — calling Stop twice is safe
// (real production code paths can race shutdown).
func TestAsyncSink_StopIsIdempotent(t *testing.T) {
	rec := &fakeRecorder{}
	sink := discovery.NewAsyncSink(rec, discovery.AsyncSinkOptions{BufferSize: 4})
	sink.Start()

	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sink.Stop()
		}()
	}
	wg.Wait() // must not deadlock or panic
}

// TestAsyncSink_StartTwice_IsNoOp pins Start's idempotency: a second Start must not launch a second run() goroutine,
// whose close(s.done) race on Stop would panic the process.
func TestAsyncSink_StartTwice_IsNoOp(t *testing.T) {
	rec := &fakeRecorder{}
	sink := discovery.NewAsyncSink(rec, discovery.AsyncSinkOptions{BufferSize: 4})
	sink.Start()
	sink.Start()
	sink.Push(discovery.Hit{ContractID: "C1", EventType: discovery.EventMint})
	sink.Stop()
	// The duplicate worker's panic lands asynchronously after Stop
	// returns; give it the chance to, so the unguarded code fails here.
	time.Sleep(50 * time.Millisecond)

	if got := rec.records.Load(); got != 1 {
		t.Errorf("records = %d, want 1", got)
	}
}

// TestAsyncSink_DedupsAcrossKindsIndependently: the dedup key (ContractID + Kind + EventType +
// Symbol) must keep a KindOracleEvent hit and a KindOracleCall hit on the same contract with
// different symbols distinct, while still deduplicating repeats of an identical (Kind, Symbol).
func TestAsyncSink_DedupsAcrossKindsIndependently(t *testing.T) {
	rec := &fakeRecorder{}
	sink := discovery.NewAsyncSink(rec, discovery.AsyncSinkOptions{BufferSize: 16})
	sink.Start()

	// Same contract, two different oracle-event symbols: 2 records.
	sink.Push(discovery.Hit{ContractID: "C-oracle", Kind: discovery.KindOracleEvent, Symbol: "price_update"})
	sink.Push(discovery.Hit{ContractID: "C-oracle", Kind: discovery.KindOracleEvent, Symbol: "oracle"})
	// Repeat of the first — must dedup, not a 3rd record.
	sink.Push(discovery.Hit{ContractID: "C-oracle", Kind: discovery.KindOracleEvent, Symbol: "price_update"})
	// Same contract and symbol but KindOracleCall, not KindOracleEvent: Kind alone makes it a distinct sighting, not a dedup.
	sink.Push(discovery.Hit{ContractID: "C-oracle", Kind: discovery.KindOracleCall, Symbol: "relay"})

	sink.Stop()

	// 3 initial enqueues (2 oracle_event symbols + 1 oracle_call) + 1
	// flushed delta for the repeated price_update push.
	if got := rec.records.Load(); got != 4 {
		t.Errorf("Recorder.Record called %d times, want 4 (3 initial + 1 flushed delta)", got)
	}
	if got := sink.SkippedCount(); got != 1 {
		t.Errorf("SkippedCount = %d, want 1 (the repeated price_update push)", got)
	}
	if got := rec.totalCount.Load(); got != 4 {
		t.Errorf("totalCount = %d, want 4 (every push accounted for, none lost to dedup)", got)
	}
}

// TestAsyncSink_LegacySEP41DedupKeyUnchanged: a Hit with only ContractID + EventType (no
// Kind/Symbol) must still dedup on EventType alone. Same EventType collapses to one record;
// different EventTypes on the same contract stay distinct.
func TestAsyncSink_LegacySEP41DedupKeyUnchanged(t *testing.T) {
	rec := &fakeRecorder{}
	sink := discovery.NewAsyncSink(rec, discovery.AsyncSinkOptions{BufferSize: 16})
	sink.Start()

	sink.Push(discovery.Hit{ContractID: "C-legacy", EventType: discovery.EventTransfer})
	sink.Push(discovery.Hit{ContractID: "C-legacy", EventType: discovery.EventTransfer})
	sink.Push(discovery.Hit{ContractID: "C-legacy", EventType: discovery.EventMint})

	sink.Stop()

	// 2 initial enqueues (transfer once, mint once) + 1 flushed delta
	// for the repeated transfer push.
	if got := rec.records.Load(); got != 3 {
		t.Errorf("Recorder.Record called %d times, want 3 (2 initial + 1 flushed delta)", got)
	}
	if got := rec.totalCount.Load(); got != 3 {
		t.Errorf("totalCount = %d, want 3 (every push accounted for, none lost to dedup)", got)
	}
}

// TestAsyncSink_DefaultBufferAndTimeout — zero-value options apply
// sensible defaults rather than producing a 0-buffer (would always
// drop) or 0-timeout sink.
func TestAsyncSink_DefaultBufferAndTimeout(t *testing.T) {
	rec := &fakeRecorder{}
	sink := discovery.NewAsyncSink(rec, discovery.AsyncSinkOptions{})
	sink.Start()

	// If BufferSize defaulted to 0 every Push would drop. With the
	// production default (1024) one Push lands cleanly.
	sink.Push(discovery.Hit{ContractID: "C-default", EventType: discovery.EventTransfer})

	// Give the worker a moment to drain.
	time.Sleep(100 * time.Millisecond)
	sink.Stop()

	if got := rec.records.Load(); got != 1 {
		t.Errorf("Record called %d times, want 1 (default buffer must be > 0)", got)
	}
	if got := sink.DroppedCount(); got != 0 {
		t.Errorf("DroppedCount = %d, want 0", got)
	}
}

// errorOnceRecorder fails the first N Record calls then succeeds —
// a transient recorder outage (Postgres restart, connection storm).
type errorOnceRecorder struct {
	mu      sync.Mutex
	fails   int
	calls   int
	success []string
}

func (r *errorOnceRecorder) Record(_ context.Context, hit discovery.Hit) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	if r.fails > 0 {
		r.fails--
		return errors.New("postgres down")
	}
	r.success = append(r.success, hit.ContractID)
	return nil
}

func (r *errorOnceRecorder) IsKnown(_ context.Context, _ string) (bool, error) { return false, nil }

func (r *errorOnceRecorder) recorded() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.success...)
}

// TestAsyncSink_RecordFailureRollsBackSeen pins that a failed Record rolls back the seen-mark set at Push time.
// Otherwise later events for the contract are skipped as duplicates and discovery loses it for the life of the process.
func TestAsyncSink_RecordFailureRollsBackSeen(t *testing.T) {
	rec := &errorOnceRecorder{fails: 1}
	sink := discovery.NewAsyncSink(rec, discovery.AsyncSinkOptions{BufferSize: 8})
	sink.Start()

	hit := discovery.Hit{ContractID: "C-outage", EventType: discovery.EventMint}

	// First sighting — lands during the outage and fails.
	sink.Push(hit)
	deadline := time.Now().Add(2 * time.Second)
	for sink.FailedCount() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if sink.FailedCount() != 1 {
		t.Fatalf("FailedCount = %d, want 1 (the outage write must have failed)", sink.FailedCount())
	}

	// The recorder is healthy again and the contract emits another
	// event. It must be re-enqueued, not skipped as already-seen.
	for time.Now().Before(deadline) {
		sink.Push(hit)
		if len(rec.recorded()) > 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	sink.Stop()

	got := rec.recorded()
	if len(got) == 0 || got[0] != "C-outage" {
		t.Fatalf("recorded = %v, want C-outage — a contract first sighted during a recorder "+
			"outage stayed marked seen and was never discovered (skipped=%d, failed=%d)",
			got, sink.SkippedCount(), sink.FailedCount())
	}
}

// TestAsyncSink_PushRacingStopDoesNotPanic pins that a dispatcher still pushing during shutdown
// cannot panic with send-on-closed-channel when Stop closes s.ch, which would take the indexer
// down mid clean exit. Run under -race.
func TestAsyncSink_PushRacingStopDoesNotPanic(t *testing.T) {
	for attempt := 0; attempt < 50; attempt++ {
		rec := &fakeRecorder{}
		sink := discovery.NewAsyncSink(rec, discovery.AsyncSinkOptions{BufferSize: 4})
		sink.Start()

		var wg sync.WaitGroup
		start := make(chan struct{})
		for i := 0; i < 4; i++ {
			wg.Add(1)
			go func(id int) {
				defer wg.Done()
				<-start
				for j := 0; j < 50; j++ {
					sink.Push(discovery.Hit{
						ContractID: fmt.Sprintf("C-%d-%d-%d", attempt, id, j),
						EventType:  discovery.EventMint,
					})
				}
			}(i)
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			sink.Stop()
		}()

		close(start)
		wg.Wait() // a panic in any goroutine fails the test process
	}
}

// stallRecorder models a Postgres stall: every Record blocks until its
// ctx ends.
type stallRecorder struct{ calls atomic.Int64 }

func (r *stallRecorder) Record(ctx context.Context, _ discovery.Hit) error {
	r.calls.Add(1)
	<-ctx.Done()
	return ctx.Err()
}

func (r *stallRecorder) IsKnown(_ context.Context, _ string) (bool, error) { return false, nil }

// TestAsyncSink_StopFlushesTrueEventCountAndLastLedger pins that in-process dedup does not
// freeze event_count at 1 and last_seen_ledger at the first sighting. Stop must flush one delta
// Record call per deduped key carrying the true accumulated count and the latest observed ledger.
func TestAsyncSink_StopFlushesTrueEventCountAndLastLedger(t *testing.T) {
	rec := &fakeRecorder{}
	sink := discovery.NewAsyncSink(rec, discovery.AsyncSinkOptions{BufferSize: 16})
	sink.Start()

	const pushes = 500
	for i := 0; i < pushes; i++ {
		sink.Push(discovery.Hit{
			ContractID:        "C-volume",
			EventType:         discovery.EventTransfer,
			Ledger:            uint32(1_000_000 + i),
			ObservedAtRFC3339: "2026-04-28T12:00:00Z",
		})
	}
	sink.Stop()

	if got := rec.totalCount.Load(); got != pushes {
		t.Fatalf("totalCount = %d, want %d — event volume must not be discarded by in-process dedup", got, pushes)
	}
	if got := rec.records.Load(); got != 2 {
		t.Fatalf("Record called %d times, want 2 (1 initial enqueue + 1 flushed delta)", got)
	}
	last := rec.lastHit("C-volume")
	wantLedger := uint32(1_000_000 + pushes - 1)
	if last.Ledger != wantLedger {
		t.Errorf("last flushed hit Ledger = %d, want %d — last_seen_ledger must track the true last "+
			"observation, not freeze at the first post-restart sighting", last.Ledger, wantLedger)
	}
	if last.Count != pushes-1 {
		t.Errorf("flushed delta Count = %d, want %d (the initial enqueue already carried 1)", last.Count, pushes-1)
	}
}

// TestAsyncSink_StopBoundedByDrainTimeout: Stop needs an overall drain deadline; draining 1024 hits at 2s each (~34 min)
// under a Postgres stall would outlive the systemd stop timeout and get SIGKILLed.
func TestAsyncSink_StopBoundedByDrainTimeout(t *testing.T) {
	const hits = 40
	rec := &stallRecorder{}
	s := discovery.NewAsyncSink(rec, discovery.AsyncSinkOptions{
		BufferSize:    hits,
		RecordTimeout: 500 * time.Millisecond,
		DrainTimeout:  200 * time.Millisecond,
	})
	s.Start()
	for i := 0; i < hits; i++ {
		s.Push(discovery.Hit{ContractID: fmt.Sprintf("C%02d", i), EventType: "transfer"})
	}

	start := time.Now()
	s.Stop()
	elapsed := time.Since(start)

	// Unbounded, this drain takes hits × RecordTimeout = 20s.
	if elapsed > 3*time.Second {
		t.Fatalf("Stop took %v; want it bounded by DrainTimeout (200ms), not hits × RecordTimeout", elapsed)
	}
	// Every hit is accounted for: attempted (and failed) or abandoned as dropped.
	calls := uint64(rec.calls.Load())
	if dropped := s.DroppedCount(); dropped == 0 || calls+dropped != hits {
		t.Errorf("attempted %d + dropped %d != %d pushed (want abandoned hits counted as dropped)", calls, dropped, hits)
	}
}
