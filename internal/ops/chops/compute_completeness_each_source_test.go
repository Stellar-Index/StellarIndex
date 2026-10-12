// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package chops

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/completeness"
	"github.com/Stellar-Index/StellarIndex/internal/config"
	"github.com/Stellar-Index/StellarIndex/internal/sources/defindex"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TestEvaluateEachSource_OneFailureDoesNotWithholdLaterVerdicts: a first-error return in the per-source loop left every
// later source on a stale verdict. Every selected source must still be evaluated, and the run must fail naming each
// source that errored.
func TestEvaluateEachSource_OneFailureDoesNotWithholdLaterVerdicts(t *testing.T) {
	cat := []reconSource{{name: "soroswap"}, {name: "aquarius"}, {name: "phoenix"}, {name: "blend"}}
	errSoroswap := errors.New("soroswap: projection: deadline")
	errPhoenix := errors.New("phoenix: served floor: boom")

	var evaluated []string
	err := evaluateEachSource(context.Background(), cat, "", 0, func(_ context.Context, src reconSource) error {
		evaluated = append(evaluated, src.name)
		switch src.name {
		case "soroswap":
			return errSoroswap
		case "phoenix":
			return errPhoenix
		}
		return nil
	})

	if want := []string{"soroswap", "aquarius", "phoenix", "blend"}; !slices.Equal(evaluated, want) {
		t.Errorf("evaluated %v, want every source %v — a failing source must not stop the pass", evaluated, want)
	}
	if !errors.Is(err, errSoroswap) || !errors.Is(err, errPhoenix) {
		t.Errorf("err = %v, want both source failures joined (the run must exit non-zero)", err)
	}
}

// TestEvaluateEachSource_HonoursTheSourceFilter: -source still limits the
// pass to one source.
func TestEvaluateEachSource_HonoursTheSourceFilter(t *testing.T) {
	cat := []reconSource{{name: "soroswap"}, {name: "aquarius"}, {name: "phoenix"}}
	var evaluated []string
	if err := evaluateEachSource(context.Background(), cat, "aquarius", 0, func(_ context.Context, src reconSource) error {
		evaluated = append(evaluated, src.name)
		return nil
	}); err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if !slices.Equal(evaluated, []string{"aquarius"}) {
		t.Errorf("evaluated %v, want only aquarius", evaluated)
	}
}

// TestEvaluateEachSource_StopsOnADoneContext: once the pass deadline has
// passed every remaining source would fail the same way, so the walk stops
// and reports what it did not evaluate.
func TestEvaluateEachSource_StopsOnADoneContext(t *testing.T) {
	cat := []reconSource{{name: "soroswap"}, {name: "aquarius"}, {name: "phoenix"}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var evaluated []string
	err := evaluateEachSource(ctx, cat, "", 0, func(_ context.Context, src reconSource) error {
		evaluated = append(evaluated, src.name)
		cancel()
		return nil
	})
	if !slices.Equal(evaluated, []string{"soroswap"}) {
		t.Errorf("evaluated %v, want only soroswap before the context ended", evaluated)
	}
	if !errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), "aquarius") {
		t.Errorf("err = %v, want context.Canceled naming the first unevaluated source", err)
	}
	if !strings.Contains(err.Error(), "phoenix") {
		t.Errorf("err = %v, want every skipped source named, including phoenix", err)
	}
}

