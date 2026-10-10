package ingest

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/config"
	blend_backstop "github.com/Stellar-Index/StellarIndex/internal/sources/blend_backstop"
	"github.com/Stellar-Index/StellarIndex/internal/sources/external"
	sep41supply "github.com/Stellar-Index/StellarIndex/internal/sources/sep41_supply"
	sep41transfers "github.com/Stellar-Index/StellarIndex/internal/sources/sep41_transfers"
	"github.com/Stellar-Index/StellarIndex/internal/sources/upshift"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// fakeProjectorCursor answers GetCursor from a scripted sequence, the
// way the real cursor advances while the projector re-walks a replayed
// range.
type fakeProjectorCursor struct {
	ledgers []uint32
	calls   int
	err     error
}

func (f *fakeProjectorCursor) GetCursor(_ context.Context, _, _ string) (timescale.Cursor, error) {
	if f.err != nil {
		return timescale.Cursor{}, f.err
	}
	i := f.calls
	if i >= len(f.ledgers) {
		i = len(f.ledgers) - 1
	}
	f.calls++
	return timescale.Cursor{LastLedger: f.ledgers[i]}, nil
}

// TestAwaitProjectorCursor_ReturnsOnlyOnceTheRangeIsReWalked: the
// post-replay CAGG refresh must not run against rows the projector has
// not written yet. A refresh over an un-re-projected range SUCCEEDS and
// materializes the short answer — worse than no refresh, because it
// leaves a green run to point at.
func TestAwaitProjectorCursor_ReturnsOnlyOnceTheRangeIsReWalked(t *testing.T) {
	f := &fakeProjectorCursor{ledgers: []uint32{60_000_000, 62_000_000, 63_500_000}}
	if err := awaitProjectorCursor(context.Background(), discardLogger(), f, "cctp", 63_500_000, 30*time.Second, time.Millisecond); err != nil {
		t.Fatalf("awaitProjectorCursor: %v", err)
	}
	if f.calls < 3 {
		t.Errorf("returned after %d cursor reads — it cannot have observed the cursor reach the target", f.calls)
	}
}

// A projector that never catches up must FAIL the command, naming the
// range left unmaterialized. The rewind is durable by then, so a silent
// return would leave every OHLC/VWAP read short over the replayed range
// with nothing saying so.
func TestAwaitProjectorCursor_TimeoutFailsLoudly(t *testing.T) {
	f := &fakeProjectorCursor{ledgers: []uint32{60_000_000}}
	err := awaitProjectorCursor(context.Background(), discardLogger(), f, "cctp", 63_500_000, 10*time.Millisecond, time.Millisecond)
	if err == nil {
		t.Fatal("a projector that never re-walked the range returned success")
	}
	for _, want := range []string{"63500000", "not fully re-projected", "were NOT refreshed"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("timeout error lacks %q: %v", want, err)
		}
	}
}

func TestAwaitProjectorCursor_CursorReadErrorPropagates(t *testing.T) {
	f := &fakeProjectorCursor{err: errors.New("boom")}
	if err := awaitProjectorCursor(context.Background(), discardLogger(), f, "cctp", 1, time.Second, time.Millisecond); err == nil {
		t.Fatal("a failing cursor read returned success")
	}
}

// fakeReplayStore is a replayFinisher: a scripted projector cursor plus a
// recording CAGG refresher.
type fakeReplayStore struct {
	fakeProjectorCursor
	rangeFrom, rangeTo uint32
	refreshed          []string
	lastTo             time.Time
}

func (f *fakeReplayStore) LedgerRangeToTimeRange(_ context.Context, from, to uint32) (time.Time, time.Time, error) {
	f.rangeFrom, f.rangeTo = from, to
	t0 := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	return t0, t0.Add(6 * time.Hour), nil
}

func (f *fakeReplayStore) LedgerRangeToOracleTimeRange(context.Context, uint32, uint32) (time.Time, time.Time, error) {
	return time.Time{}, time.Time{}, timescale.ErrNotFound
}

