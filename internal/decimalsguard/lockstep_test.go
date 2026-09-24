package decimalsguard

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// fakeStore is an in-memory DecimalsAssetReconciler: the projection table
// (`nonstandard_decimals_assets`) as a map, plus per-verb failure injection
// and call counts so tests can prove WHICH repair Reconcile chose.
type fakeStore struct {
	rows      map[string]timescale.NonstandardDecimalsAsset
	loadErr   error
	upsertErr error
	deleteErr error
	upserts   int
	deletes   int
}

func newFakeStore(rows ...timescale.NonstandardDecimalsAsset) *fakeStore {
	s := &fakeStore{rows: make(map[string]timescale.NonstandardDecimalsAsset, len(rows))}
	for _, r := range rows {
		s.rows[r.Asset] = r
	}
	return s
}

func (s *fakeStore) LoadNonstandardDecimalsAssets(_ context.Context) ([]timescale.NonstandardDecimalsAsset, error) {
	if s.loadErr != nil {
		return nil, s.loadErr
	}
	out := make([]timescale.NonstandardDecimalsAsset, 0, len(s.rows))
	for _, r := range s.rows {
		out = append(out, r)
	}
	return out, nil
}

func (s *fakeStore) UpsertNonstandardDecimalsAsset(_ context.Context, asset string, decimals uint32, source string) error {
	s.upserts++
	if s.upsertErr != nil {
		return s.upsertErr
	}
	s.rows[asset] = timescale.NonstandardDecimalsAsset{Asset: asset, Decimals: int(decimals), Source: source, ConfirmedAt: time.Now()}
	return nil
}

func (s *fakeStore) DeleteNonstandardDecimalsAsset(_ context.Context, asset string) error {
	s.deletes++
	if s.deleteErr != nil {
		return s.deleteErr
	}
	delete(s.rows, asset)
	return nil
}

// mismatchVal reads the guard_reconcile lockstep counter for one asset.
func mismatchVal(asset string) float64 {
	return testutil.ToFloat64(obs.NonstandardDecimalsLockstepMismatchTotal.WithLabelValues("guard_reconcile", asset))
}

// row is a projection-row literal (opaque fake ids, never real strkeys —
// the fake resolver looks them up in a map).
func row(asset string, decimals int) timescale.NonstandardDecimalsAsset {
	return timescale.NonstandardDecimalsAsset{Asset: asset, Decimals: decimals, Source: "aquarius"}
}

// TestReconcile_RepairsRowThatDisagreesWithLake: a persisted row carrying a
// value the lake contradicts (a hand-seeded 6 for a token whose instance
// declares 9) is upserted to the lake's value, counted once, and the
// resolved cache learns the lake's value so the next Sweep agrees.
func TestReconcile_RepairsRowThatDisagreesWithLake(t *testing.T) {
	const asset = "fake-lockstep-hand-seeded-6-lake-9"
	before := mismatchVal(asset)

	store := newFakeStore(row(asset, 6))
	resolver := &fakeResolver{decimals: map[string]uint32{asset: 9}}
	g := New(&fakeReader{}, resolver, Options{Writer: store})

	if err := g.Reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if got := store.rows[asset].Decimals; got != 9 {
		t.Fatalf("row decimals after reconcile = %d, want 9 (repaired toward the lake)", got)
	}
	if store.upserts != 1 || store.deletes != 0 {
		t.Fatalf("upserts=%d deletes=%d, want 1/0", store.upserts, store.deletes)
	}
	if got := mismatchVal(asset) - before; got != 1 {
		t.Fatalf("mismatch counter delta = %v, want 1", got)
	}
	// The resolved cache now carries the lake's value: a Sweep over the same
	// asset must not re-query the lake and must not re-report.
	g.reader = &fakeReader{refs: []timescale.SorobanDEXTradeRef{{Source: "aquarius", Asset: asset}}}
	if err := g.Sweep(context.Background()); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if resolver.calls != 1 {
		t.Fatalf("resolver.calls = %d, want 1 (reconcile's reading is cached for the sweep)", resolver.calls)
	}
}

