//go:build integration

package integration_test

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	c "github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/ops/chops"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// ─── the two mirror tiers, against a real COMPRESSED chunk ──────────────
//
// These are the DB-backed proofs for `usd-volume-restamp -tier xlm-quote`
// and `-tier cex-fx`, driven through the real subcommand (flags, config,
// live-tail guard, generation) on real TimescaleDB, against a `trades`
// chunk that is COMPRESSED the way every chunk older than the 7-day
// policy is on production. They are the mirrors of
// TestXLMBaseRestampChunks_RestampsInsideACompressedChunk, and they exist
// for the same reason: an in-place UPDATE into a compressed chunk
// measured ~1,574 rows/min on 2026-09-03, and these two populations are
// ~18.0M and ~12.6M rows.
//
// Each one pins the same four things:
//
//  1. the rows the tier owns are restamped to the value the LIVE insert
//     path computes, and stamped with the run's generation;
//  2. the chunk is COMPRESSED again afterwards;
//  3. a row the anchor / the FX feed cannot price is left EXACTLY as it
//     was — a stored NULL stays NULL — and is COUNTED in the report,
//     never guessed at;
//  4. a token/token spam row is untouched by both tiers, whatever the
//     window: the substance gate from the tier-3b valuation incident.

// restampRow is one row's money state, for the before/after comparisons.
type restampRow struct {
	usd *string
	gen int64
}

// mirrorRestampFixture is the shared harness: a store, a compressed
// `trades` chunk, a config file and the row readers the two tests below
// assert with.
type mirrorRestampFixture struct {
	store   *timescale.Store
	ctx     context.Context
	cfgPath string
	t       *testing.T
}

func newMirrorRestampFixture(t *testing.T, ctx context.Context, usdcID string) *mirrorRestampFixture {
	t.Helper()
	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	// The insert path's own wiring, so the seeded rows are what
	// production rows are.
	if err := timescale.InstallUSDVolumeResolution(store, []string{usdcID}, nil); err != nil {
		t.Fatalf("InstallUSDVolumeResolution: %v", err)
	}
	cfgPath := filepath.Join(t.TempDir(), "stellarindex.toml")
	cfg := fmt.Sprintf("[storage]\npostgres_dsn = %q\n\n[trades]\nusd_pegged_classic_assets = [%q]\n", dsn, usdcID)
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	return &mirrorRestampFixture{store: store, ctx: ctx, cfgPath: cfgPath, t: t}
}

func (f *mirrorRestampFixture) exec(q string, args ...any) {
	f.t.Helper()
	if _, err := f.store.DB().ExecContext(f.ctx, q, args...); err != nil {
		f.t.Fatalf("%q: %v", q, err)
	}
}

func (f *mirrorRestampFixture) read(source string, ledger uint32) restampRow {
	f.t.Helper()
	var (
		usd sql.NullString
		gen int64
	)
	if err := f.store.DB().QueryRowContext(f.ctx,
		`SELECT usd_volume::text, derive_generation FROM trades WHERE source = $1 AND ledger = $2`, source, ledger,
	).Scan(&usd, &gen); err != nil {
		f.t.Fatalf("read %s ledger %d: %v", source, ledger, err)
	}
	if !usd.Valid {
		return restampRow{nil, gen}
	}
	return restampRow{&usd.String, gen}
}

// compressTrades compresses every `trades` chunk, as the 7-day policy
// would have, and asserts the window's chunk is compressed.
func (f *mirrorRestampFixture) compressTrades(at time.Time) {
	f.t.Helper()
	f.exec(`SELECT compress_chunk(c, true) FROM show_chunks('trades') c`)
	if !f.chunkCompressed(at) {
		f.t.Fatal("fixture: the day's chunk did not compress")
	}
}

func (f *mirrorRestampFixture) chunkCompressed(at time.Time) bool {
	f.t.Helper()
	var compressed bool
	if err := f.store.DB().QueryRowContext(f.ctx, `
		SELECT is_compressed FROM timescaledb_information.chunks
		 WHERE hypertable_name = 'trades' AND range_start <= $1 AND range_end > $1`, at,
	).Scan(&compressed); err != nil {
		f.t.Fatalf("read chunk state: %v", err)
	}
	return compressed
}