func (f *fakeReplayStore) Prices1mRetentionArmed(context.Context) (bool, error) { return false, nil }

func (f *fakeReplayStore) RefreshContinuousAggregateForced(ctx context.Context, name string, from, to time.Time) error {
	return f.RefreshContinuousAggregate(ctx, name, from, to)
}

func (f *fakeReplayStore) RefreshContinuousAggregate(_ context.Context, name string, from, to time.Time) error {
	// RunCAGGRefreshStep cuts a long window into consecutive CALLs; record
	// them as the one per-view refresh they make up.
	if n := len(f.refreshed); n == 0 || f.refreshed[n-1] != name || !f.lastTo.Equal(from) {
		f.refreshed = append(f.refreshed, name)
	}
	f.lastTo = to
	return nil
}

// TestRematerializeReplayedRange_RefreshesEveryViewOverTheReplayedRange:
// once the projector is back at the pre-rewind ledger, the tail refreshes
// every long-lived price CAGG over exactly the replayed ledger range.
func TestRematerializeReplayedRange_RefreshesEveryViewOverTheReplayedRange(t *testing.T) {
	f := &fakeReplayStore{fakeProjectorCursor: fakeProjectorCursor{ledgers: []uint32{63_500_000}}}
	replayed := chunkRange{from: 62_000_000, to: 63_500_000}
	err := rematerializeReplayedRange(io.Discard, discardLogger(), f, "cctp", replayed,
		replayFollowUp{refreshCAGGs: true, wait: true, waitTimeout: time.Minute})
	if err != nil {
		t.Fatalf("rematerializeReplayedRange: %v", err)
	}
	if f.rangeFrom != replayed.from || f.rangeTo != replayed.to {
		t.Errorf("refreshed ledgers [%d,%d], want the replayed range [%d,%d]",
			f.rangeFrom, f.rangeTo, replayed.from, replayed.to)
	}
	if got, want := len(f.refreshed), len(timescale.TradesCAGGs); got != want {
		t.Errorf("refreshed %d views %v, want all %d trades CAGGs", got, f.refreshed, want)
	}
}

// TestRematerializeReplayedRange_NeverRefreshesAheadOfTheProjector: a
// projector still short of the pre-rewind ledger when the budget runs out
// is an ERROR, and no view is refreshed — a refresh over rows not yet
// re-projected succeeds and materializes the short answer.
func TestRematerializeReplayedRange_NeverRefreshesAheadOfTheProjector(t *testing.T) {
	f := &fakeReplayStore{fakeProjectorCursor: fakeProjectorCursor{ledgers: []uint32{62_400_000}}}
	err := rematerializeReplayedRange(io.Discard, discardLogger(), f, "cctp",
		chunkRange{from: 62_000_000, to: 63_500_000},
		replayFollowUp{refreshCAGGs: true, wait: true, waitTimeout: 0})
	if err == nil {
		t.Fatal("want an error while the projector is short of the pre-rewind ledger, got nil")
	}
	if !strings.Contains(err.Error(), "NOT refreshed") {
		t.Errorf("error does not say the CAGGs were left unrefreshed: %v", err)
	}
	if len(f.refreshed) != 0 {
		t.Errorf("refreshed %v ahead of the projector — that materializes the short answer", f.refreshed)
	}
}

// TestRematerializeReplayedRange_OptOutsTouchNothing: -refresh-caggs=false
// and -wait=false both return nil without reading the cursor or
// refreshing a view; the rewind they follow is already durable.
func TestRematerializeReplayedRange_OptOutsTouchNothing(t *testing.T) {
	for name, opts := range map[string]replayFollowUp{
		"-refresh-caggs=false": {refreshCAGGs: false, wait: true, waitTimeout: time.Minute},
		"-wait=false":          {refreshCAGGs: true, wait: false, waitTimeout: time.Minute},
	} {
		f := &fakeReplayStore{fakeProjectorCursor: fakeProjectorCursor{ledgers: []uint32{1}}}
		if err := rematerializeReplayedRange(io.Discard, discardLogger(), f, "cctp", chunkRange{from: 10, to: 20}, opts); err != nil {
			t.Errorf("%s: %v", name, err)
		}
		if f.calls != 0 || len(f.refreshed) != 0 {
			t.Errorf("%s: cursor reads=%d refreshed=%v, want neither", name, f.calls, f.refreshed)
		}
	}
}

