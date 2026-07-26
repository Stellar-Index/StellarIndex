package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/Stellar-Index/StellarIndex/internal/aggregate/anomaly"
	"github.com/Stellar-Index/StellarIndex/internal/aggregate/freeze"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

type recordingClearer struct {
	calls []string
	err   error
}

func (c *recordingClearer) Clear(_ context.Context, asset, quote canonical.Asset) error {
	c.calls = append(c.calls, asset.String()+"/"+quote.String())
	return c.err
}

type recordingRecoverer struct {
	calls []string
	err   error
}

func (r *recordingRecoverer) MarkRecovered(_ context.Context, asset, quote canonical.Asset) error {
	r.calls = append(r.calls, asset.String()+"/"+quote.String())
	return r.err
}

func testPair(t *testing.T) (canonical.Asset, canonical.Asset) {
	t.Helper()
	asset, err := canonical.ParseAsset("native")
	if err != nil {
		t.Fatalf("parse native: %v", err)
	}
	quote, err := canonical.ParseAsset("USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")
	if err != nil {
		t.Fatalf("parse quote: %v", err)
	}
	return asset, quote
}

// TestUnfreezePair_ClearsMarkerAndClosesDurableRow pins the whole point of
// the subcommand: ADR-0019 says an escalated freeze "stays active until
// manual unfreeze", and manual unfreeze has to do BOTH halves — the Redis
// marker (the serving path's authority for flags.frozen) and the
// freeze_events row (the explorer's durable timeline). Doing only one leaves
// the two surfaces disagreeing about whether the pair is frozen.
func TestUnfreezePair_ClearsMarkerAndClosesDurableRow(t *testing.T) {
	asset, quote := testPair(t)
	clearer := &recordingClearer{}
	recoverer := &recordingRecoverer{}

	if err := unfreezePair(context.Background(), recoverer, clearer, asset, quote, "oracle recovered", false); err != nil {
		t.Fatalf("unfreezePair: %v", err)
	}
	want := asset.String() + "/" + quote.String()
	if len(clearer.calls) != 1 || clearer.calls[0] != want {
		t.Errorf("redis marker clear calls = %v, want exactly [%s]", clearer.calls, want)
	}
	if len(recoverer.calls) != 1 || recoverer.calls[0] != want {
		t.Errorf("MarkRecovered calls = %v, want exactly [%s]", recoverer.calls, want)
	}
}

// TestUnfreezePair_MarkerFirstAndNothingElseOnFailure — if the Redis clear
// fails, the pair is STILL FROZEN on the serving path, so the durable row
// must not be closed. Closing it would publish a "recovered" timeline for a
// price that is still being withheld: the dishonest direction.
func TestUnfreezePair_MarkerFirstAndNothingElseOnFailure(t *testing.T) {
	asset, quote := testPair(t)
	clearer := &recordingClearer{err: errors.New("redis: connection refused")}
	recoverer := &recordingRecoverer{}

	err := unfreezePair(context.Background(), recoverer, clearer, asset, quote, "why", false)
	if err == nil {
		t.Fatal("a failed Redis clear must fail the run — the pair is still frozen")
	}
	if len(recoverer.calls) != 0 {
		t.Errorf("MarkRecovered must NOT run when the marker clear failed, got %v — that would close the timeline row for a pair the serving path still reports as frozen", recoverer.calls)
	}
	if !strings.Contains(err.Error(), "NOTHING was changed") {
		t.Errorf("the error must tell the operator no state changed, got: %v", err)
	}
}

// TestUnfreezePair_AlreadyClosedRowIsNotAFailure — the recovery worker polls
// every 60s and may have closed the row already, and a repeat unfreeze is a
// legitimate operator action. Neither is an error: the end state is the
// intended one.
func TestUnfreezePair_AlreadyClosedRowIsNotAFailure(t *testing.T) {
	asset, quote := testPair(t)
	clearer := &recordingClearer{}
	recoverer := &recordingRecoverer{err: timescale.ErrNotFound}

	if err := unfreezePair(context.Background(), recoverer, clearer, asset, quote, "repeat", false); err != nil {
		t.Fatalf("an already-closed freeze_events row must not fail the run: %v", err)
	}
	if len(clearer.calls) != 1 {
		t.Errorf("the marker must still be cleared: %v", clearer.calls)
	}
}