// TestReconcile_DeletesRowWhenLakeConfirmsStandard: the table's CHECK
// forbids storing 7, so a row whose token the lake now confirms as 7 dp
// can only be repaired by deletion — and the durable-write latch for it
// is cleared so a later confirmed non-7 reading is persisted again.
func TestReconcile_DeletesRowWhenLakeConfirmsStandard(t *testing.T) {
	const asset = "fake-lockstep-stale-9-lake-7"
	before := mismatchVal(asset)

	store := newFakeStore(row(asset, 9))
	resolver := &fakeResolver{decimals: map[string]uint32{asset: 7}}
	g := New(&fakeReader{}, resolver, Options{Writer: store})
	g.persisted["aquarius\x00"+asset] = struct{}{}
	g.persisted["soroswap\x00"+asset] = struct{}{}
	g.persisted["aquarius\x00fake-lockstep-other"] = struct{}{}

	if err := g.Reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if _, still := store.rows[asset]; still {
		t.Fatal("row still present after the lake confirmed 7 dp, want deleted")
	}
	if store.deletes != 1 || store.upserts != 0 {
		t.Fatalf("deletes=%d upserts=%d, want 1/0", store.deletes, store.upserts)
	}
	if got := mismatchVal(asset) - before; got != 1 {
		t.Fatalf("mismatch counter delta = %v, want 1", got)
	}
	if _, latched := g.persisted["aquarius\x00"+asset]; latched {
		t.Fatal("persisted latch for the deleted asset (aquarius) not cleared")
	}
	if _, latched := g.persisted["soroswap\x00"+asset]; latched {
		t.Fatal("persisted latch for the deleted asset (soroswap) not cleared")
	}
	if _, latched := g.persisted["aquarius\x00fake-lockstep-other"]; !latched {
		t.Fatal("persisted latch for an unrelated asset was cleared")
	}
}

// TestReconcile_LeavesUnresolvableRowAlone: a lake read error and a
// not-derivable declaration are NOT evidence the row is wrong — an
// operator-seeded row for an uncaptured instance must survive. No write,
// no delete, no metric.
func TestReconcile_LeavesUnresolvableRowAlone(t *testing.T) {
	const (
		errAsset  = "fake-lockstep-lake-error"
		missAsset = "fake-lockstep-lake-not-derivable"
	)
	beforeErr, beforeMiss := mismatchVal(errAsset), mismatchVal(missAsset)

	store := newFakeStore(row(errAsset, 9), row(missAsset, 18))
	inner := &fakeResolver{decimals: map[string]uint32{}}
	res := &selectiveResolver{inner: inner, errFor: map[string]error{errAsset: errors.New("clickhouse down")}}
	g := New(&fakeReader{}, res, Options{Writer: store})

	if err := g.Reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if store.rows[errAsset].Decimals != 9 || store.rows[missAsset].Decimals != 18 {
		t.Fatalf("rows changed: %+v", store.rows)
	}
	if store.upserts != 0 || store.deletes != 0 {
		t.Fatalf("upserts=%d deletes=%d, want 0/0", store.upserts, store.deletes)
	}
	if mismatchVal(errAsset)-beforeErr != 0 || mismatchVal(missAsset)-beforeMiss != 0 {
		t.Fatal("mismatch counter moved for an unresolvable row")
	}
}

// TestReconcile_AgreementIsSilent: a row in lockstep with the lake is left
// untouched and uncounted.
func TestReconcile_AgreementIsSilent(t *testing.T) {
	const asset = "fake-lockstep-agrees-18"
	before := mismatchVal(asset)

	store := newFakeStore(row(asset, 18))
	resolver := &fakeResolver{decimals: map[string]uint32{asset: 18}}
	g := New(&fakeReader{}, resolver, Options{Writer: store})

	if err := g.Reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if store.upserts != 0 || store.deletes != 0 {
		t.Fatalf("upserts=%d deletes=%d, want 0/0", store.upserts, store.deletes)
	}
	if got := mismatchVal(asset) - before; got != 0 {
		t.Fatalf("mismatch counter delta = %v, want 0", got)
	}
}

// TestReconcile_BypassesResolvedCache is the drift the lockstep exists for:
// the guard confirmed 9 and latched it in its per-process resolved cache,
// then the lake reading changed (a re-captured instance). Sweep would never
// look again; Reconcile must re-read the lake and repair the row.
func TestReconcile_BypassesResolvedCache(t *testing.T) {
	const asset = "fake-lockstep-recaptured-instance"
	before := mismatchVal(asset)

	store := newFakeStore()
	resolver := &fakeResolver{decimals: map[string]uint32{asset: 9}}
	g := New(&fakeReader{refs: []timescale.SorobanDEXTradeRef{{Source: "aquarius", Asset: asset}}}, resolver, Options{Writer: store})

	// Sweep confirms 9, persists it, and caches it.
	if err := g.Sweep(context.Background()); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if store.rows[asset].Decimals != 9 || resolver.calls != 1 {
		t.Fatalf("after sweep: rows=%+v calls=%d", store.rows, resolver.calls)
	}

	// The lake now says 8. A second Sweep is blind to it (cache hit) …
	resolver.decimals[asset] = 8
	if err := g.Sweep(context.Background()); err != nil {
		t.Fatalf("sweep 2: %v", err)
	}
	if resolver.calls != 1 || store.rows[asset].Decimals != 9 {
		t.Fatalf("sweep must not have re-read (calls=%d rows=%+v)", resolver.calls, store.rows)
	}

	// … Reconcile is not.
	if err := g.Reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if resolver.calls != 2 {
		t.Fatalf("resolver.calls = %d, want 2 (reconcile bypasses the resolved cache)", resolver.calls)
	}
	if got := store.rows[asset].Decimals; got != 8 {
		t.Fatalf("row decimals = %d, want 8 (repaired to the re-read lake value)", got)
	}
	if got := mismatchVal(asset) - before; got != 1 {
		t.Fatalf("mismatch counter delta = %v, want 1", got)
	}
}