// On a -wait-timeout, the recovery advice must not be
// "re-run with the same -from" — a re-run recomputes the rewind target
// from the now partially-advanced cursor and rewinds+re-walks the whole
// range again, never refreshing the gap between the first run's partial
// progress and its original pre-rewind cursor. The advice must instead
// point at the refresh-only recovery path, which refreshes CAGGs over an
// explicit range without touching the cursor.
func TestAwaitProjectorCursor_TimeoutAdvisesRefreshOnly_NotPlainRerun(t *testing.T) {
	f := &fakeProjectorCursor{ledgers: []uint32{62_900_000}}
	err := awaitProjectorCursor(context.Background(), discardLogger(), f, "cctp", 63_500_000, 10*time.Millisecond, time.Millisecond)
	if err == nil {
		t.Fatal("a projector that never re-walked the range returned success")
	}
	msg := err.Error()
	if !strings.Contains(msg, "-refresh-only") {
		t.Errorf("timeout error does not point at the refresh-only recovery path: %v", err)
	}
	if strings.Contains(msg, "re-run this command with the same -from (the rewind is already durable and idempotent)") {
		t.Errorf("timeout error still gives the plain re-run advice that rewinds and re-walks the whole range again, leaving the already-progressed prefix unrefreshed: %v", err)
	}
}

// -refresh-only requires an explicit upper bound and refuses a
// range that doesn't make sense, before touching config or the store.
func TestProjectorReplay_RefreshOnlyRequiresRefreshTo(t *testing.T) {
	t.Parallel()
	err := projectorReplay(io.Discard, replayArgs(t, "cctp", "-refresh-only"))
	if err == nil || !strings.Contains(err.Error(), "-refresh-to") {
		t.Fatalf("missing -refresh-to must be refused, got: %v", err)
	}

	err = projectorReplay(io.Discard, replayArgs(t, "cctp", "-refresh-only", "-refresh-to", "1"))
	if err == nil || !strings.Contains(err.Error(), "-refresh-to") {
		t.Fatalf("-refresh-to below -from must be refused, got: %v", err)
	}
}

// -refresh-only -dry-run never rewinds the cursor and never loads the
// config/store — it prints the range it would refresh and returns.
func TestProjectorReplay_RefreshOnlyDryRun(t *testing.T) {
	t.Parallel()
	var err error
	out := captureOutput(func(w io.Writer) {
		err = projectorReplay(w, replayArgs(t, "cctp", "-refresh-only", "-refresh-to", "52728400", "-dry-run"))
	})
	if err != nil {
		t.Fatalf("dry-run refresh-only returned error: %v", err)
	}
	if !strings.Contains(out, "would refresh the price CAGGs over ledgers [52728375,52728400]") {
		t.Errorf("dry-run output missing the planned range: %q", out)
	}
	if strings.Contains(out, "would RecordProjectionDirtyWindow") || strings.Contains(out, "would then UpsertCursor") {
		t.Errorf("refresh-only dry-run must not go through the rewind dry-run path: %q", out)
	}
}

// projector-replay is the documented catch-up procedure for every
// projected source, and it re-decodes history with the CURRENT decoder —
// but only `backfill` consulted external.BackfillSafe. These tests drive
// the real subcommand entry point. The config path does not exist, so a
// run that gets PAST the gate fails at "load config"; which of the two
// errors comes back is therefore exactly "did the gate fire".
func replayArgs(t *testing.T, source string, extra ...string) []string {
	t.Helper()
	missing := filepath.Join(t.TempDir(), "absent.toml")
	return append([]string{"-config", missing, "-source", source, "-from", "52728375"}, extra...)
}