// TestUnfreezePair_DryRunTouchesNothing.
func TestUnfreezePair_DryRunTouchesNothing(t *testing.T) {
	asset, quote := testPair(t)
	clearer := &recordingClearer{}
	recoverer := &recordingRecoverer{}

	if err := unfreezePair(context.Background(), recoverer, clearer, asset, quote, "look only", true); err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if len(clearer.calls) != 0 || len(recoverer.calls) != 0 {
		t.Errorf("-dry-run must touch nothing, got clear=%v recover=%v", clearer.calls, recoverer.calls)
	}
}

// TestFreezeUnfreeze_RequiresReasonForAMutation — an unfreeze overrides an
// automated safety control on a money surface. The admin API hard-400s a
// privileged write with no X-Reason; the CLI equivalent must refuse too,
// BEFORE it opens Postgres or Redis.
func TestFreezeUnfreeze_RequiresReasonForAMutation(t *testing.T) {
	err := freezeUnfreeze([]string{
		"-config", "/nonexistent/stellarindex.toml",
		"-asset", "native",
		"-quote", "USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN",
	})
	if err == nil {
		t.Fatal("expected a refusal with no -reason")
	}
	if !strings.Contains(err.Error(), "-reason is required") {
		t.Errorf("expected the missing-reason refusal (and BEFORE any config load), got: %v", err)
	}

	// Whitespace is not a reason.
	err = freezeUnfreeze([]string{
		"-config", "/nonexistent/stellarindex.toml",
		"-asset", "native",
		"-quote", "USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN",
		"-reason", "   ",
	})
	if err == nil || !strings.Contains(err.Error(), "-reason is required") {
		t.Errorf("a whitespace-only -reason must be refused, got: %v", err)
	}
}

// TestFreezeUnfreeze_RequiresPairOrList — the command must not fall through
// to some default target when neither a pair nor -list was given.
func TestFreezeUnfreeze_RequiresPairOrList(t *testing.T) {
	err := freezeUnfreeze([]string{"-config", "/nonexistent/stellarindex.toml"})
	if err == nil || !strings.Contains(err.Error(), "-asset and -quote are required") {
		t.Errorf("expected the missing-pair refusal, got: %v", err)
	}
}

// TestSubcommandDispatch_LeafHandlersSeeTheirFlags is the coverage that was
// missing and let every leaf subcommand ship UNINVOKABLE.
//
// The dispatch table hands a handler the FULL argv (args[0] == the verb).
// Go's flag package stops parsing at the first non-flag argument, so a leaf
// handler doing fs.Parse(args) on that argv parses NOTHING and every flag
// keeps its zero value: `stellarindex-ops usage-rollup-backfill -config
// /etc/stellarindex.toml -from 2026-07-19` answered "-config is required"
// and there was no way to run it at all. The existing unit tests all call
// the handlers DIRECTLY with flags-only argv, so they exercised the
// convention the handlers wanted and never the one the dispatcher used —
// which is precisely why nothing caught it.
//
// This test goes through `subcommands`, the way main() does. The assertion
// is deliberately "the error is NOT the no-flags-parsed symptom": each
// handler's next gate differs, but every one of them reports
// "-config is required" if and only if the flags were dropped.
func TestSubcommandDispatch_LeafHandlersSeeTheirFlags(t *testing.T) {
	cases := []struct {
		verb string
		argv []string
	}{
		{"mint-key", []string{"mint-key", "-config", "/nonexistent.toml", "-identifier", "customer-acme", "-label", "Acme"}},
		{"upgrade-key", []string{"upgrade-key", "-config", "/nonexistent.toml"}},
		{"emit-incident", []string{"emit-incident", "-config", "/nonexistent.toml", "-slug", "s", "-event", "sev1"}},
		{"usage-rollup-backfill", []string{"usage-rollup-backfill", "-config", "/nonexistent.toml", "-from", "2026-07-19"}},
		{"freeze-unfreeze", []string{"freeze-unfreeze", "-config", "/nonexistent.toml", "-list"}},
	}
	for _, tc := range cases {
		t.Run(tc.verb, func(t *testing.T) {
			run, ok := subcommands[tc.verb]
			if !ok {
				t.Fatalf("%s is not in the dispatch table", tc.verb)
			}
			err := run(tc.argv)
			if err == nil {
				t.Fatalf("%s: expected an error (the config file does not exist)", tc.verb)
			}
			if strings.Contains(err.Error(), "-config is required") {
				t.Fatalf("%s: dispatch dropped the flags — -config was passed but the handler never saw it (got %q). The subcommand is uninvokable from the CLI.", tc.verb, err)
			}
		})
	}
}