// TestOrderForPass: in -pass mode a source that re-verifies from genesis runs
// after every incrementally-resuming source, so its cost cannot exhaust the
// deadline before the cheap sources publish.
func TestOrderForPass(t *testing.T) {
	cat := []reconSource{{name: "soroswap", genesis: 10}, {name: "sdex", genesis: 2}, {name: "aquarius", genesis: 10}, {name: "phoenix", genesis: 10}}
	names := func(srcs []reconSource) []string {
		out := make([]string, len(srcs))
		for i, s := range srcs {
			out[i] = s.name
		}
		return out
	}
	allOK := map[string]priorProjection{
		"soroswap": {known: true, ok: true}, "sdex": {known: true, ok: true},
		"aquarius": {known: true, ok: true}, "phoenix": {known: true, ok: true},
	}
	wm := map[string]uint32{"soroswap": 500, "sdex": 500, "aquarius": 500, "phoenix": 500}

	if got := names(orderForPass(cat, allOK, wm)); !slices.Equal(got, names(cat)) {
		t.Errorf("all incremental: got %v, want catalogue order %v", got, names(cat))
	}

	failing := maps.Clone(allOK)
	failing["sdex"] = priorProjection{known: true, ok: false}
	if got, want := names(orderForPass(cat, failing, wm)), []string{"soroswap", "aquarius", "phoenix", "sdex"}; !slices.Equal(got, want) {
		t.Errorf("failing prior: got %v, want %v", got, want)
	}

	unknown := maps.Clone(allOK)
	delete(unknown, "soroswap")
	failing2 := maps.Clone(unknown)
	failing2["aquarius"] = priorProjection{known: true, ok: false}
	if got, want := names(orderForPass(cat, failing2, wm)), []string{"sdex", "phoenix", "soroswap", "aquarius"}; !slices.Equal(got, want) {
		t.Errorf("unknown + failing prior: got %v, want %v (stable within each group)", got, want)
	}

	if got := names(cat); !slices.Equal(got, []string{"soroswap", "sdex", "aquarius", "phoenix"}) {
		t.Errorf("orderForPass mutated its input: %v", got)
	}
}

// TestOrderForPassCensusLastFromGenesis: when SDEX re-verifies from genesis
// alongside the SEP-41 sources, its census runs after them so a census that
// overruns the pass cannot leave sep41_transfers/sep41_supply without a verdict.
func TestOrderForPassCensusLastFromGenesis(t *testing.T) {
	cat := []reconSource{
		{name: "soroswap", genesis: 10},
		{name: "sdex", genesis: 2, census: true},
		{name: "sep41_transfers", genesis: 10},
		{name: "sep41_supply", genesis: 10},
	}
	prior := map[string]priorProjection{
		"soroswap": {known: true, ok: true},
		"sdex":     {known: true, ok: false},
	}
	wm := map[string]uint32{"soroswap": 500, "sdex": 500}
	got := make([]string, 0, len(cat))
	for _, s := range orderForPass(cat, prior, wm) {
		got = append(got, s.name)
	}
	if want := []string{"soroswap", "sep41_transfers", "sep41_supply", "sdex"}; !slices.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}

	// An incrementally-resuming census stays in catalogue order: only its
	// full-history re-derive is the long one.
	prior["sdex"] = priorProjection{known: true, ok: true}
	got = got[:0]
	for _, s := range orderForPass(cat, prior, wm) {
		got = append(got, s.name)
	}
	if want := []string{"soroswap", "sdex", "sep41_transfers", "sep41_supply"}; !slices.Equal(got, want) {
		t.Errorf("incremental census: got %v, want %v", got, want)
	}
}

// TestEvaluateEachSource_PerSourceBudgetDoesNotStarveLaterSources: a source that
// outlives its own budget fails alone; the sources after it still run.
func TestEvaluateEachSource_PerSourceBudgetDoesNotStarveLaterSources(t *testing.T) {
	cat := []reconSource{{name: "comet"}, {name: "blend"}}
	var evaluated []string
	err := evaluateEachSource(context.Background(), cat, "", 20*time.Millisecond, func(ctx context.Context, src reconSource) error {
		evaluated = append(evaluated, src.name)
		if src.name == "comet" {
			<-ctx.Done()
			return ctx.Err()
		}
		return nil
	})
	if !slices.Equal(evaluated, []string{"comet", "blend"}) {
		t.Errorf("evaluated %v, want blend to run after comet timed out", evaluated)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want comet's deadline reported (no fake verdict)", err)
	}
}