func TestProjectorReplay_RefusesSourceThatIsNotBackfillSafe(t *testing.T) {
	t.Parallel()
	// The registry is the authority; if the audit lands and this flips,
	// the test must be re-pointed at another unaudited source, not deleted.
	if external.BackfillSafe(upshift.SourceName) {
		t.Fatalf("%s is now BackfillSafe — pick a source that is still unaudited for this test", upshift.SourceName)
	}
	for _, extra := range [][]string{nil, {"-dry-run"}} {
		err := projectorReplay(io.Discard, replayArgs(t, upshift.SourceName, extra...))
		if err == nil {
			t.Fatalf("args %v: replay of an unaudited source returned nil", extra)
		}
		if !strings.Contains(err.Error(), "not BackfillSafe") {
			t.Errorf("args %v: replay of %s got past the BackfillSafe gate (F050); error was: %v",
				extra, upshift.SourceName, err)
		}
		if !strings.Contains(err.Error(), upshift.SourceName) {
			t.Errorf("args %v: refusal does not name the source: %v", extra, err)
		}
	}
}

func TestProjectorReplay_RefusesUnknownSourceName(t *testing.T) {
	t.Parallel()
	// The gap detector's hyphenated per-table name — not a projector source.
	err := projectorReplay(io.Discard, replayArgs(t, "blend-backstop"))
	if err == nil || !strings.Contains(err.Error(), "not BackfillSafe") {
		t.Fatalf("unknown source must be refused fail-closed at the gate, got: %v", err)
	}
	if !strings.Contains(err.Error(), "underscored") {
		t.Errorf("refusal lost the naming hint a typo needs: %v", err)
	}
}

// The destructive branch has a twin: the gate must NOT strand the
// sanctioned replays. Every name here has to get past it (and then fail
// on the absent config, proving the run continued).
func TestProjectorReplay_AuditedAndRegistrylessSourcesPassTheGate(t *testing.T) {
	t.Parallel()
	for _, source := range []string{
		"aquarius", // the canonical scenario: audited, replay allowed
		blend_backstop.SourceName,
		sep41supply.SourceName,
		sep41transfers.SourceName,
	} {
		err := projectorReplay(io.Discard, replayArgs(t, source))
		if err == nil {
			t.Fatalf("%s: expected the absent config to fail the run", source)
		}
		if strings.Contains(err.Error(), "not BackfillSafe") {
			t.Errorf("%s: a sanctioned replay is refused by the BackfillSafe gate: %v", source, err)
		}
		if !strings.Contains(err.Error(), "load config") {
			t.Errorf("%s: expected to reach the config load, got: %v", source, err)
		}
	}
}

// blend_backstop has no registry row of its own: its replay-safety is
// `blend`'s attestation (docs/operations/wasm-audits/blend.md) and must
// fall with it.
func TestReplayBackfillSafe_BackstopFollowsBlendAttestation(t *testing.T) {
	// Not parallel: mutates the package-level registry.
	orig := external.Registry["blend"]
	t.Cleanup(func() { external.Registry["blend"] = orig })

	if !external.ReplayBackfillSafe(blend_backstop.SourceName) {
		t.Fatalf("%s must be replay-safe while blend is attested", blend_backstop.SourceName)
	}
	withdrawn := orig
	withdrawn.Backfill = external.BackfillUnsafe
	external.Registry["blend"] = withdrawn
	if external.ReplayBackfillSafe(blend_backstop.SourceName) {
		t.Errorf("%s stayed replay-safe after blend's attestation was withdrawn", blend_backstop.SourceName)
	}
	if err := checkReplayBackfillSafe(blend_backstop.SourceName, 55_000_000); err == nil {
		t.Errorf("replay of %s must be refused once blend is not BackfillSafe", blend_backstop.SourceName)
	}
}