// ─── the operator-override vs aggregator-rehydrate race ─────────

// fakeLadderStore is an in-memory freeze.LadderStore mirroring the SQL in
// timescale.FreezeEventSink: SaveLadder UPDATEs the open row (a zero State
// nulls hold_until), LoadLadder reports absent once the row is closed
// (recovered_at stamped) or the ladder retired (hold_until NULL).
type fakeLadderStore struct {
	states map[string]freeze.State
	closed map[string]bool
}

func newFakeLadderStore() *fakeLadderStore {
	return &fakeLadderStore{states: map[string]freeze.State{}, closed: map[string]bool{}}
}

func (f *fakeLadderStore) key(asset, quote canonical.Asset) string {
	return asset.String() + "|" + quote.String()
}

func (f *fakeLadderStore) SaveLadder(_ context.Context, asset, quote canonical.Asset, st freeze.State) error {
	k := f.key(asset, quote)
	if f.closed[k] {
		return nil // UPDATE ... WHERE recovered_at IS NULL matches nothing
	}
	f.states[k] = st
	return nil
}

func (f *fakeLadderStore) LoadLadder(_ context.Context, asset, quote canonical.Asset) (freeze.State, bool, error) {
	k := f.key(asset, quote)
	if f.closed[k] {
		return freeze.State{}, false, nil
	}
	st, ok := f.states[k]
	if ok && st.HoldUntil.IsZero() { // mirrors `hold_until IS NOT NULL`
		return freeze.State{}, false, nil
	}
	return st, ok, nil
}

// markRecovered is the durable half of the override (recovered_at).
func (f *fakeLadderStore) markRecovered(asset, quote canonical.Asset) {
	f.closed[f.key(asset, quote)] = true
}

// tickProbingRecoverer stands in for the real MarkRecovered and, before
// stamping recovered_at, performs the exact read an aggregator tick would
// perform at that instant: freeze.Writer.LoadState against the SAME Redis +
// ladder store. That is the race window — Clear has run, MarkRecovered has
// not — so whatever this probe sees is what a concurrent tick would act on.
type tickProbingRecoverer struct {
	aggregator *freeze.Writer
	ladder     *fakeLadderStore
	sawFrozen  bool
	probed     bool
}

func (r *tickProbingRecoverer) MarkRecovered(ctx context.Context, asset, quote canonical.Asset) error {
	_, present, err := r.aggregator.LoadState(ctx, asset, quote)
	if err != nil {
		return err
	}
	r.probed, r.sawFrozen = true, present
	r.ladder.markRecovered(asset, quote)
	return nil
}