// With -source the run's -timeout is the one source's budget: the per-source
// bound must not cut the weekly `-source sdex` re-proof (hours) at 45m.
func TestEvaluateEachSource_SourceFilterIgnoresPerSourceBudget(t *testing.T) {
	cat := []reconSource{{name: "sdex"}}
	err := evaluateEachSource(context.Background(), cat, "sdex", time.Nanosecond, func(ctx context.Context, _ reconSource) error {
		time.Sleep(5 * time.Millisecond)
		return ctx.Err()
	})
	if err != nil {
		t.Fatalf("err = %v, want the -source run to keep the run budget", err)
	}
}

// A reproofOutlastsPass source is never re-floored on expired evidence, and
// when it does start from genesis it runs after the light from-genesis
// sources and before the census.
func TestPass_HeavyReproofIsNotForcedAndRunsLate(t *testing.T) {
	now := time.Now()
	old := now.Add(-30 * 24 * time.Hour)
	cat := []reconSource{
		{name: "sdex", census: true, genesis: 1},
		{name: "defindex", genesis: 1},
		{name: "sep41_transfers", genesis: 1, reproofOutlastsPass: "heavy"},
	}
	prior := map[string]priorProjection{
		"defindex":        {known: true, ok: true, evidencedAt: old},
		"sep41_transfers": {known: true, ok: true, evidencedAt: old},
	}
	if got := expireStaleCarries(prior, cat, now, completeness.MaxProjectionCarryAge); !slices.Equal(got, []string{"defindex"}) {
		t.Errorf("expired = %v, want only defindex", got)
	}
	var names []string
	for _, s := range orderForPass(cat, map[string]priorProjection{}, map[string]uint32{}) {
		names = append(names, s.name)
	}
	if want := []string{"defindex", "sep41_transfers", "sdex"}; !slices.Equal(names, want) {
		t.Errorf("order = %v, want %v", names, want)
	}
}

// defindex's lake read must be contract-scoped, and the scope must cover
// everything its decoder can accept, or the expected side undercounts.
func TestCatalogue_DefindexReDeriveIsContractScoped(t *testing.T) {
	cat, _, err := buildReconciliationCatalogue(config.Config{})
	if err != nil {
		t.Fatalf("buildReconciliationCatalogue: %v", err)
	}
	src := catalogueSource(t, cat, "defindex")
	if len(src.contractIDs) == 0 {
		t.Fatal("defindex has no contractIDs: its re-derive streams the whole lake")
	}
	for _, c := range append(append([]string{}, defindex.MainnetVaults...), defindex.MainnetStrategies...) {
		if !slices.Contains(src.contractIDs, c) {
			t.Errorf("defindex prefilter omits gated contract %s", c)
		}
	}
	if src.outlastsPass() {
		t.Error("defindex is contract-scoped now; it must stay in the pass's evidence re-proof")
	}
	if s := catalogueSource(t, cat, "sdex"); !s.outlastsPass() {
		t.Error("sdex census must stay out of the pass's forced re-proof")
	}
	var cfg config.Config
	cfg.Supply.WatchedSEP41Contracts = []string{defindex.MainnetVaults[0]}
	sep, err := buildSEP41ReconSources(cfg)
	if err != nil {
		t.Fatalf("buildSEP41ReconSources: %v", err)
	}
	if s := catalogueSource(t, sep, "sep41_transfers"); !s.outlastsPass() {
		t.Error("sep41_transfers must stay out of the pass's forced re-proof")
	}
}

// TestOrderForPass_ExpiredCarryJoinsTheFromGenesisGroup: a re-verify forced by
// an expired carry is as slow as any other from-genesis reconcile, so it must
// run after the cheap incremental sources, not ahead of them.
func TestOrderForPass_ExpiredCarryJoinsTheFromGenesisGroup(t *testing.T) {
	cat := []reconSource{{name: "a", genesis: 10}, {name: "b", genesis: 10}}
	prior := map[string]priorProjection{
		"a": {known: true, ok: true, tip: 100, evidenceExpired: true},
		"b": {known: true, ok: true, tip: 100},
	}
	got := orderForPass(cat, prior, map[string]uint32{"a": 100, "b": 100})
	if got[0].name != "b" || got[1].name != "a" {
		t.Errorf("order = [%s %s], want [b a]", got[0].name, got[1].name)
	}
}