// The per-WASM gate has no skip: a BackfillPerWASM source (cctp) whose lake
// cannot be read is refused, never admitted on the static policy alone.
func TestProjectorReplayGate_UnreachableLakeRefusesPerWASMSource(t *testing.T) {
	var cfg config.Config
	cfg.Storage.ClickHouseAddr = "127.0.0.1:1"
	err := gateProjectorReplay(cfg, nil, "cctp", 62_200_000)
	if err == nil || !strings.Contains(err.Error(), "wasm replay gate") {
		t.Fatalf("err = %v, want a wasm replay gate refusal", err)
	}
}

// fakeSEP41RollupResetter records ResetSEP41SupplyRollupFold calls so a
// test can assert whether — and with what contract scope — a replay
// reset the rollup fold.
type fakeSEP41RollupResetter struct {
	calls       int
	gotContract [][]string
	n           int64
	err         error
}

func (f *fakeSEP41RollupResetter) ResetSEP41SupplyRollupFold(_ context.Context, contractIDs []string) (int64, error) {
	f.calls++
	f.gotContract = append(f.gotContract, contractIDs)
	return f.n, f.err
}

// TestResetSEP41RollupAfterReplay_ResetsOnlyForTheSEP41SupplySource is
// the regression for a stale rollup: a replay of the
// sep41_supply source re-drives rows a held-row retry gave up on
// (quarantined) or corrects rows already written, exactly at-or-below
// the ledger the cursor is rewound below. AdvanceSEP41SupplyRollup only
// ever folds `ledger > last_ledger`, so without a fold reset those rows
// are invisible to served SEP-41 supply forever, no matter how many
// times the replay runs. This must fold a FULL reset (nil contract
// scope), because a source-level replay re-walks every watched
// contract's events over the range, not just one.
func TestResetSEP41RollupAfterReplay_ResetsOnlyForTheSEP41SupplySource(t *testing.T) {
	f := &fakeSEP41RollupResetter{n: 7}
	reset, n, err := resetSEP41RollupAfterReplay(context.Background(), f, sep41supply.SourceName)
	if err != nil {
		t.Fatalf("resetSEP41RollupAfterReplay: %v", err)
	}
	if !reset {
		t.Fatal("want reset=true for a sep41_supply replay")
	}
	if n != 7 {
		t.Errorf("n = %d, want the resetter's reported row count 7", n)
	}
	if f.calls != 1 {
		t.Fatalf("ResetSEP41SupplyRollupFold called %d times, want exactly 1", f.calls)
	}
	if got := f.gotContract[0]; got != nil {
		t.Errorf("contract scope = %v, want nil (FULL reset — a source-level replay is not scoped to one contract)", got)
	}
}

// A replay of any OTHER source (trades sources: cctp, phoenix, blend,
// …) never touches sep41_supply_events, so resetting the SEP-41 rollup
// would be a no-op at best and a spurious "off the fast path" window for
// every watched contract at worst. It must not be called.
func TestResetSEP41RollupAfterReplay_NoopsForOtherSources(t *testing.T) {
	f := &fakeSEP41RollupResetter{}
	reset, n, err := resetSEP41RollupAfterReplay(context.Background(), f, "cctp")
	if err != nil {
		t.Fatalf("resetSEP41RollupAfterReplay: %v", err)
	}
	if reset {
		t.Error("want reset=false for a non-sep41_supply replay")
	}
	if n != 0 {
		t.Errorf("n = %d, want 0", n)
	}
	if f.calls != 0 {
		t.Errorf("ResetSEP41SupplyRollupFold called %d times for source=cctp, want 0", f.calls)
	}
}

func TestResetSEP41RollupAfterReplay_PropagatesResetError(t *testing.T) {
	f := &fakeSEP41RollupResetter{err: errors.New("boom")}
	reset, _, err := resetSEP41RollupAfterReplay(context.Background(), f, sep41supply.SourceName)
	if err == nil {
		t.Fatal("want the resetter's error propagated")
	}
	if !reset {
		t.Error("want reset=true even on error — the caller needs to know a reset was attempted and owed")
	}
}