// TestReconcile_FailedRepairRecountsNextTick: the counter increments on
// OBSERVATION, so a repair whose write fails keeps counting until it lands —
// a sustained value is what the correction_failing alert reads.
func TestReconcile_FailedRepairRecountsNextTick(t *testing.T) {
	const asset = "fake-lockstep-repair-write-fails"
	before := mismatchVal(asset)

	store := newFakeStore(row(asset, 6))
	store.upsertErr = errors.New("connection refused")
	resolver := &fakeResolver{decimals: map[string]uint32{asset: 9}}
	g := New(&fakeReader{}, resolver, Options{Writer: store})

	for i := 0; i < 2; i++ {
		if err := g.Reconcile(context.Background()); err != nil {
			t.Fatalf("reconcile %d: %v", i, err)
		}
	}
	if store.rows[asset].Decimals != 6 {
		t.Fatalf("row repaired despite the write failing: %+v", store.rows[asset])
	}
	if got := mismatchVal(asset) - before; got != 2 {
		t.Fatalf("mismatch counter delta = %v, want 2 (one per observed, unrepaired tick)", got)
	}
	store.upsertErr = nil
	if err := g.Reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile 3: %v", err)
	}
	if store.rows[asset].Decimals != 9 {
		t.Fatalf("row not repaired once the write succeeds: %+v", store.rows[asset])
	}
}

// TestReconcile_DisabledWithoutReconciler: a Writer that is only a Writer
// (no load/delete) keeps the pre-lockstep behaviour — Reconcile is a no-op,
// never an error.
func TestReconcile_DisabledWithoutReconciler(t *testing.T) {
	writer := &fakeWriter{}
	resolver := &fakeResolver{decimals: map[string]uint32{}}
	g := New(&fakeReader{}, resolver, Options{Writer: writer})

	if err := g.Reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if resolver.calls != 0 || writer.calls != 0 {
		t.Fatalf("reconcile touched the lake/writer without a reconciler: resolver=%d writer=%d", resolver.calls, writer.calls)
	}
}

// TestNew_LogsWhenWriterDisarmsReconcile: a non-nil Writer that does not
// implement DecimalsAssetReconciler (a wrapper that forgot to forward it)
// disarms the lockstep, and New must say so at ERROR naming the concrete
// type. A full reconciler and a nil Writer (persistence deliberately off)
// must stay quiet.
func TestNew_LogsWhenWriterDisarmsReconcile(t *testing.T) {
	cases := []struct {
		name    string
		writer  DecimalsAssetWriter
		wantLog bool
	}{
		{"writer-only wrapper", &fakeWriter{}, true},
		{"full reconciler", newFakeStore(), false},
		{"nil writer", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
			g := New(&fakeReader{}, &fakeResolver{decimals: map[string]uint32{}}, Options{Writer: tc.writer, Logger: logger})
			out := buf.String()
			logged := strings.Contains(out, "level=ERROR") &&
				strings.Contains(out, "lockstep reconcile is DISABLED") &&
				strings.Contains(out, "writer_type=*decimalsguard.fakeWriter")
			if logged != tc.wantLog {
				t.Fatalf("disarm ERROR logged=%v want %v; log=%q", logged, tc.wantLog, out)
			}
			if armed := g.reconciler != nil; armed != (tc.name == "full reconciler") {
				t.Fatalf("reconciler armed=%v for %s", armed, tc.name)
			}
		})
	}
}

// TestReconcile_LoadErrorPropagates: the one error Reconcile surfaces is
// the row load itself (Run logs and retries next tick).
func TestReconcile_LoadErrorPropagates(t *testing.T) {
	store := newFakeStore(row("fake-lockstep-load-error", 9))
	store.loadErr = errors.New("connection refused")
	g := New(&fakeReader{}, &fakeResolver{decimals: map[string]uint32{}}, Options{Writer: store})

	if err := g.Reconcile(context.Background()); !errors.Is(err, store.loadErr) {
		t.Fatalf("reconcile err = %v, want the load error", err)
	}
}