// recordingPublisher captures what publishSourceVerdict hands the store, to observe the arguments only;
// applied/cleared behaviour is proven on real TimescaleDB in test/integration/completeness_test.go.
type recordingPublisher struct {
	clear *timescale.DirtyWindowClear
	calls int
	out   timescale.VerdictPublication
}

func (r *recordingPublisher) PublishCompletenessVerdict(_ context.Context, _ timescale.CompletenessSnapshot, c *timescale.DirtyWindowClear) (timescale.VerdictPublication, error) {
	r.calls++
	r.clear = c
	return r.out, nil
}

// The clear travels INTO the verdict's transaction — identified by
// the exact row this run read — only when the run earned it; an unearned
// run must hand the store no clear at all.
func TestPublishSourceVerdict_ClearOnlyWhenEarned(t *testing.T) {
	stamp := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	win := timescale.ProjectionDirtyWindow{Source: "cctp", From: 62_000_000, To: 62_500_000, UpdatedAt: stamp}

	earned := &recordingPublisher{}
	if _, err := publishSourceVerdict(context.Background(), earned, timescale.CompletenessSnapshot{Source: "cctp"}, win, true); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if earned.calls != 1 || earned.clear == nil {
		t.Fatalf("earned run: calls=%d clear=%v, want one call carrying a clear", earned.calls, earned.clear)
	}
	if *earned.clear != (timescale.DirtyWindowClear{From: 62_000_000, To: 62_500_000, UpdatedAt: stamp}) {
		t.Errorf("clear = %+v, want the exact window this run read (bounds AND updated_at)", *earned.clear)
	}

	unearned := &recordingPublisher{}
	if _, err := publishSourceVerdict(context.Background(), unearned, timescale.CompletenessSnapshot{Source: "cctp"}, win, false); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if unearned.calls != 1 || unearned.clear != nil {
		t.Fatalf("unearned run: calls=%d clear=%+v, want one call with a nil clear", unearned.calls, unearned.clear)
	}
}

// The operator-facing line must not read like a stored verdict when the
// never-regress guard rejected it.
func TestVerdictNotStoredNote(t *testing.T) {
	if got := verdictNotStoredNote(timescale.VerdictPublication{Applied: true, WindowCleared: true}, 63_600_000, true); got != "" {
		t.Errorf("applied verdict: note = %q, want empty", got)
	}
	got := verdictNotStoredNote(timescale.VerdictPublication{}, 63_000_000, true)
	for _, want := range []string{"VERDICT NOT STORED", "63000000", "stays PENDING"} {
		if !strings.Contains(got, want) {
			t.Errorf("rejected verdict with a pending window: note %q lacks %q", got, want)
		}
	}
	if got := verdictNotStoredNote(timescale.VerdictPublication{}, 63_000_000, false); strings.Contains(got, "PENDING") || !strings.Contains(got, "VERDICT NOT STORED") {
		t.Errorf("rejected verdict, no window: note = %q", got)
	}
}

// A frozen ledgerstream cursor must not become the verdict tip: the network
// keeps closing ledgers, so a verdict stamped against it would read "complete
// to tip" for a tip the chain left behind.
func TestTipFromLiveCursor_RefusesFrozenCursor(t *testing.T) {
	now := time.Date(2026, 9, 1, 5, 30, 0, 0, time.UTC)
	cur := timescale.Cursor{Source: "ledgerstream", LastLedger: 63_000_000, UpdatedAt: now.Add(-3 * time.Hour)}
	tip, err := tipFromLiveCursor(cur, now)
	if err == nil {
		t.Fatalf("tipFromLiveCursor = %d, nil; want an error for a cursor frozen 3h", tip)
	}
	if !strings.Contains(err.Error(), "frozen at ledger 63000000") || !strings.Contains(err.Error(), "-to") {
		t.Errorf("error must name the frozen ledger and the -to override, got: %v", err)
	}
}