// TestUnfreezePair_OperatorWinsTheRehydrateRace pins the direction of the
// one race this command has.
//
// `unfreezePair` deliberately clears the Redis marker FIRST and stamps
// recovered_at second (the marker is the serving path's authority, so
// clearing it is what republishes the price). Since migration 0119 the
// durable ladder is ALSO an authority: an aggregator tick that finds no
// marker but a live open ladder rehydrates the freeze and re-writes the
// marker. A tick landing in the window between those two steps would
// therefore resurrect the freeze, and the operator's MarkRecovered would
// then close a row the aggregator had already replaced — the unfreeze
// silently undone, on a pair that by construction never unfreezes itself.
//
// The fix is the wiring in [newFreezeWriterForOps]: with the ladder store
// passed, Clear retires the ladder in the same call that deletes the marker,
// so the window is empty by construction rather than merely narrow.
//
// This asserts the corrected observation, not just an end state: at the
// instant MarkRecovered runs, a concurrent aggregator tick must already see
// the pair as NOT frozen.
func TestUnfreezePair_OperatorWinsTheRehydrateRace(t *testing.T) {
	asset, quote := testPair(t)

	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	ladder := newFakeLadderStore()

	// The aggregator's writer — always ladder-aware. Its LoadState is the
	// read the racing tick performs.
	aggregator, err := freeze.NewWriter(rdb, 0, freeze.WithLadderStore(ladder, 0))
	if err != nil {
		t.Fatalf("aggregator writer: %v", err)
	}

	// A pair that spent the whole ADR-0019 ladder and escalated: the exact
	// case that "stays active until manual unfreeze", i.e. the only reason
	// this subcommand exists.
	now := time.Now().UTC()
	escalated := freeze.State{
		FiredAt:        now.Add(-2*time.Hour - 5*time.Minute),
		HoldUntil:      now.Add(25 * time.Minute),
		ExtensionsUsed: freeze.DefaultMaxExtensions,
		Escalated:      true,
		Corroborated:   true,
	}
	if err := aggregator.MarkHold(context.Background(), asset, quote, "0.87",
		anomaly.Decision{Action: anomaly.ActionFreeze, Reason: "phase2:3_signal_AND"},
		escalated, 30*time.Minute); err != nil {
		t.Fatalf("MarkHold: %v", err)
	}
	if _, present, err := aggregator.LoadState(context.Background(), asset, quote); err != nil || !present {
		t.Fatalf("precondition: the pair must start frozen (present=%v, err=%v)", present, err)
	}

	// The operator's writer, built exactly as freezeUnfreeze() builds it.
	opsWriter, err := newFreezeWriterForOps(rdb, ladder)
	if err != nil {
		t.Fatalf("newFreezeWriterForOps: %v", err)
	}
	recoverer := &tickProbingRecoverer{aggregator: aggregator, ladder: ladder}

	if err := unfreezePair(context.Background(), recoverer, opsWriter,
		asset, quote, "oracle recovered, verified by hand", false); err != nil {
		t.Fatalf("unfreezePair: %v", err)
	}

	if !recoverer.probed {
		t.Fatal("MarkRecovered never ran — the race window was never reached")
	}
	if recoverer.sawFrozen {
		t.Error("a concurrent aggregator tick would still see this pair as FROZEN between Clear and " +
			"MarkRecovered: it rehydrates the escalated ladder from the durable row, re-writes the marker, " +
			"and the operator's unfreeze is silently undone")
	}

	// End state: both authorities agree the freeze is over.
	if _, present, err := aggregator.LoadState(context.Background(), asset, quote); err != nil || present {
		t.Errorf("after the unfreeze the pair still reads frozen (present=%v, err=%v)", present, err)
	}
}

// TestNewFreezeWriterForOps_ReadsTheDurableLadder — `-list`'s ladder column
// must survive a Redis flush too. Before the ladder store was wired here,
// the exact situation an operator is most likely to be investigating (an
// escalated freeze whose marker evaporated) printed as EXTS 0 / ESCALATED
// false: it lied in the reassuring direction.
func TestNewFreezeWriterForOps_ReadsTheDurableLadder(t *testing.T) {
	asset, quote := testPair(t)

	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	ladder := newFakeLadderStore()
	now := time.Now().UTC()
	want := freeze.State{
		FiredAt:        now.Add(-3 * time.Hour),
		HoldUntil:      now.Add(20 * time.Minute),
		ExtensionsUsed: freeze.DefaultMaxExtensions,
		Escalated:      true,
	}
	if err := ladder.SaveLadder(context.Background(), asset, quote, want); err != nil {
		t.Fatalf("SaveLadder: %v", err)
	}
	mr.FlushAll() // Redis lost the marker; the durable row is untouched.

	writer, err := newFreezeWriterForOps(rdb, ladder)
	if err != nil {
		t.Fatalf("newFreezeWriterForOps: %v", err)
	}
	got, present, err := writer.LoadState(context.Background(), asset, quote)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if !present {
		t.Fatal("-list would report MARKER GONE with a zero ladder after a Redis flush")
	}
	if !got.Escalated || got.ExtensionsUsed != freeze.DefaultMaxExtensions {
		t.Errorf("ladder = {escalated:%v exts:%d}, want {escalated:true exts:%d}",
			got.Escalated, got.ExtensionsUsed, freeze.DefaultMaxExtensions)
	}
}

