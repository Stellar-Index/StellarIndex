//go:build integration

package integration_test

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"math/big"
	"testing"
	"time"

	"github.com/stellar/go-stellar-sdk/strkey"

	c "github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/canonical/discovery"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// anchorFixture seeds trades and lets a test stamp each one's usd_volume, the
// anchor's weight, independently of its legs.
type anchorFixture struct {
	t     *testing.T
	ctx   context.Context
	store *timescale.Store
	now   time.Time
	nonce int
	usd   map[int]string
}

func newAnchorFixture(t *testing.T) *anchorFixture {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	t.Cleanup(cancel)
	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return &anchorFixture{t: t, ctx: ctx, store: store, now: time.Now().UTC().Truncate(time.Minute), usd: map[int]string{}}
}

// contract returns a deterministic contract asset, registered as discovered
// so the detail query can resolve it.
func (f *anchorFixture) contract(label string) c.Asset {
	f.t.Helper()
	h := sha256.Sum256([]byte("xlm-usd-anchor/" + label))
	id, err := strkey.Encode(strkey.VersionByteContract, h[:])
	if err != nil {
		f.t.Fatal(err)
	}
	a, err := c.NewSorobanAsset(id)
	if err != nil {
		f.t.Fatal(err)
	}
	if err := f.store.RecordDiscovered(f.ctx, discovery.Hit{
		ContractID: id, Kind: discovery.KindSEP41, EventType: discovery.EventTransfer,
		Ledger: 50_000_000, ObservedAtRFC3339: f.now.Add(-8 * 24 * time.Hour).Format(time.RFC3339),
	}); err != nil {
		f.t.Fatalf("RecordDiscovered %s: %v", label, err)
	}
	return a
}

// trade records base -> quote at ago before now; usd, when set, is the
// trade's usd_volume.
func (f *anchorFixture) trade(source string, ago time.Duration, base, quote c.Asset, baseAmt, quoteAmt int64, usd string) {
	f.t.Helper()
	p, err := c.NewPair(base, quote)
	if err != nil {
		f.t.Fatal(err)
	}
	f.nonce++
	if err := f.store.InsertTrade(f.ctx, mkIntegrationTrade(source, f.nonce, f.now.Add(-ago), p, baseAmt, quoteAmt)); err != nil {
		f.t.Fatalf("InsertTrade %s #%d: %v", source, f.nonce, err)
	}
	if usd != "" {
		f.usd[50_000_000+f.nonce] = usd
	}
}

// refresh stamps usd_volume, materialises prices_1m and runs the rollup.
func (f *anchorFixture) refresh() {
	f.t.Helper()
	db := f.store.DB()
	for ledger, usd := range f.usd {
		if _, err := db.ExecContext(f.ctx, `UPDATE trades SET usd_volume = $1::numeric WHERE ledger = $2`, usd, ledger); err != nil {
			f.t.Fatalf("stamp usd_volume: %v", err)
		}
	}
	for _, q := range []string{
		`UPDATE trades SET usd_volume = 100 WHERE usd_volume IS NULL`,
		`CALL refresh_continuous_aggregate('prices_1m', NULL, NULL)`,
	} {
		if _, err := db.ExecContext(f.ctx, q); err != nil {
			f.t.Fatalf("%s: %v", q, err)
		}
	}
	if err := f.store.RefreshAssetListingRollups(f.ctx); err != nil {
		f.t.Fatalf("RefreshAssetListingRollups: %v", err)
	}
}

// snapshot returns the rollup's stored price and change_7d_pct for id; ok is
// false when the rollup did not price it.
func (f *anchorFixture) snapshot(id string) (price, change7d sql.NullString, ok bool) {
	f.t.Helper()
	err := f.store.DB().QueryRowContext(f.ctx,
		`SELECT price_usd::text, change_7d_pct::text FROM asset_price_snapshot WHERE asset_id = $1`, id).
		Scan(&price, &change7d)
	if err == sql.ErrNoRows {
		return price, change7d, false
	}
	if err != nil {
		f.t.Fatalf("snapshot %s: %v", id, err)
	}
	return price, change7d, true
}

func ratEq(t *testing.T, got, want string) bool {
	t.Helper()
	g, ok1 := new(big.Rat).SetString(got)
	w, ok2 := new(big.Rat).SetString(want)
	return ok1 && ok2 && g.Cmp(w) == 0
}