func TestTipFromLiveCursor_AcceptsLiveCursor(t *testing.T) {
	now := time.Date(2026, 9, 1, 5, 30, 0, 0, time.UTC)
	for _, age := range []time.Duration{0, 4 * time.Second, 30 * time.Minute, time.Hour} {
		cur := timescale.Cursor{Source: "ledgerstream", LastLedger: 63_000_000, UpdatedAt: now.Add(-age)}
		tip, err := tipFromLiveCursor(cur, now)
		if err != nil || tip != 63_000_000 {
			t.Errorf("cursor %s old: tipFromLiveCursor = %d, %v; want 63000000, nil", age, tip, err)
		}
	}
}

// The nightly wrapper passes -to (cursor minus a margin); a frozen cursor must
// still be refused on that path so computed_at is not refreshed.
func TestResolveVerdictTip_ExplicitToStillRefusesFrozenCursor(t *testing.T) {
	now := time.Date(2026, 9, 1, 5, 30, 0, 0, time.UTC)
	frozen := timescale.Cursor{Source: "ledgerstream", LastLedger: 63_000_000, UpdatedAt: now.Add(-30 * time.Hour)}
	if tip, err := resolveVerdictTip(62_999_900, frozen, now, false); err == nil {
		t.Fatalf("resolveVerdictTip = %d, nil; want refusal for -to against a cursor frozen 30h", tip)
	}
	if tip, err := resolveVerdictTip(0, frozen, now, true); err == nil {
		t.Fatalf("resolveVerdictTip = %d, nil; override without -to must still refuse", tip)
	}
	if tip, err := resolveVerdictTip(62_999_900, frozen, now, true); err != nil || tip != 62_999_900 {
		t.Errorf("override: resolveVerdictTip = %d, %v; want 62999900, nil", tip, err)
	}
}

func TestResolveVerdictTip_LiveCursor(t *testing.T) {
	now := time.Date(2026, 9, 1, 5, 30, 0, 0, time.UTC)
	live := timescale.Cursor{Source: "ledgerstream", LastLedger: 63_000_000, UpdatedAt: now.Add(-4 * time.Second)}
	for _, tc := range []struct{ to, want uint32 }{{0, 63_000_000}, {62_999_900, 62_999_900}} {
		if tip, err := resolveVerdictTip(tc.to, live, now, false); err != nil || tip != tc.want {
			t.Errorf("to=%d: resolveVerdictTip = %d, %v; want %d, nil", tc.to, tip, err, tc.want)
		}
	}
}

func TestVerdictTipFromCursorRead_MissingCursor(t *testing.T) {
	now := time.Now()
	if tip, err := verdictTipFromCursorRead(70_000_000, timescale.Cursor{}, timescale.ErrNotFound, now, false); err != nil || tip != 70_000_000 {
		t.Errorf("-to with no cursor row: tip=%d err=%v; want 70000000, nil", tip, err)
	}
	if tip, err := verdictTipFromCursorRead(0, timescale.Cursor{}, timescale.ErrNotFound, now, false); err == nil {
		t.Errorf("no -to, no cursor row: tip=%d; want error", tip)
	}
	if tip, err := verdictTipFromCursorRead(70_000_000, timescale.Cursor{}, errors.New("conn reset"), now, false); err == nil {
		t.Errorf("-to with transient read error: tip=%d; want error", tip)
	}
}