// TestXLMQuoteRestampChunks_RestampsInsideACompressedChunk is the
// xlm-quote mirror's proof: DEX trades whose QUOTE leg is XLM are valued
// off that leg (`quote_amount/1e7 x XLM/USD at ts`) through the same
// anchor the xlm-base tier calls, inside a compressed chunk, which is
// left compressed.
func TestXLMQuoteRestampChunks_RestampsInsideACompressedChunk(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	const usdcID = "USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
	f := newMirrorRestampFixture(t, ctx, usdcID)

	usdc, err := c.NewClassicAsset("USDC", "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")
	if err != nil {
		t.Fatal(err)
	}
	token, err := c.NewSorobanAsset("CAFJZQWSED6YAWZU3GWRTOCNPPCGBN32L7QV43XX5LZLFTK6JLN34DLN")
	if err != nil {
		t.Fatal(err)
	}
	other, err := c.NewSorobanAsset("CAQQR5SWBXKIGZKPBZDH3KM5GQ5GUTPKB7JAFCINLZBC5WXPJKRG3IM7")
	if err != nil {
		t.Fatal(err)
	}
	xlm := c.NativeAsset()
	xlmUSDC, err := c.NewPair(xlm, usdc)
	if err != nil {
		t.Fatal(err)
	}
	// The mirror population: XLM in the QUOTE leg.
	tokenXLM, err := c.NewPair(token, xlm)
	if err != nil {
		t.Fatal(err)
	}
	// The spam population: neither leg is XLM, USD-pegged or fiat.
	tokenToken, err := c.NewPair(token, other)
	if err != nil {
		t.Fatal(err)
	}

	day := time.Date(2026, 6, 10, 0, 0, 0, 0, time.UTC)
	anchorTS := day.Add(10 * time.Hour)

	// XLM/USD anchor: 100 XLM for 50 USDC -> $0.50.
	anchor := mkIntegrationTrade("sdex", 1, anchorTS, xlmUSDC, 1_000_000_000, 500_000_000)
	if err := f.store.InsertTrade(ctx, anchor); err != nil {
		t.Fatalf("InsertTrade anchor: %v", err)
	}
	f.exec(`CALL refresh_continuous_aggregate('prices_1m', NULL, NULL)`)

	// 10 XLM in the QUOTE leg against a token with no USD market, so the
	// anchor's answer is exactly $5.00.
	const wantAnchored = "5.00000000"
	fixtures := []struct {
		name  string
		nonce int
		ts    time.Time
	}{
		{"quote-side wrong", 10, anchorTS.Add(5 * time.Minute)},
		{"stored NULL", 11, anchorTS.Add(6 * time.Minute)},
		{"already correct", 12, anchorTS.Add(7 * time.Minute)},
		// Three hours BEFORE the anchor trade: prices_1m holds no XLM/USD
		// bucket at or before this row, so the anchor declines it. It is
		// the "cannot price" case — reported, never guessed at.
		{"anchor declines", 13, anchorTS.Add(-3 * time.Hour)},
	}
	ledger := map[string]uint32{}
	var topLedger uint32
	for _, fx := range fixtures {
		tr := mkIntegrationTrade("sdex", fx.nonce, fx.ts, tokenXLM, 300, 100_000_000)
		if err := f.store.InsertTrade(ctx, tr); err != nil {
			t.Fatalf("InsertTrade %s: %v", fx.name, err)
		}
		ledger[fx.name] = tr.Ledger
		if tr.Ledger > topLedger {
			topLedger = tr.Ledger
		}
	}
	// The spam row: a token/token pair whose only possible rate is the
	// tier-3b bridge its own counterparties author.
	spam := mkIntegrationTrade("sdex", 20, anchorTS.Add(8*time.Minute), tokenToken, 10_000_000, 50_000_000_000_000)
	if err := f.store.InsertTrade(ctx, spam); err != nil {
		t.Fatalf("InsertTrade spam: %v", err)
	}
	if spam.Ledger > topLedger {
		topLedger = spam.Ledger
	}

	// The pre-fix state, imposed by hand.
	f.exec(`UPDATE trades SET usd_volume = 0.00372265 WHERE source='sdex' AND ledger=$1`, ledger["quote-side wrong"])
	f.exec(`UPDATE trades SET usd_volume = NULL       WHERE source='sdex' AND ledger=$1`, ledger["stored NULL"])

	// The row the anchor cannot price must START as NULL for the claim
	// "a stored NULL stays NULL" to mean anything.
	if got := f.read("sdex", ledger["anchor declines"]); got.usd != nil {
		t.Fatalf("fixture: the pre-anchor row was priced at insert (%s); the anchor was expected to decline it", *got.usd)
	}
	if got := f.read("sdex", spam.Ledger); got.usd != nil {
		t.Fatalf("fixture: the token/token row was priced at insert (%s)", *got.usd)
	}

	f.compressTrades(anchorTS)
	if err := f.store.UpsertCursor(ctx, "ledgerstream", "", topLedger+1_000); err != nil {
		t.Fatalf("UpsertCursor: %v", err)
	}

	const gen = "1756800000"
	args := []string{
		"usd-volume-restamp", "-config", f.cfgPath, "-tier", "xlm-quote", "-chunks",
		"-from", "2026-06-10", "-to", "2026-06-10", "-fill-null",
		"-generation", gen,
		// The database's data directory is inside the container, so the
		// host cannot statfs it: the operator-override path.
		"-min-free-bytes", fmt.Sprint(int64(1) << 40),
	}

	// ── the dry run: the plan is printed, nothing is decompressed ──────
	out, err := captureStdout(t, func() error { return chops.Run(args) })
	if err != nil {
		t.Fatalf("dry run: %v\n%s", err, out)
	}
	for _, want := range []string{
		"chunk plan: 1 trades chunk(s) intersect [2026-06-10, 2026-06-10] — 1 compressed, 0 not",
		"DRY RUN: nothing is decompressed",
		"would change 2 row(s)",
		"would restamp 2 row(s) in [2026-06-10, 2026-06-10] (tier xlm-quote)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("dry-run output lacks %q:\n%s", want, out)
		}
	}
	if !f.chunkCompressed(anchorTS) {
		t.Fatal("the dry run decompressed the chunk")
	}

	// ── -write: restamped through the anchor, chunk compressed again ───
	out, err = captureStdout(t, func() error { return chops.Run(append(args, "-write")) })
	if err != nil {
		t.Fatalf("write run: %v\n%s", err, out)
	}
	for _, want := range []string{
		"changed 2 row(s) (planned 2)",
		"restamped 2 row(s) in [2026-06-10, 2026-06-10] (tier xlm-quote)",
		"scanned (source=DEX, quote=XLM form, derive_generation <= 1756800000)",
		// The unpriceable row is REPORTED, with what it holds.
		"anchor declined, stored NULL   (coverage NOT recoverable)    1",
		"CALL refresh_continuous_aggregate('prices_1m'",
		"CALL refresh_continuous_aggregate('twap_1d'",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("write-run output lacks %q:\n%s", want, out)
		}
	}
	if !f.chunkCompressed(anchorTS) {
		t.Fatal("the chunk was left DECOMPRESSED after a successful write run")
	}
	for _, name := range []string{"quote-side wrong", "stored NULL"} {
		got := f.read("sdex", ledger[name])
		if got.usd == nil || *got.usd != wantAnchored {
			t.Errorf("%s: usd_volume = %v, want %s", name, got.usd, wantAnchored)
		}
		if fmt.Sprint(got.gen) != gen {
			t.Errorf("%s: derive_generation = %d, want the run's %s", name, got.gen, gen)
		}
	}
	// The row the anchor declined keeps its NULL, at its own generation.
	if got := f.read("sdex", ledger["anchor declines"]); got.usd != nil || got.gen != 0 {
		t.Errorf("the unpriceable row was written: usd=%v gen=%d", got.usd, got.gen)
	}
	// The token/token row is untouched — the substance gate.
	if got := f.read("sdex", spam.Ledger); got.usd != nil || got.gen != 0 {
		t.Errorf("the token/token row was valued by the xlm-quote tier: usd=%v gen=%d", got.usd, got.gen)
	}
	// The already-correct row and the exact-tier anchor row are untouched.
	if got := f.read("sdex", ledger["already correct"]); got.usd == nil || *got.usd != wantAnchored || got.gen != 0 {
		t.Errorf("the already-correct row moved: usd=%v gen=%d", got.usd, got.gen)
	}
	if got := f.read("sdex", anchor.Ledger); got.usd == nil || got.gen != 0 {
		t.Errorf("the exact-tier anchor row moved: usd=%v gen=%d", got.usd, got.gen)
	}

	// ── the rerun: probed, skipped, nothing moves ─────────────────────
	out, err = captureStdout(t, func() error { return chops.Run(append(args, "-write")) })
	if err != nil {
		t.Fatalf("rerun: %v\n%s", err, out)
	}
	for _, want := range []string{"nothing to change — skipped, chunk left compressed", "restamped 0 row(s)"} {
		if !strings.Contains(out, want) {
			t.Errorf("rerun output lacks %q:\n%s", want, out)
		}
	}
	if !f.chunkCompressed(anchorTS) {
		t.Fatal("the rerun left the chunk decompressed")
	}
}