// TestXLMUSDAnchor_AliasDirectionPrecedenceAndTimeOfTrade seeds XLM/USD as a
// timeline of single-form windows and prices one XLM-quoted asset in each,
// on the rollup (grid anchor) and the detail query (lateral anchor):
//
//	N-12m   native form, both directions: 0.40 ($300) + 1/2 ($100) -> 0.425
//	N-2m    SAC dust 0.90 ($0.50), newer than the native minute -> ignored
//	N-3h10m crypto:XLM/fiat:USD only (CEX)                   0.30
//	N-6h5m  (USDC-SAC, XLM-SAC) only, the inverted direction 0.20
//	N-10h1m native 0.99 — 61 min before S's trade            -> S unpriced
//	N-12h59 native 0.25 — 59 min before T's trade            -> T priced
//	N-7d2h30m native 0.40, seeding the 7d grid edge for E's change.
//
// Every asset trades at 2 XLM (R at 4, E at 1), so price = 2 × anchor at
// its own minute. On main, Q, R and T are unpriced (no native/USDC row),
// native reads 0.40, and P reads 0.80.
func TestXLMUSDAnchor_AliasDirectionPrecedenceAndTimeOfTrade(t *testing.T) {
	f := newAnchorFixture(t)
	const unit, cexUnit = 10_000_000, 100_000_000
	usdc, err := c.NewClassicAsset("USDC", "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")
	if err != nil {
		t.Fatal(err)
	}
	usdcSAC, err := c.NewSorobanAsset("CCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUZPUTHXSTZLEO7SJMI75")
	if err != nil {
		t.Fatal(err)
	}
	xlmSAC, err := c.NewSorobanAsset(c.XLMSacContractID)
	if err != nil {
		t.Fatal(err)
	}
	native := c.NativeAsset()
	cryptoXLM := c.Asset{Type: c.AssetCrypto, Code: "XLM"}
	fiatUSD := c.Asset{Type: c.AssetFiat, Code: "USD"}
	m, h := time.Minute, time.Hour

	f.trade("sdex", 12*m, native, usdc, 10*unit, 4*unit, "300")
	f.trade("sdex", 12*m, usdc, native, 1*unit, 2*unit, "100")
	f.trade("soroswap", 2*m, xlmSAC, usdcSAC, 10*unit, 9*unit, "0.5")
	f.trade("coinbase", 3*h+10*m, cryptoXLM, fiatUSD, 10*cexUnit, 3*cexUnit, "3")
	f.trade("aquarius", 6*h+5*m, usdcSAC, xlmSAC, 1*unit, 5*unit, "1")
	f.trade("sdex", 10*h+1*m, native, usdc, 10*unit, 99*unit/10, "9.9")
	f.trade("sdex", 12*h+59*m, native, usdc, 4*unit, 1*unit, "1")
	f.trade("sdex", 7*24*h+2*h+30*m, native, usdc, 10*unit, 4*unit, "4")

	P, Q, R, S := f.contract("P"), f.contract("Q"), f.contract("R"), f.contract("S")
	T, U, E := f.contract("T"), f.contract("U"), f.contract("E")
	H, K := f.contract("H"), f.contract("K")
	f.trade("soroswap", 1*m, P, xlmSAC, 1*unit, 2*unit, "")
	f.trade("sdex", 3*h, Q, native, 1*unit, 2*unit, "")
	f.trade("aquarius", 6*h, xlmSAC, R, 4*unit, 1*unit, "")
	f.trade("sdex", 9*h, S, native, 1*unit, 2*unit, "")
	f.trade("sdex", 12*h, T, native, 1*unit, 2*unit, "")
	f.trade("soroswap", 2*24*h, U, usdc, 2*unit, 3*unit, "")
	f.trade("sdex", 9*h, U, native, 1*unit, 2*unit, "")
	f.trade("sdex", 1*m, E, native, 1*unit, 1*unit, "")
	f.trade("sdex", 7*24*h+2*h-5*m, E, native, 1*unit, 1*unit, "")
	f.trade("soroswap", 3*h, K, xlmSAC, 1*unit, 2*unit, "")
	f.trade("soroswap", 30*m, H, K, 1*unit, 3*unit, "")
	f.refresh()

	want := map[string]string{
		P.String(): "0.85", // native-form anchor; the newer SAC dust is ignored
		Q.String(): "0.60", // crypto:XLM-only window, at Q's own minute
		R.String(): "0.80", // SAC-only, inverted-only window
		T.String(): "0.50", // anchor 59 min old
		U.String(): "1.5",  // stale anchor -> the older direct row still prices
		E.String(): "0.425",
	}
	for id, w := range want {
		price, _, ok := f.snapshot(id)
		if !ok || !ratEq(t, price.String, w) {
			t.Errorf("rollup %s price_usd = %v (ok=%v), want %s", id, price.String, ok, w)
		}
		row, err := f.store.GetAssetBySlug(f.ctx, id)
		if err != nil {
			t.Errorf("GetAssetBySlug %s: %v", id, err)
			continue
		}
		if row.PriceUSD == nil || !ratEq(t, *row.PriceUSD, w) {
			t.Errorf("detail %s price_usd = %s, want %s", id, derefOrNil(row.PriceUSD), w)
		}
	}

	if _, _, ok := f.snapshot(S.String()); ok {
		t.Errorf("rollup priced S through an anchor 61 min older than its trade")
	}
	if row, err := f.store.GetAssetBySlug(f.ctx, S.String()); err != nil || row.PriceUSD != nil {
		t.Errorf("detail S price_usd = %s (err %v), want unpriced", derefOrNil(row.PriceUSD), err)
	}

	if _, ch, _ := f.snapshot(E.String()); !ch.Valid || !ratEq(t, ch.String, "6.25") {
		t.Errorf("rollup E change_7d_pct = %v, want 6.25 (anchor at the 7d grid edge)", ch.String)
	}

	if price, _, ok := f.snapshot("native"); !ok || !ratEq(t, price.String, "0.425") {
		t.Errorf("rollup native price_usd = %v, want 0.425 (volume_usd-weighted, both directions)", price.String)
	}
	nrow, err := f.store.GetNativeAssetRow(f.ctx)
	if err != nil {
		t.Fatalf("GetNativeAssetRow: %v", err)
	}
	if nrow.PriceUSD == nil || !ratEq(t, *nrow.PriceUSD, "0.425") {
		t.Errorf("native row price_usd = %s, want 0.425", derefOrNil(nrow.PriceUSD))
	}

	cands, err := f.store.TransitiveUSDPriceCandidates(f.ctx, H.String())
	if err != nil {
		t.Fatalf("TransitiveUSDPriceCandidates: %v", err)
	}
	if len(cands) != 1 || cands[0].Hop != K.String() || !ratEq(t, cands[0].PriceUSD, "1.8") {
		t.Errorf("transitive H via K = %+v, want one candidate at 1.8 (3 K × 2 XLM × 0.30 at K's minute)", cands)
	}
}