// TestProjectionEvidence_CarryKeepsThePriorProofTime pins the published age of projection evidence: only a run that
// reconciled the whole served range stamps now; a carry restates the prior claim and keeps its proof time, or each
// nightly incremental run would present a weeks-old proof as fresh.
func TestProjectionEvidence_CarryKeepsThePriorProofTime(t *testing.T) {
	const servedFrom = uint32(61_609_957)
	proven := time.Date(2026, 9, 1, 5, 40, 0, 0, time.UTC)
	prior := priorProjection{known: true, ok: true, tip: 64_000_000, evidencedAt: proven}

	if now, at := projectionEvidence(true, servedFrom, servedFrom, prior); !now || !at.IsZero() {
		t.Errorf("full-range reconcile: (now=%v, at=%v), want (true, zero) — this run is the proof", now, at)
	}
	if now, at := projectionEvidence(true, servedFrom, 64_000_001, prior); now || !at.Equal(proven) {
		t.Errorf("carried claim: (now=%v, at=%v), want (false, %v) — a carry is not new evidence", now, at, proven)
	}
	if now, at := projectionEvidence(true, servedFrom, 64_000_001, priorProjection{known: true, ok: true, tip: 64_000_000}); now || !at.IsZero() {
		t.Errorf("carry from a prior with no evidence on record: (now=%v, at=%v), want (false, zero)", now, at)
	}
	if now, at := projectionEvidence(false, servedFrom, servedFrom, prior); now || !at.IsZero() {
		t.Errorf("failing claim: (now=%v, at=%v), want (false, zero) — no claim, no evidence", now, at)
	}
}

// TestProjectionFloor_PassReVerifiesAnExpiredCarry: a clean prior whose
// full-range evidence is past -max-carry-age must re-reconcile from genesis,
// so the run proves the whole served range instead of carrying it yet again.
func TestProjectionFloor_PassReVerifiesAnExpiredCarry(t *testing.T) {
	expired := priorProjection{known: true, ok: true, tip: sushiTip, evidenceExpired: true}
	if got := projectionFloor(sushiGenesis, true, expired, sushiTip, 0); got != sushiGenesis {
		t.Errorf("expired-carry floor = %d, want genesis %d", got, sushiGenesis)
	}
	// Outside -pass the floor stays operator-stated.
	if got := projectionFloor(sushiGenesis, false, expired, sushiTip, 63_000_000); got != 63_000_000 {
		t.Errorf("non-pass floor = %d, want the -from 63000000", got)
	}
}

func TestExpireStaleCarries(t *testing.T) {
	now := time.Date(2026, 10, 2, 5, 30, 0, 0, time.UTC)
	const maxAge = 7 * 24 * time.Hour
	cat := []reconSource{{name: "fresh"}, {name: "old"}, {name: "unknown"}, {name: "failing"}}
	prior := map[string]priorProjection{
		"fresh":   {known: true, ok: true, evidencedAt: now.Add(-time.Hour)},
		"old":     {known: true, ok: true, evidencedAt: now.Add(-maxAge - time.Minute)},
		"unknown": {known: true, ok: true},
		"failing": {known: true, ok: false},
	}
	expireStaleCarries(prior, cat, now, maxAge)
	for name, want := range map[string]bool{"fresh": false, "old": true, "unknown": true, "failing": false} {
		if got := prior[name].evidenceExpired; got != want {
			t.Errorf("%s: evidenceExpired = %v, want %v", name, got, want)
		}
	}

	disabled := map[string]priorProjection{"unknown": {known: true, ok: true}}
	expireStaleCarries(disabled, []reconSource{{name: "unknown"}}, now, 0)
	if disabled["unknown"].evidenceExpired {
		t.Error("-max-carry-age 0 must carry without bound")
	}
}

// TestExpireStaleCarries_NeverRefloorsTheCensus: a full SDEX re-derive outlasts
// the pass's deadline and a deadline-cut source writes nothing, so a forced
// re-floor would recur every night without ever refreshing the evidence.
func TestExpireStaleCarries_NeverRefloorsTheCensus(t *testing.T) {
	now := time.Date(2026, 10, 2, 5, 30, 0, 0, time.UTC)
	cat := []reconSource{{name: "sdex", census: true}, {name: "aquarius"}}
	prior := map[string]priorProjection{
		"sdex":     {known: true, ok: true},
		"aquarius": {known: true, ok: true},
	}
	got := expireStaleCarries(prior, cat, now, completeness.MaxProjectionCarryAge)
	if prior["sdex"].evidenceExpired {
		t.Error("census source marked expired; it must keep carrying with its evidence ageing honestly")
	}
	if len(got) != 1 || got[0] != "aquarius" {
		t.Errorf("refloored = %v, want [aquarius]", got)
	}
	if f := projectionFloor(100, true, prior["sdex"], 64_000_000, 0); f != 64_000_000 {
		t.Errorf("census -pass floor = %d, want its prior watermark 64000000", f)
	}
}