// TestCEXFiatRestampChunks_RestampsInsideACompressedChunk is the cex-fx
// tier's proof: off-chain CEX trades quoted in a non-USD fiat are valued
// from `fx_quotes` at or before the trade, inside a compressed chunk,
// which is left compressed — and a trade whose nearest quote is outside
// the as-of tolerance is REFUSED rather than extrapolated to.
func TestCEXFiatRestampChunks_RestampsInsideACompressedChunk(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	const usdcID = "USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
	f := newMirrorRestampFixture(t, ctx, usdcID)

	btc, err := c.NewCryptoAsset("BTC")
	if err != nil {
		t.Fatal(err)
	}
	eur, err := c.NewFiatAsset("EUR")
	if err != nil {
		t.Fatal(err)
	}
	gbp, err := c.NewFiatAsset("GBP")
	if err != nil {
		t.Fatal(err)
	}
	usd, err := c.NewFiatAsset("USD")
	if err != nil {
		t.Fatal(err)
	}
	btcEUR, err := c.NewPair(btc, eur)
	if err != nil {
		t.Fatal(err)
	}
	btcGBP, err := c.NewPair(btc, gbp)
	if err != nil {
		t.Fatal(err)
	}
	btcUSD, err := c.NewPair(btc, usd)
	if err != nil {
		t.Fatal(err)
	}
	tokenA, err := c.NewSorobanAsset("CAFJZQWSED6YAWZU3GWRTOCNPPCGBN32L7QV43XX5LZLFTK6JLN34DLN")
	if err != nil {
		t.Fatal(err)
	}
	tokenB, err := c.NewSorobanAsset("CAQQR5SWBXKIGZKPBZDH3KM5GQ5GUTPKB7JAFCINLZBC5WXPJKRG3IM7")
	if err != nil {
		t.Fatal(err)
	}
	tokenToken, err := c.NewPair(tokenA, tokenB)
	if err != nil {
		t.Fatal(err)
	}

	day := time.Date(2026, 6, 10, 0, 0, 0, 0, time.UTC)
	ts := day.Add(10 * time.Hour)

	// The vendor feed, as the forex worker writes it: rate_usd is
	// UNITS-OF-TICKER PER 1 USD (migration 0028), so 0.8 EUR per USD is
	// $1.25 per EUR. Written as SQL rather than through the float64
	// batch API so the fixture rate is exact.
	//
	//   EUR — a bucket at 00:00 on the trades' own day (10 h before them)
	//   GBP — a bucket NINE DAYS earlier and nothing since: the nearest
	//         quote at or before the trade is outside the 7-day as-of
	//         tolerance, which is a refusal, not an extrapolation.
	f.exec(`INSERT INTO fx_quotes (bucket, ticker, rate_usd, inverse_usd, source) VALUES ($1, 'EUR', 0.8, 1.25, 'massive')`, day)
	f.exec(`INSERT INTO fx_quotes (bucket, ticker, rate_usd, inverse_usd, source) VALUES ($1, 'GBP', 0.5, 2.0,  'massive')`, day.AddDate(0, 0, -9))

	// 100.00000000 EUR (the CEX 1e8 amount scale) x $1.25 = $125.
	const wantEUR = "125.00000000"
	fixtures := []struct {
		name  string
		nonce int
		pair  c.Pair
	}{
		{"fx wrong", 30, btcEUR},
		{"stored NULL", 31, btcEUR},
		{"already correct", 32, btcEUR},
		{"outside tolerance", 33, btcGBP},
		{"USD quote (tier 1)", 34, btcUSD},
	}
	ledger := map[string]uint32{}
	var topLedger uint32
	for i, fx := range fixtures {
		tr := mkIntegrationTrade("binance", fx.nonce, ts.Add(time.Duration(i)*time.Minute), fx.pair, 100_000, 10_000_000_000)
		if err := f.store.InsertTrade(ctx, tr); err != nil {
			t.Fatalf("InsertTrade %s: %v", fx.name, err)
		}
		ledger[fx.name] = tr.Ledger
		if tr.Ledger > topLedger {
			topLedger = tr.Ledger
		}
	}
	spam := mkIntegrationTrade("sdex", 40, ts.Add(9*time.Minute), tokenToken, 10_000_000, 50_000_000_000_000)
	if err := f.store.InsertTrade(ctx, spam); err != nil {
		t.Fatalf("InsertTrade spam: %v", err)
	}
	if spam.Ledger > topLedger {
		topLedger = spam.Ledger
	}

	// The pre-fix state: the population this tier exists for is stored
	// NULL (prices_1m has no fiat pair, so before the resolver read
	// fx_quotes these rows fell through every tier).
	f.exec(`UPDATE trades SET usd_volume = 0.50000000 WHERE source='binance' AND ledger=$1`, ledger["fx wrong"])
	f.exec(`UPDATE trades SET usd_volume = NULL       WHERE source='binance' AND ledger=$1`, ledger["stored NULL"])
	if got := f.read("binance", ledger["already correct"]); got.usd == nil || *got.usd != wantEUR {
		t.Fatalf("fixture: the insert path valued the EUR row as %v, want %s — the tier's arithmetic and the insert path's have drifted", got.usd, wantEUR)
	}
	if got := f.read("binance", ledger["outside tolerance"]); got.usd != nil {
		t.Fatalf("fixture: the GBP row was priced at insert (%s) off a nine-day-old quote", *got.usd)
	}
	usdBefore := f.read("binance", ledger["USD quote (tier 1)"])
	if usdBefore.usd == nil {
		t.Fatal("fixture: the USD-quoted row is not priced; tier 1 should have valued it exactly")
	}

	f.compressTrades(ts)
	if err := f.store.UpsertCursor(ctx, "ledgerstream", "", topLedger+1_000); err != nil {
		t.Fatalf("UpsertCursor: %v", err)
	}

	const gen = "1756800000"
	args := []string{
		"usd-volume-restamp", "-config", f.cfgPath, "-tier", "cex-fx", "-chunks",
		"-from", "2026-06-10", "-to", "2026-06-10", "-fill-null",
		"-generation", gen,
		"-min-free-bytes", fmt.Sprint(int64(1) << 40),
	}

	// ── the as-of tolerance is the TOOL's, not just the resolver's ─────
	// At -fx-max-staleness 1h the EUR bucket (10 h before the trades) is
	// outside the tolerance, so the run refuses every row rather than
	// pricing them off it.
	narrow := append(append([]string{}, args...), "-fx-max-staleness", "1h")
	out, err := captureStdout(t, func() error { return chops.Run(narrow) })
	if err != nil {
		t.Fatalf("narrowed-tolerance dry run: %v\n%s", err, out)
	}
	for _, want := range []string{
		"would change 0 row(s)",
		"refused for want of a quote within the as-of tolerance",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("narrowed-tolerance output lacks %q:\n%s", want, out)
		}
	}
	// Widening past the live insert path's own lookback is refused.
	wide := append(append([]string{}, args...), "-fx-max-staleness", "240h")
	if _, err := captureStdout(t, func() error { return chops.Run(wide) }); err == nil ||
		!strings.Contains(err.Error(), "exceeds the live insert path") {
		t.Fatalf("-fx-max-staleness 240h: err = %v, want the widened-tolerance refusal", err)
	}

	// ── the dry run at the default tolerance ──────────────────────────
	out, err = captureStdout(t, func() error { return chops.Run(args) })
	if err != nil {
		t.Fatalf("dry run: %v\n%s", err, out)
	}
	for _, want := range []string{
		"DRY RUN: nothing is decompressed",
		"would change 2 row(s)",
		"would restamp 2 row(s) in [2026-06-10, 2026-06-10] (tier cex-fx)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("dry-run output lacks %q:\n%s", want, out)
		}
	}
	if !f.chunkCompressed(ts) {
		t.Fatal("the dry run decompressed the chunk")
	}

	// ── -write ────────────────────────────────────────────────────────
	out, err = captureStdout(t, func() error { return chops.Run(append(args, "-write")) })
	if err != nil {
		t.Fatalf("write run: %v\n%s", err, out)
	}
	for _, want := range []string{
		"changed 2 row(s) (planned 2)",
		"restamped 2 row(s) in [2026-06-10, 2026-06-10] (tier cex-fx)",
		"scanned (source=CEX, quote=non-USD fiat, derive_generation <= 1756800000)",
		// The row whose nearest quote is nine days old: refused, counted,
		// and said out loud.
		"fx declined, stored NULL   (coverage NOT recoverable)    1",
		"refused for want of a quote within the as-of tolerance  1",
		"CALL refresh_continuous_aggregate('prices_1m'",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("write-run output lacks %q:\n%s", want, out)
		}
	}
	if !f.chunkCompressed(ts) {
		t.Fatal("the chunk was left DECOMPRESSED after a successful write run")
	}
	for _, name := range []string{"fx wrong", "stored NULL"} {
		got := f.read("binance", ledger[name])
		if got.usd == nil || *got.usd != wantEUR {
			t.Errorf("%s: usd_volume = %v, want %s", name, got.usd, wantEUR)
		}
		if fmt.Sprint(got.gen) != gen {
			t.Errorf("%s: derive_generation = %d, want the run's %s", name, got.gen, gen)
		}
	}
	// Outside the tolerance: still NULL, never extrapolated to the
	// nine-day-old rate.
	if got := f.read("binance", ledger["outside tolerance"]); got.usd != nil || got.gen != 0 {
		t.Errorf("the out-of-tolerance row was written: usd=%v gen=%d", got.usd, got.gen)
	}
	// The USD-quoted row is tier 1's, and the token/token row is nobody's.
	if got := f.read("binance", ledger["USD quote (tier 1)"]); got.usd == nil || *got.usd != *usdBefore.usd || got.gen != usdBefore.gen {
		t.Errorf("the exact-tier USD row moved: %v -> %v", usdBefore, got)
	}
	if got := f.read("sdex", spam.Ledger); got.usd != nil || got.gen != 0 {
		t.Errorf("the token/token row was valued by the cex-fx tier: usd=%v gen=%d", got.usd, got.gen)
	}
	if got := f.read("binance", ledger["already correct"]); got.usd == nil || *got.usd != wantEUR || got.gen != 0 {
		t.Errorf("the already-correct row moved: usd=%v gen=%d", got.usd, got.gen)
	}

	// ── the rerun: probed, skipped, nothing moves ─────────────────────
	out, err = captureStdout(t, func() error { return chops.Run(append(args, "-write")) })
	if err != nil {
		t.Fatalf("rerun: %v\n%s", err, out)
	}
	for _, want := range []string{"nothing to change — skipped, chunk left compressed", "restamped 0 row(s)"} {
		if !strings.Contains(out, want) {
			t.Errorf("rerun output lacks %q:\n%s", want, out)
		}
	}
	if !f.chunkCompressed(ts) {
		t.Fatal("the rerun left the chunk decompressed")
	}
}
