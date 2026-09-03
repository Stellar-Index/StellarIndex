//go:build integration

package integration_test

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	c "github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TestXLMBaseRestamp_RederivesThroughTheLiveAnchor is the DB-backed proof
// for `usd-volume-restamp -tier xlm-base` (issue #372), on real
// TimescaleDB, against rows the REAL insert path wrote with the REAL
// resolver wiring:
//
//  1. the re-derived value is the ANCHOR value — base_amount/1e7 x the
//     XLM/USD rate the resolver reads out of prices_1m at the row's ts —
//     and not the quote-side thin-book number the pre-fd1860bd waterfall
//     stored;
//  2. DRY RUN WRITES NOTHING: planning the window leaves every row's
//     value and derive_generation byte-identical;
//  3. -write writes EXACTLY the rows the dry run reported, and nothing
//     else in the window moves;
//  4. a row the anchor cannot price is left alone — a stored NULL stays
//     NULL rather than inheriting the quote-side estimate, and a stored
//     (wrong) value is not blanked;
//  5. the NULL population is opt-in (-fill-null) and counted either way;
//  6. INV-3: a row at a HIGHER derive_generation is never clawed back,
//     and every rewritten row carries the run's generation;
//  7. idempotent: an immediate re-run plans zero rows;
//  8. the exact tiers are NOT touched by this tier (a USD-pegged quote
//     belongs to `-tier exact`), so the two tools cannot undo each other.
func TestXLMBaseRestamp_RederivesThroughTheLiveAnchor(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	const usdcID = "USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
	usdc, err := c.NewClassicAsset("USDC", "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")
	if err != nil {
		t.Fatal(err)
	}
	token, err := c.NewSorobanAsset("CAFJZQWSED6YAWZU3GWRTOCNPPCGBN32L7QV43XX5LZLFTK6JLN34DLN")
	if err != nil {
		t.Fatal(err)
	}
	xlm := c.NativeAsset()
	xlmUSDC, err := c.NewPair(xlm, usdc)
	if err != nil {
		t.Fatal(err)
	}
	xlmToken, err := c.NewPair(xlm, token)
	if err != nil {
		t.Fatal(err)
	}

	// The blessed wiring every trade writer gets — NOT a hand-built
	// resolver, so the re-derive runs against exactly the production
	// resolution the insert path uses.
	if err := timescale.InstallUSDVolumeResolution(store, []string{usdcID}, nil); err != nil {
		t.Fatalf("InstallUSDVolumeResolution: %v", err)
	}

	day := time.Date(2026, 6, 10, 0, 0, 0, 0, time.UTC)
	anchorTS := day.Add(10 * time.Hour)

	// ── the XLM/USD anchor: 100 XLM for 50 USDC -> vwap 0.5 ──────────
	// Clears the resolver's $0.01 direct-leg dust floor by a wide margin
	// (quote notional $50), so it is the rate every XLM-base row below
	// resolves through.
	anchor := mkIntegrationTrade("sdex", 1, anchorTS, xlmUSDC, 1_000_000_000, 500_000_000)
	if err := store.InsertTrade(ctx, anchor); err != nil {
		t.Fatalf("InsertTrade anchor: %v", err)
	}

	// ── the THIN TOKEN/USDC BOOK: the #372 defect's other half ───────
	// A direct `<token>/USDC` market 4h later, when the XLM/USD anchor
	// above has aged past the resolver's 1h direct-leg freshness. This is
	// exactly the 2026-05-19 BUCK shape: the XLM leg cannot be priced,
	// but the counterparty-authored token book CAN — so the pre-fd1860bd
	// waterfall valued the trade through it. A restamp that fell through
	// to the quote side (rather than reporting "the anchor declined")
	// would re-commit that value at a winning derive_generation, which is
	// what fixtures 20/21 below prove it does not.
	tokenUSDC, err := c.NewPair(token, usdc)
	if err != nil {
		t.Fatal(err)
	}
	thinBook := mkIntegrationTrade("sdex", 2, anchorTS.Add(3*time.Hour+55*time.Minute), tokenUSDC, 1_000, 5_000_000)
	if err := store.InsertTrade(ctx, thinBook); err != nil {
		t.Fatalf("InsertTrade thin book: %v", err)
	}
	if _, err := store.DB().ExecContext(ctx,
		`CALL refresh_continuous_aggregate('prices_1m', NULL, NULL)`); err != nil {
		t.Fatalf("refresh prices_1m: %v", err)
	}

	// ── the XLM-base population ─────────────────────────────────────
	// Every one is 10 XLM against a pure SEP-41 token with no USD market,
	// so the anchor's answer is 10 x $0.50 = $5.00 exactly.
	const wantAnchored = "5.00000000"
	type fixture struct {
		name  string
		nonce int
		ts    time.Time
	}
	fixtures := []fixture{
		{"quote-side wrong", 10, anchorTS.Add(5 * time.Minute)},
		{"stored NULL", 11, anchorTS.Add(6 * time.Minute)},
		{"already correct", 12, anchorTS.Add(7 * time.Minute)},
		{"higher generation", 13, anchorTS.Add(8 * time.Minute)},
		// 4h past the anchor bucket: beyond the resolver's 1h direct-leg
		// freshness, and XLM is the bridge's own base case, so the anchor
		// declines — while the thin TOKEN/USDC book above is fresh, so
		// the QUOTE side can still price them. These two rows are the
		// whole point: the restamp must report them, not value them.
		{"declined, stored NULL", 20, anchorTS.Add(4 * time.Hour)},
		{"declined, stored value", 21, anchorTS.Add(4*time.Hour + time.Minute)},
	}
	ledger := map[string]uint32{}
	for _, f := range fixtures {
		tr := mkIntegrationTrade("sdex", f.nonce, f.ts, xlmToken, 100_000_000, 300)
		if err := store.InsertTrade(ctx, tr); err != nil {
			t.Fatalf("InsertTrade %s: %v", f.name, err)
		}
		ledger[f.name] = tr.Ledger
	}

	type row struct {
		usd *string
		gen int64
	}
	readRow := func(t *testing.T, l uint32) row {
		t.Helper()
		var (
			usd sql.NullString
			gen int64
		)
		if err := store.DB().QueryRowContext(ctx,
			`SELECT usd_volume::text, derive_generation FROM trades WHERE source = 'sdex' AND ledger = $1`, l,
		).Scan(&usd, &gen); err != nil {
			t.Fatalf("read ledger %d: %v", l, err)
		}
		if !usd.Valid {
			return row{nil, gen}
		}
		return row{&usd.String, gen}
	}
	snapshot := func(t *testing.T) map[uint32]row {
		t.Helper()
		out := map[uint32]row{}
		for _, l := range ledger {
			out[l] = readRow(t, l)
		}
		out[anchor.Ledger] = readRow(t, anchor.Ledger)
		out[thinBook.Ledger] = readRow(t, thinBook.Ledger)
		return out
	}
	sameSnapshot := func(t *testing.T, before, after map[uint32]row, what string) {
		t.Helper()
		for l, b := range before {
			a := after[l]
			switch {
			case (b.usd == nil) != (a.usd == nil):
				t.Errorf("%s: ledger %d usd_volume nullness changed (%v -> %v)", what, l, b.usd, a.usd)
			case b.usd != nil && *b.usd != *a.usd:
				t.Errorf("%s: ledger %d usd_volume %s -> %s", what, l, *b.usd, *a.usd)
			case b.gen != a.gen:
				t.Errorf("%s: ledger %d derive_generation %d -> %d", what, l, b.gen, a.gen)
			}
		}
	}

	// ── the pre-fd1860bd state, imposed by hand ──────────────────────
	// HEAD's insert path already writes the anchor value, so the defect
	// has to be re-created: row 10 valued quote-side through the token's
	// own thin book (the 2026-05-19 BUCK shape), row 11 unpriced, row 21
	// carrying a wrong value the anchor cannot re-derive.
	exec := func(t *testing.T, q string, args ...any) {
		t.Helper()
		if _, err := store.DB().ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("fixture %q: %v", q, err)
		}
	}
	exec(t, `UPDATE trades SET usd_volume = 0.00372265 WHERE source='sdex' AND ledger=$1`, ledger["quote-side wrong"])
	exec(t, `UPDATE trades SET usd_volume = NULL       WHERE source='sdex' AND ledger=$1`, ledger["stored NULL"])
	const higherGen = int64(9_000_000_000)
	exec(t, `UPDATE trades SET usd_volume = 0.11111111, derive_generation = $2 WHERE source='sdex' AND ledger=$1`,
		ledger["higher generation"], higherGen)
	// NOT poisoned by hand: the insert path itself valued this one
	// quote-side through the thin book (0.15 = 300/1e7 x the 5000 raw
	// TOKEN/USDC vwap), which IS the pre-fd1860bd defect state.

	// Sanity: the fixture really did land the states the test asserts on.
	if got := readRow(t, ledger["already correct"]); got.usd == nil || *got.usd != wantAnchored {
		t.Fatalf("fixture: the 'already correct' row is %v, want %s written by the insert path", got.usd, wantAnchored)
	}
	// The quote side really can price the declined pair — otherwise the
	// "only the anchor" assertions below would pass vacuously.
	quoteSide := readRow(t, ledger["declined, stored value"])
	if quoteSide.usd == nil {
		t.Fatal("fixture: the thin TOKEN/USDC book did not price the 4h row — the 'only the anchor' assertions would be vacuous")
	}
	if *quoteSide.usd == wantAnchored {
		t.Fatalf("fixture: the quote-side value %s coincides with the anchor value; pick amounts that differ", *quoteSide.usd)
	}
	exec(t, `UPDATE trades SET usd_volume = NULL WHERE source='sdex' AND ledger=$1`, ledger["declined, stored NULL"])

	const gen = int64(1_756_400_000)
	params := func(fillNull bool) timescale.XLMBaseRestampParams {
		return timescale.XLMBaseRestampParams{
			From: day, To: day.AddDate(0, 0, 1),
			FillNull: fillNull, MaxGeneration: gen,
		}
	}

	// ── 1+2. plan (the DRY RUN) and prove it wrote nothing ───────────
	before := snapshot(t)
	plan, err := store.PlanXLMBaseUSDVolumeRestamp(ctx, params(false))
	if err != nil {
		t.Fatalf("PlanXLMBaseUSDVolumeRestamp: %v", err)
	}
	sameSnapshot(t, before, snapshot(t), "dry run")

	if len(plan.Rows) != 1 {
		t.Fatalf("dry run planned %d row(s), want exactly 1 (the quote-side row); stats %+v", len(plan.Rows), plan.Stats)
	}
	if plan.Rows[0].Ledger != ledger["quote-side wrong"] {
		t.Fatalf("dry run planned ledger %d, want %d", plan.Rows[0].Ledger, ledger["quote-side wrong"])
	}
	if plan.Rows[0].Want != wantAnchored {
		t.Fatalf("planned value = %q, want %q (10 XLM x $0.50 via the anchor)", plan.Rows[0].Want, wantAnchored)
	}
	// 4. the two declined rows are counted, not written.
	if plan.Stats.AnchorDeclinedNull != 1 || plan.Stats.AnchorDeclinedStored != 1 {
		t.Errorf("declined split = %d null / %d valued, want 1/1 (stats %+v)",
			plan.Stats.AnchorDeclinedNull, plan.Stats.AnchorDeclinedStored, plan.Stats)
	}
	// 5. the NULL row is counted but not admitted.
	if plan.Stats.NullCandidates != 1 || plan.Stats.NullFilled != 0 {
		t.Errorf("null accounting = %d candidates / %d filled, want 1/0", plan.Stats.NullCandidates, plan.Stats.NullFilled)
	}
	// 6. the higher-generation row is out of scope entirely.
	for _, r := range plan.Rows {
		if r.Ledger == ledger["higher generation"] {
			t.Error("the higher-generation row entered the plan — INV-3 says a newer re-derive is never clawed back")
		}
	}
	// 8. the exact-tier anchor trade (native/USDC) is classified out.
	if plan.Stats.QuotePegged != 1 {
		t.Errorf("QuotePegged = %d, want 1 (the native/USDC anchor row belongs to `-tier exact`)", plan.Stats.QuotePegged)
	}
	if plan.Stats.Unchanged != 1 {
		t.Errorf("Unchanged = %d, want 1 (the already-correct row)", plan.Stats.Unchanged)
	}
	if got := plan.Stats.Residual(); got != 0 {
		t.Errorf("Residual() = %d, want 0 — a scanned row was filed nowhere (stats %+v)", got, plan.Stats)
	}

	// ── 3. -write writes EXACTLY the planned rows ────────────────────
	before = snapshot(t)
	n, err := store.ApplyXLMBaseUSDVolumeRestamp(ctx, plan, gen, 0)
	if err != nil {
		t.Fatalf("ApplyXLMBaseUSDVolumeRestamp: %v", err)
	}
	if n != int64(len(plan.Rows)) {
		t.Fatalf("applied %d row(s), want %d (the plan)", n, len(plan.Rows))
	}
	after := snapshot(t)
	fixed := after[ledger["quote-side wrong"]]
	if fixed.usd == nil || *fixed.usd != wantAnchored {
		t.Errorf("restamped row = %v, want %s", fixed.usd, wantAnchored)
	}
	if fixed.gen != gen {
		t.Errorf("restamped row derive_generation = %d, want the run's %d", fixed.gen, gen)
	}
	// Everything the plan did not name is byte-identical.
	delete(before, ledger["quote-side wrong"])
	delete(after, ledger["quote-side wrong"])
	sameSnapshot(t, before, after, "write run (unplanned rows)")

	// 4 (again), now against the written state: the declined rows kept
	// exactly what they held.
	if got := readRow(t, ledger["declined, stored NULL"]); got.usd != nil {
		t.Errorf("a row the anchor cannot price was given the value %s — it must stay NULL", *got.usd)
	}
	if got := readRow(t, ledger["declined, stored value"]); got.usd == nil {
		t.Error("a row the anchor cannot price was BLANKED — the restamp must never write NULL over a value")
	} else if *got.usd != *quoteSide.usd {
		t.Errorf("a row the anchor cannot price moved from %s to %s — it must be reported, not re-valued", *quoteSide.usd, *got.usd)
	}

	// ── 7. idempotent: an immediate re-run plans nothing ─────────────
	replan, err := store.PlanXLMBaseUSDVolumeRestamp(ctx, params(false))
	if err != nil {
		t.Fatalf("re-plan: %v", err)
	}
	if len(replan.Rows) != 0 {
		t.Errorf("re-run planned %d row(s), want 0 — the restamp is not idempotent", len(replan.Rows))
	}
	if replan.Stats.Unchanged != 2 {
		t.Errorf("re-run Unchanged = %d, want 2 (the original correct row plus the one just fixed)", replan.Stats.Unchanged)
	}

	// ── 5. -fill-null admits the NULL population ─────────────────────
	fillPlan, err := store.PlanXLMBaseUSDVolumeRestamp(ctx, params(true))
	if err != nil {
		t.Fatalf("plan with FillNull: %v", err)
	}
	if len(fillPlan.Rows) != 1 || fillPlan.Rows[0].Ledger != ledger["stored NULL"] || !fillPlan.Rows[0].NullFill {
		t.Fatalf("FillNull plan = %+v, want exactly the stored-NULL row", fillPlan.Rows)
	}
	if fillPlan.Rows[0].Want != wantAnchored {
		t.Errorf("NULL fill value = %q, want %q", fillPlan.Rows[0].Want, wantAnchored)
	}
	if _, err := store.ApplyXLMBaseUSDVolumeRestamp(ctx, fillPlan, gen, 0); err != nil {
		t.Fatalf("apply FillNull: %v", err)
	}
	if got := readRow(t, ledger["stored NULL"]); got.usd == nil || *got.usd != wantAnchored {
		t.Errorf("filled row = %v, want %s", got.usd, wantAnchored)
	}

	// ── 6. the higher-generation row never moved ─────────────────────
	if got := readRow(t, ledger["higher generation"]); got.gen != higherGen || got.usd == nil || *got.usd != "0.11111111" {
		t.Errorf("higher-generation row = %+v (usd %v), want it untouched at generation %d", got, got.usd, higherGen)
	}

	// ── the live-overlap guard's input: the window's top ledger ──────
	top, ok, err := store.MaxTradeLedgerInRange(ctx, day, day.AddDate(0, 0, 1))
	if err != nil || !ok {
		t.Fatalf("MaxTradeLedgerInRange = %d, %v, %v", top, ok, err)
	}
	if want := ledger["declined, stored value"]; top != want {
		t.Errorf("window top ledger = %d, want %d (the highest on-chain ledger in the window)", top, want)
	}
	if _, ok, err := store.MaxTradeLedgerInRange(ctx, day.AddDate(0, 0, 10), day.AddDate(0, 0, 11)); err != nil || ok {
		t.Errorf("an empty window reported a top ledger (ok=%v, err=%v)", ok, err)
	}
}

// TestXLMBaseRestamp_RefusesWithoutResolution is the configuration
// fail-closed: a re-derive whose resolver was never installed would
// report every row as "the anchor cannot price this", which is a wiring
// error wearing a finding's clothes — and, if it ever gained a write
// path, the A-CRIT-1 shape that overwrites correct values with NULL at a
// winning generation.
func TestXLMBaseRestamp_RefusesWithoutResolution(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	day := time.Date(2026, 6, 10, 0, 0, 0, 0, time.UTC)
	_, err = store.PlanXLMBaseUSDVolumeRestamp(ctx, timescale.XLMBaseRestampParams{
		From: day, To: day.AddDate(0, 0, 1), MaxGeneration: 1,
	})
	if err == nil {
		t.Fatal("planning without an installed FX resolver succeeded; want a refusal")
	}
	if want := "no USD-volume FX resolver installed"; !strings.Contains(err.Error(), want) {
		t.Fatalf("err = %v, want containing %q", err, want)
	}
}