// TestExpireStaleCarries_CapsOldestFirst: at most maxEvidenceRefloorsPerPass
// sources re-prove per pass, unknown evidence first, then the oldest; the rest
// keep carrying, which staggers the next expiry instead of re-flooring all at once.
func TestExpireStaleCarries_CapsOldestFirst(t *testing.T) {
	if maxEvidenceRefloorsPerPass != 3 {
		t.Fatalf("maxEvidenceRefloorsPerPass = %d, test assumes 3", maxEvidenceRefloorsPerPass)
	}
	now := time.Date(2026, 10, 2, 5, 30, 0, 0, time.UTC)
	day := 24 * time.Hour
	cat := []reconSource{{name: "a"}, {name: "b"}, {name: "c"}, {name: "d"}, {name: "e"}}
	prior := map[string]priorProjection{
		"a": {known: true, ok: true, evidencedAt: now.Add(-8 * day)},
		"b": {known: true, ok: true, evidencedAt: now.Add(-20 * day)},
		"c": {known: true, ok: true},
		"d": {known: true, ok: true, evidencedAt: now.Add(-9 * day)},
		"e": {known: true, ok: true, evidencedAt: now.Add(-day)},
	}
	got := expireStaleCarries(prior, cat, now, completeness.MaxProjectionCarryAge)
	if strings.Join(got, ",") != "c,b,d" {
		t.Errorf("refloored = %v, want [c b d] (unknown first, then oldest)", got)
	}
	for name, want := range map[string]bool{"a": false, "b": true, "c": true, "d": true, "e": false} {
		if prior[name].evidenceExpired != want {
			t.Errorf("%s: evidenceExpired = %v, want %v", name, prior[name].evidenceExpired, want)
		}
	}
	if !prior["a"].evidencedAt.Equal(now.Add(-8 * day)) {
		t.Error("a source left over the cap must keep its old evidence time")
	}
}

func TestBuildPriorVerdicts_CarriesTheEvidenceTime(t *testing.T) {
	proven := time.Date(2026, 9, 1, 5, 40, 0, 0, time.UTC)
	priorProj, _, _, _ := buildPriorVerdicts([]timescale.CompletenessSnapshot{
		{Source: "sdex", ProjectionOK: true, Watermark: 64_000_000, ProjectionEvidencedAt: proven},
	})
	if got := priorProj["sdex"].evidencedAt; !got.Equal(proven) {
		t.Errorf("prior evidencedAt = %v, want %v", got, proven)
	}
}

func TestCarriedEvidenceDetail(t *testing.T) {
	if d := carriedEvidenceDetail(time.Time{}); !strings.Contains(d, "no full-range reconcile on record") {
		t.Errorf("unknown evidence detail = %q", d)
	}
	at := time.Date(2026, 9, 1, 5, 40, 0, 0, time.UTC)
	if d := carriedEvidenceDetail(at); !strings.Contains(d, "2026-09-01T05:40:00Z") {
		t.Errorf("detail = %q, want it to name the proof time", d)
	}
}