// TestProjectorReplay_ResetsTheSEP41RollupAfterRewinding is the wiring
// guard: resetSEP41RollupAfterReplay is only worth anything if
// projectorReplay actually reaches it after the cursor rewind lands. The
// call lives one hop down, in reportSEP41RollupReset (split out to keep
// projectorReplay under the cognitive-complexity limit), so BOTH hops are
// pinned — mirrors
// TestProjectorReplay_RefreshesTheCAGGsOverTheReplayedRange's guard for
// the sibling CAGG-refresh wiring. Read from the AST rather than a list so
// the assertion cannot drift from the code it describes.
func TestProjectorReplay_ResetsTheSEP41RollupAfterRewinding(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "projector.go", nil, 0)
	if err != nil {
		t.Fatalf("parse projector.go: %v", err)
	}
	const why = "a replay of -source sep41_supply rewinds and re-walks rows the sep41_supply_rollup fold may " +
		"already have checkpointed past, and AdvanceSEP41SupplyRollup never looks back down for them, so served " +
		"SEP-41 supply stays wrong forever"
	hops := []struct {
		fn    string
		wants []string
	}{
		{"projectorReplay", []string{"reportSEP41RollupReset"}},
		{"reportSEP41RollupReset", []string{"resetSEP41RollupAfterReplay"}},
	}
	for _, hop := range hops {
		called, ok := callsInFunc(file, hop.fn)
		if !ok {
			t.Fatalf("%s is gone from projector.go — this guard has moved", hop.fn)
		}
		for _, want := range hop.wants {
			if !called[want] {
				t.Errorf("%s never calls %s — %s", hop.fn, want, why)
			}
		}
	}
}

// TestProjectorReplay_RefreshesTheCAGGsOverTheReplayedRange is the
// wiring guard: the helper above is only worth anything if the replay
// command actually calls it and then refreshes the aggregates. A replay
// re-projects trades into a historical range, and the CAGG refresh
// policies only roll forward over their own start_offset window (five
// minutes for prices_1m), so nothing else will ever materialize those
// buckets — the rows land in the hypertable and no served read can
// reach them.
//
// The calls live one hop down, in rematerializeReplayedRange (split out
// to keep projectorReplay under the cyclomatic limit), so BOTH hops are
// pinned: a helper that refreshes perfectly and is never reached from
// the command is the same defect as no helper at all.
//
// Read from the AST rather than from a list so the assertion cannot
// drift from the code it describes.
func TestProjectorReplay_RefreshesTheCAGGsOverTheReplayedRange(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "projector.go", nil, 0)
	if err != nil {
		t.Fatalf("parse projector.go: %v", err)
	}
	const why = "a replay rewrites trades in a historical range, and the continuous aggregates over them only roll forward, so the re-projected rows stay invisible to /v1/ohlc, /v1/chart, /v1/vwap and /v1/history/since-inception"
	hops := []struct {
		fn    string
		wants []string
	}{
		{"projectorReplay", []string{"rematerializeReplayedRange"}},
		{"rematerializeReplayedRange", []string{"awaitProjectorCursor", "refreshCAGGsForChunk"}},
	}
	for _, hop := range hops {
		called, ok := callsInFunc(file, hop.fn)
		if !ok {
			t.Fatalf("%s is gone from projector.go — this guard has moved", hop.fn)
		}
		for _, want := range hop.wants {
			if !called[want] {
				t.Errorf("%s never calls %s — %s", hop.fn, want, why)
			}
		}
	}
}

// callsInFunc returns the set of plain-identifier callees inside the
// named top-level function, and whether that function exists.
func callsInFunc(file *ast.File, name string) (map[string]bool, bool) {
	for _, d := range file.Decls {
		fn, ok := d.(*ast.FuncDecl)
		if !ok || fn.Name.Name != name {
			continue
		}
		called := map[string]bool{}
		ast.Inspect(fn, func(n ast.Node) bool {
			if call, ok := n.(*ast.CallExpr); ok {
				if id, ok := call.Fun.(*ast.Ident); ok {
					called[id.Name] = true
				}
			}
			return true
		})
		return called, true
	}
	return nil, false
}