// TestLockstep_EveryPersistedRowMatchesTheLake is the invariant test: over a
// projection with every kind of row — drifted, stale-standard, in lockstep,
// unresolvable — one Reconcile leaves NO row the lake can read that
// disagrees with the lake. If the two resolvers can still diverge after a
// tick, this fails.
func TestLockstep_EveryPersistedRowMatchesTheLake(t *testing.T) {
	const (
		drifted      = "fake-lockstep-inv-drifted"      // row 6, lake 9
		staleStd     = "fake-lockstep-inv-stale-std"    // row 9, lake 7
		agreed       = "fake-lockstep-inv-agreed"       // row 18, lake 18
		unresolvable = "fake-lockstep-inv-unresolvable" // row 8, lake not derivable
	)
	store := newFakeStore(row(drifted, 6), row(staleStd, 9), row(agreed, 18), row(unresolvable, 8))
	resolver := &fakeResolver{decimals: map[string]uint32{drifted: 9, staleStd: 7, agreed: 18}}
	g := New(&fakeReader{}, resolver, Options{Writer: store})

	if err := g.Reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	// The invariant, checked against the lake directly (not against what
	// Reconcile says it did): for every remaining row the lake can read,
	// row.Decimals == lake, and the lake never reads 7 for a remaining row.
	for asset, r := range store.rows {
		lake, found, err := resolver.TokenDecimals(context.Background(), asset)
		if err != nil {
			t.Fatalf("resolver %s: %v", asset, err)
		}
		if !found {
			continue // unresolvable rows are out of the invariant's reach by design
		}
		if int(lake) != r.Decimals {
			t.Errorf("row %s persisted %d but the lake says %d — resolvers can diverge", asset, r.Decimals, lake)
		}
		if lake == StandardDecimals {
			t.Errorf("row %s remains although the lake confirms 7 dp", asset)
		}
	}
	// And the specific shape: drifted repaired, stale-standard gone, agreed
	// and unresolvable untouched.
	if store.rows[drifted].Decimals != 9 {
		t.Errorf("drifted row = %+v, want 9", store.rows[drifted])
	}
	if _, still := store.rows[staleStd]; still {
		t.Errorf("stale-standard row still present")
	}
	if store.rows[agreed].Decimals != 18 || store.rows[unresolvable].Decimals != 8 {
		t.Errorf("agreed/unresolvable rows changed: %+v / %+v", store.rows[agreed], store.rows[unresolvable])
	}
}

// TestRun_ReconcilesOnTheInitialTick: Run's first iteration reconciles, not
// just sweeps — a drifted row is repaired at process start, not one
// interval later.
func TestRun_ReconcilesOnTheInitialTick(t *testing.T) {
	const asset = "fake-lockstep-run-initial-tick"
	store := newFakeStore(row(asset, 6))
	resolver := &fakeResolver{decimals: map[string]uint32{asset: 9}}
	g := New(&fakeReader{}, resolver, Options{Writer: store})

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	err := g.Run(ctx, time.Hour) // the ticker never fires; only the initial tick runs
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Run err = %v, want context deadline", err)
	}
	if got := store.rows[asset].Decimals; got != 9 {
		t.Fatalf("row decimals after Run's initial tick = %d, want 9", got)
	}
}

// deadlineRecordingResolver records whether the context reconcileRow passed
// it carried a deadline, without actually blocking for one (a hung-lake
// repro would have to sleep past reconcileRowReadTimeout — this asserts the
// bound is APPLIED, which is the invariant, not that a slow call was
// observed timing out).
type deadlineRecordingResolver struct {
	decimals    uint32
	hadDeadline bool
}

func (r *deadlineRecordingResolver) TokenDecimals(ctx context.Context, _ string) (uint32, bool, error) {
	_, r.hadDeadline = ctx.Deadline()
	return r.decimals, true, nil
}

// TestReconcile_BoundsEachRowReadWithADeadline is GH-1059's #4: Reconcile
// ran every row's lake read on the tick's root context, unbounded, so one
// hung ClickHouse query stalled the whole serial pass. PROVEN RED before
// reconcileRowReadTimeout: the resolver saw the caller's bare
// context.Background() (no deadline) passed straight through.
func TestReconcile_BoundsEachRowReadWithADeadline(t *testing.T) {
	const asset = "fake-lockstep-read-budget"
	store := newFakeStore(row(asset, 9))
	resolver := &deadlineRecordingResolver{decimals: 9}
	g := New(&fakeReader{}, resolver, Options{Writer: store})

	if err := g.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if !resolver.hadDeadline {
		t.Error("lake decimals() read ran with no deadline — a hung read stalls the whole reconcile pass")
	}
}