// A prior clean projection verdict carries only as far as its Watermark (the
// range it reconciled), never to Tip, which sits above a recognition gap.
func TestBuildPriorVerdicts_ProjectionCarryBoundsToWatermarkNotTip(t *testing.T) {
	const (
		servedFrom = uint32(61_500_000)
		watermark  = uint32(62_000_000)
		tip        = uint32(62_500_000)
	)
	snaps := []timescale.CompletenessSnapshot{
		{Source: "soroswap", ProjectionOK: true, SubstrateOK: true, RecognitionOK: true, Tip: tip, Watermark: watermark},
	}
	priorProj, _, _, _ := buildPriorVerdicts(snaps)

	prior := priorProj["soroswap"]
	if prior.tip != watermark {
		t.Fatalf("priorProj[soroswap].tip = %d, want %d (Watermark, not Tip=%d)", prior.tip, watermark, tip)
	}
	ok, detail := projectionClaim(servedFrom, tip, tip, true, "", prior, testScope)
	if ok {
		t.Fatalf("projectionClaim carried a prior verdict over [%d,%d], a band the prior run never reconciled", watermark+1, tip-1)
	}
	if !strings.Contains(detail, fmt.Sprintf("%d", watermark)) {
		t.Errorf("rejection detail must name the prior verdict's true reach (watermark=%d), got: %s", watermark, detail)
	}
}

func TestBuildPriorVerdicts_CarriesProjectionVerifiedFrom(t *testing.T) {
	prior, _, _, _ := buildPriorVerdicts([]timescale.CompletenessSnapshot{
		{Source: "band", ProjectionOK: true, Watermark: bandTip, ProjectionVerifiedFrom: bandServedMin},
	})
	if got := prior["band"].verifiedFrom; got != bandServedMin {
		t.Fatalf("priorProj[band].verifiedFrom = %d, want %d", got, bandServedMin)
	}
}

// -pass resumes each source's projection from its own watermark when its prior
// verdict is clean (keeping the nightly cheap), from genesis when it is red or
// unseeded; outside -pass the floor is the operator-stated max(genesis, -from).
func TestProjectionFloor(t *testing.T) {
	const (
		aquariusGenesis     = uint32(52_728_375)
		healthyGenesis      = uint32(50_746_266)
		blendEmitterGenesis = uint32(51_499_914)
		tip                 = uint32(63_997_554)
	)
	clean := priorProjection{known: true, ok: true, tip: tip}
	clean63 := priorProjection{known: true, ok: true, tip: 63_000_000}
	failing63 := priorProjection{known: true, ok: false, tip: 63_000_000}
	cases := []struct {
		name      string
		genesis   uint32
		pass      bool
		prior     priorProjection
		watermark uint32
		from      uint
		want      uint32
	}{
		{"pass: healthy resumes at watermark", healthyGenesis, true, clean, tip - 100, 0, tip - 100},
		{"pass: recognition-capped resumes at its low watermark", aquariusGenesis, true, clean, 55_363_631, 0, 55_363_631},
		{"pass: never-seeded floors at genesis", blendEmitterGenesis, true, priorProjection{}, 0, 0, blendEmitterGenesis},
		{"pass: sub-genesis watermark clamps", blendEmitterGenesis, true, clean63, 40_000_000, 0, blendEmitterGenesis},
		{"pass: clean prior keeps the cheap resume", sushiGenesis, true, priorProjection{known: true, ok: true, tip: sushiTip}, sushiTip, 0, sushiTip},
		{"pass: failing prior re-verifies from genesis", sushiGenesis, true, priorProjection{known: true, ok: false, tip: sushiTip}, sushiTip, 0, sushiGenesis},
		{"non-pass -from, clean prior", healthyGenesis, false, clean63, 999_999, 63_000_000, 63_000_000},
		{"non-pass -from, failing prior", healthyGenesis, false, failing63, 999_999, 63_000_000, 63_000_000},
		{"non-pass -from, no prior", healthyGenesis, false, priorProjection{}, 999_999, 63_000_000, 63_000_000},
		{"non-pass full run ignores the watermark", healthyGenesis, false, clean63, 63_000_000, 0, healthyGenesis},
	}
	for _, tc := range cases {
		if got := projectionFloor(tc.genesis, tc.pass, tc.prior, tc.watermark, tc.from); got != tc.want {
			t.Errorf("%s: projectionFloor = %d, want %d", tc.name, got, tc.want)
		}
	}
}