// TestListOpenFreezes_DistinguishesRehydratedFromGone — the STATE column
// must not collapse "Redis lost the marker, the ladder is still holding"
// into "GONE".
//
// Before migration 0119 the column was a marker-only probe, so a flushed
// Redis printed GONE — which an operator reads as "nothing to do here" on
// exactly the pair whose marker just evaporated. After 0119 LoadState falls
// back to the ladder, so a marker-only reading of `present` would swing the
// other way and print `live` for a marker that does not exist. Both are
// wrong; the three states have to be distinguished.
func TestListOpenFreezes_DistinguishesRehydratedFromGone(t *testing.T) {
	asset, quote := testPair(t)

	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	ladder := newFakeLadderStore()
	writer, err := newFreezeWriterForOps(rdb, ladder)
	if err != nil {
		t.Fatalf("newFreezeWriterForOps: %v", err)
	}
	looker, err := freeze.NewLooker(rdb)
	if err != nil {
		t.Fatalf("NewLooker: %v", err)
	}
	lister := &fakeOpenLister{pairs: []freeze.OpenFreezePair{{Asset: asset, Quote: quote}}}

	now := time.Now().UTC()
	escalated := freeze.State{
		FiredAt:        now.Add(-2*time.Hour - 5*time.Minute),
		HoldUntil:      now.Add(25 * time.Minute),
		ExtensionsUsed: freeze.DefaultMaxExtensions,
		Escalated:      true,
	}
	if err := writer.MarkHold(context.Background(), asset, quote, "0.87",
		anomaly.Decision{Action: anomaly.ActionFreeze}, escalated, 30*time.Minute); err != nil {
		t.Fatalf("MarkHold: %v", err)
	}

	// Marker present → live.
	if out := captureStdout(t, func() {
		if err := listOpenFreezes(context.Background(), lister, writer, looker); err != nil {
			t.Fatalf("listOpenFreezes: %v", err)
		}
	}); !strings.Contains(out, "live") || strings.Contains(out, "rehydrated") {
		t.Errorf("with the marker present the STATE column should be `live`, got:\n%s", out)
	}

	// Redis flushed, ladder still holding → rehydrated (NOT GONE, NOT live).
	mr.FlushAll()
	out := captureStdout(t, func() {
		if err := listOpenFreezes(context.Background(), lister, writer, looker); err != nil {
			t.Fatalf("listOpenFreezes: %v", err)
		}
	})
	if !strings.Contains(out, "rehydrated") {
		t.Errorf("a flushed marker with a live durable ladder must print `rehydrated`, got:\n%s", out)
	}
	if strings.Contains(out, "GONE") {
		t.Errorf("printed GONE for a pair that is still frozen to every API caller:\n%s", out)
	}
	// The ladder itself must still be reported truthfully.
	if !strings.Contains(out, "true") {
		t.Errorf("ESCALATED column lost after the flush:\n%s", out)
	}

	// Ladder retired as well → GONE.
	if err := writer.Clear(context.Background(), asset, quote); err != nil {
		t.Fatalf("Clear: %v", err)
	}
	if out := captureStdout(t, func() {
		if err := listOpenFreezes(context.Background(), lister, writer, looker); err != nil {
			t.Fatalf("listOpenFreezes: %v", err)
		}
	}); !strings.Contains(out, "GONE") {
		t.Errorf("with neither authority holding it the STATE column should be `GONE`, got:\n%s", out)
	}
}

// fakeOpenLister is the -list source.
type fakeOpenLister struct{ pairs []freeze.OpenFreezePair }

func (l *fakeOpenLister) ListOpen(context.Context) ([]freeze.OpenFreezePair, error) {
	return l.pairs, nil
}

// captureStdout runs fn with os.Stdout redirected to a pipe and returns what
// it wrote. listOpenFreezes prints the table to stdout by design (it is an
// operator report), so asserting on it means asserting on the pipe.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	orig := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	os.Stdout = w
	done := make(chan string, 1)
	go func() {
		var buf bytes.Buffer
		_, _ = io.Copy(&buf, r)
		done <- buf.String()
	}()
	fn()
	_ = w.Close()
	os.Stdout = orig
	return <-done
}