// TestXLMUSDAnchor_NoFreshAnchorUnpricesOnlyTheXLMArm: with XLM/USD last
// printed 2 h ago, native and an XLM-quoted asset are unpriced while a
// direct-USD asset keeps its price (so the zero-row prune guard is not what
// saves it). On main all three priced, the XLM ones at the 2-hour-old rate.
func TestXLMUSDAnchor_NoFreshAnchorUnpricesOnlyTheXLMArm(t *testing.T) {
	f := newAnchorFixture(t)
	const unit = 10_000_000
	usdc, err := c.NewClassicAsset("USDC", "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")
	if err != nil {
		t.Fatal(err)
	}
	native := c.NativeAsset()
	f.trade("sdex", 2*time.Hour, native, usdc, 10*unit, 4*unit, "4")
	D, X := f.contract("D"), f.contract("X")
	f.trade("soroswap", 10*time.Minute, D, usdc, 1*unit, 2*unit, "")
	f.trade("sdex", 5*time.Minute, X, native, 1*unit, 2*unit, "")
	f.refresh()

	if price, _, ok := f.snapshot(D.String()); !ok || !ratEq(t, price.String, "2") {
		t.Errorf("direct asset price_usd = %v (ok=%v), want 2", price.String, ok)
	}
	for _, id := range []string{"native", X.String()} {
		if price, _, ok := f.snapshot(id); ok {
			t.Errorf("rollup priced %s at %s with no XLM/USD within the hour", id, price.String)
		}
	}
	nrow, err := f.store.GetNativeAssetRow(f.ctx)
	if err != nil {
		t.Fatalf("GetNativeAssetRow: %v", err)
	}
	if nrow.PriceUSD != nil {
		t.Errorf("native row price_usd = %s, want unpriced", *nrow.PriceUSD)
	}
}
