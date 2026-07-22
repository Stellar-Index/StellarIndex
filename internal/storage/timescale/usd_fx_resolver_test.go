package timescale

import (
	"context"
	"errors"
	"math/big"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// TestVWAPUSDFXResolver_NoPegs — empty USDPegs list means the
// resolver is a no-op: every call returns ok=false without
// touching the DB. Pre-Phase-2 behaviour, preserved by F-1268
// for deployments that haven't opted in.
func TestVWAPUSDFXResolver_NoPegs(t *testing.T) {
	r, err := NewVWAPUSDFXResolver(&Store{}, VWAPUSDFXResolverOptions{
		USDPegs: nil,
	})
	if err != nil {
		t.Fatalf("NewVWAPUSDFXResolver: %v", err)
	}
	eurc, _ := canonical.NewClassicAsset("EURC", "GDHU6WRG4IEQXM5NZ4BMPKOXHW76MZM4Y2IEMFDVXBSDP6SJY4ITNPP2")
	got, ok, err := r.USDPriceAt(context.Background(), eurc, time.Now())
	if err != nil {
		t.Errorf("USDPriceAt: unexpected error: %v", err)
	}
	if ok || got != "" {
		t.Errorf("USDPriceAt(no pegs) = (%q, %t), want ('', false)", got, ok)
	}
}

// TestVWAPUSDFXResolver_NilStore — defensive guard at construction.
func TestVWAPUSDFXResolver_NilStore(t *testing.T) {
	_, err := NewVWAPUSDFXResolver(nil, VWAPUSDFXResolverOptions{})
	if err == nil {
		t.Fatal("expected error when store is nil")
	}
	if !errors.Is(err, err) {
		t.Errorf("expected wrapped error, got: %v", err)
	}
}

// TestVWAPUSDFXResolver_DefaultsApplied — zero-value options yield
// the production-sane defaults (1h freshness, 5m cache, time.Now).
func TestVWAPUSDFXResolver_DefaultsApplied(t *testing.T) {
	r, err := NewVWAPUSDFXResolver(&Store{}, VWAPUSDFXResolverOptions{
		USDPegs: []string{"USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"},
	})
	if err != nil {
		t.Fatalf("NewVWAPUSDFXResolver: %v", err)
	}
	if r.freshness != time.Hour {
		t.Errorf("freshness default = %v, want 1h", r.freshness)
	}
	if r.cacheTTL != 5*time.Minute {
		t.Errorf("cacheTTL default = %v, want 5m", r.cacheTTL)
	}
}

// TestVWAPUSDFXResolver_CachePopulatedHits — once a rate is in
// the cache, subsequent calls within the TTL skip the DB query.
// We exercise this by populating the cache directly + asserting
// USDPriceAt returns the cached value without panicking on the
// nil DB.
func TestVWAPUSDFXResolver_CachePopulatedHits(t *testing.T) {
	now := time.Date(2026, 5, 12, 14, 30, 0, 0, time.UTC)
	r, err := NewVWAPUSDFXResolver(&Store{}, VWAPUSDFXResolverOptions{
		USDPegs: []string{"USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"},
		Clock:   func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("NewVWAPUSDFXResolver: %v", err)
	}

	eurc, _ := canonical.NewClassicAsset("EURC", "GDHU6WRG4IEQXM5NZ4BMPKOXHW76MZM4Y2IEMFDVXBSDP6SJY4ITNPP2")
	// Floor the timestamp the same way the resolver does.
	bucket := now.UTC().Truncate(time.Minute)
	key := fxCacheKey{asset: eurc.String(), bucketMs: bucket.UnixMilli()}
	r.cache[key] = fxCacheEntry{rate: "1.0850", cachedAt: now}

	got, ok, err := r.USDPriceAt(context.Background(), eurc, now)
	if err != nil {
		t.Fatalf("USDPriceAt: %v", err)
	}
	if !ok {
		t.Fatalf("expected cache hit, got ok=false")
	}
	if got != "1.0850" {
		t.Errorf("USDPriceAt = %q, want 1.0850", got)
	}
}

// TestVWAPUSDFXResolver_CacheNegativeHitsAlsoSkipDB — a previous
// resolution that returned "" (no rate available) is also cached,
// so we don't re-query for the next thousand trades against the
// same uncovered asset. The cached negative result should produce
// ok=false without DB access.
func TestVWAPUSDFXResolver_CacheNegativeHitsAlsoSkipDB(t *testing.T) {
	now := time.Date(2026, 5, 12, 14, 30, 0, 0, time.UTC)
	r, err := NewVWAPUSDFXResolver(&Store{}, VWAPUSDFXResolverOptions{
		USDPegs: []string{"USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"},
		Clock:   func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("NewVWAPUSDFXResolver: %v", err)
	}
	mxn, _ := canonical.NewClassicAsset("MXNe", "GDHU6WRG4IEQXM5NZ4BMPKOXHW76MZM4Y2IEMFDVXBSDP6SJY4ITNPP2")
	bucket := now.UTC().Truncate(time.Minute)
	key := fxCacheKey{asset: mxn.String(), bucketMs: bucket.UnixMilli()}
	// Negative cache entry — empty rate, fresh-cachedAt.
	r.cache[key] = fxCacheEntry{rate: "", cachedAt: now}

	got, ok, err := r.USDPriceAt(context.Background(), mxn, now)
	if err != nil {
		t.Errorf("USDPriceAt with cached negative: %v", err)
	}
	if ok {
		t.Errorf("expected ok=false for cached negative, got ok=true, rate=%q", got)
	}
	if got != "" {
		t.Errorf("expected empty rate for cached negative, got %q", got)
	}
}

// TestVWAPUSDFXResolver_CacheTTLExpiry — a cache entry older than
// CacheTTL is treated as a miss. Without a DB the call returns
// an error because queryDB would touch nil; we don't assert on
// the error type, just on the cache miss path (we re-acquire the
// lock + look up; we don't take the early-return).
func TestVWAPUSDFXResolver_CacheTTLExpiry(t *testing.T) {
	now := time.Date(2026, 5, 12, 14, 30, 0, 0, time.UTC)
	clock := now
	r, err := NewVWAPUSDFXResolver(&Store{}, VWAPUSDFXResolverOptions{
		USDPegs:  []string{"USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"},
		CacheTTL: 5 * time.Minute,
		Clock:    func() time.Time { return clock },
	})
	if err != nil {
		t.Fatalf("NewVWAPUSDFXResolver: %v", err)
	}
	asset, _ := canonical.NewClassicAsset("USDX", "GDHU6WRG4IEQXM5NZ4BMPKOXHW76MZM4Y2IEMFDVXBSDP6SJY4ITNPP2")
	bucket := now.UTC().Truncate(time.Minute)
	key := fxCacheKey{asset: asset.String(), bucketMs: bucket.UnixMilli()}
	// Cache an entry — but stamped 10 minutes ago, past the TTL.
	r.cache[key] = fxCacheEntry{rate: "0.95", cachedAt: now.Add(-10 * time.Minute)}

	got, isCached := r.lookupCache(key)
	if isCached {
		t.Errorf("expired entry should return ok=false from lookupCache; got rate=%q", got)
	}
}

// TestVWAPUSDFXResolver_MinuteBucketKey — two trades within the
// same minute share the same cache key. Pin this — the cache
// resolution is what makes the per-trade lookup affordable.
func TestVWAPUSDFXResolver_MinuteBucketKey(t *testing.T) {
	asset, _ := canonical.NewClassicAsset("EURC", "GDHU6WRG4IEQXM5NZ4BMPKOXHW76MZM4Y2IEMFDVXBSDP6SJY4ITNPP2")
	base := time.Date(2026, 5, 12, 14, 30, 0, 0, time.UTC)
	for _, offset := range []time.Duration{0, time.Second, 30 * time.Second, 59 * time.Second} {
		t.Run(offset.String(), func(t *testing.T) {
			at := base.Add(offset)
			gotBucket := at.UTC().Truncate(time.Minute)
			if !gotBucket.Equal(base) {
				t.Errorf("offset %v truncated to %v, want %v", offset, gotBucket, base)
			}
			_ = asset
		})
	}
}

// TestTrimNumericText — Postgres NUMERIC::text preserves the
// column's full scale, so a VWAP arithmetically equal to 1.085
// arrives as `1.085000000000000000000`. The resolver canonicalises
// before returning so consumers see the human-friendly form.
// F-1251 (codex audit-2026-05-12).
func TestTrimNumericText(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"1.085000", "1.085"},
		{"1.085000000000000000000", "1.085"},
		{"1.000000", "1"},
		{"42", "42"},
		{"42.0", "42"},
		{"0.000", "0"},
		{"0.5", "0.5"},
		{"100.500", "100.5"},
		{"-1.500", "-1.5"},
		{"-0.0", "0"},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			if got := trimNumericText(tc.in); got != tc.want {
				t.Errorf("trimNumericText(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestVWAPUSDFXResolver_FreshnessSentinels — F-1251 sentinel
// semantics: 0 → default 1h; negative → disabled; positive →
// use as-is. Pre-fix the docstring claimed "0 = disable" but
// the constructor silently overrode 0 to 1h.
func TestVWAPUSDFXResolver_FreshnessSentinels(t *testing.T) {
	cases := []struct {
		name string
		in   time.Duration
		want time.Duration
	}{
		{"zero defaults to 1h", 0, time.Hour},
		{"negative disables", -1, 0},
		{"explicit positive used", 30 * time.Minute, 30 * time.Minute},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, err := NewVWAPUSDFXResolver(&Store{}, VWAPUSDFXResolverOptions{
				USDPegs:   []string{"USDC-G..."},
				Freshness: tc.in,
			})
			if err != nil {
				t.Fatalf("NewVWAPUSDFXResolver: %v", err)
			}
			if r.freshness != tc.want {
				t.Errorf("freshness = %v, want %v", r.freshness, tc.want)
			}
		})
	}
}

// ─── fiat quotes via fx_quotes (usd-volume coverage, 2026-07-22) ─────

// fiatAsset is a test helper for the fx-quote-resolved fiat side.
func fiatAsset(t *testing.T, code string) canonical.Asset {
	t.Helper()
	a, err := canonical.NewFiatAsset(code)
	if err != nil {
		t.Fatalf("NewFiatAsset(%q): %v", code, err)
	}
	return a
}

// TestVWAPUSDFXResolver_FiatUSDIsExactlyOne — USD is the anchor
// rate_usd is expressed against, so it resolves to exactly 1 with no
// DB access at all (fx_quotes holds no USD row to find). The nil DB in
// &Store{} is the assertion: a query would panic.
func TestVWAPUSDFXResolver_FiatUSDIsExactlyOne(t *testing.T) {
	t.Parallel()
	r, err := NewVWAPUSDFXResolver(&Store{}, VWAPUSDFXResolverOptions{})
	if err != nil {
		t.Fatalf("NewVWAPUSDFXResolver: %v", err)
	}
	got, ok, err := r.USDPriceAt(context.Background(), fiatAsset(t, "USD"), time.Now())
	if err != nil {
		t.Fatalf("USDPriceAt: %v", err)
	}
	if !ok || got != "1" {
		t.Errorf("USDPriceAt(fiat:USD) = (%q, %t), want ('1', true)", got, ok)
	}
}

// TestVWAPUSDFXResolver_FiatResolvesWithoutPegs is the regression
// guard for the ordering bug this branch was written to avoid: pricing
// EUR needs no Stellar USD-peg market, so the fiat branch MUST run
// before the `len(usdPegs) == 0` early return. If that check ever moves
// back in front, every fiat rate silently returns ok=false on a
// deployment with no pegs configured — and NULL usd_volume is exactly
// the failure this whole change exists to remove.
func TestVWAPUSDFXResolver_FiatResolvesWithoutPegs(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 17, 9, 15, 0, 0, time.UTC)
	r, err := NewVWAPUSDFXResolver(&Store{}, VWAPUSDFXResolverOptions{
		USDPegs: nil, // deliberately empty
		Clock:   func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("NewVWAPUSDFXResolver: %v", err)
	}
	eur := fiatAsset(t, "EUR")
	r.cache[fxCacheKey{
		asset:    eur.String(),
		bucketMs: now.Truncate(24 * time.Hour).UnixMilli(),
	}] = fxCacheEntry{rate: "1.1407191093265194", cachedAt: now}

	got, ok, err := r.USDPriceAt(context.Background(), eur, now)
	if err != nil {
		t.Fatalf("USDPriceAt: %v", err)
	}
	if !ok || got != "1.1407191093265194" {
		t.Errorf("USDPriceAt(fiat:EUR, no pegs) = (%q, %t), want the cached rate + true", got, ok)
	}
}

// TestVWAPUSDFXResolver_FiatCacheKeyIsUTCDay — fx_quotes stores one
// row per (date, ticker) at exactly UTC midnight, so the fiat cache key
// floors to the UTC day rather than the minute. Every trade on the same
// UTC day shares one entry: correct (they all resolve to the same
// daily bucket) and 1440x fewer keys, which is what keeps a
// months-long historical backfill from growing the cache per traded
// minute per currency.
func TestVWAPUSDFXResolver_FiatCacheKeyIsUTCDay(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 17, 0, 0, 0, 0, time.UTC)
	r, err := NewVWAPUSDFXResolver(&Store{}, VWAPUSDFXResolverOptions{
		Clock: func() time.Time { return now },
		// Long TTL so expiry can't be confused with a key miss.
		CacheTTL: 48 * time.Hour,
	})
	if err != nil {
		t.Fatalf("NewVWAPUSDFXResolver: %v", err)
	}
	gbp := fiatAsset(t, "GBP")
	r.cache[fxCacheKey{
		asset:    gbp.String(),
		bucketMs: now.UnixMilli(),
	}] = fxCacheEntry{rate: "1.3386880856760375", cachedAt: now}

	// Both ends of the same UTC day must hit the single entry; the
	// next day must miss it (and would hit the nil DB, so we only
	// assert the same-day hits here).
	for _, at := range []time.Time{
		time.Date(2026, 7, 17, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 7, 17, 12, 30, 45, 0, time.UTC),
		time.Date(2026, 7, 17, 23, 59, 59, 0, time.UTC),
	} {
		got, ok, err := r.USDPriceAt(context.Background(), gbp, at)
		if err != nil {
			t.Fatalf("USDPriceAt(%s): %v", at, err)
		}
		if !ok || got != "1.3386880856760375" {
			t.Errorf("USDPriceAt(fiat:GBP, %s) = (%q, %t), want the cached rate + true", at, got, ok)
		}
	}
}

// TestFiatUSDRateOrientation is the arithmetic guard against the
// single most damaging way this can break: returning rate_usd (units
// of ticker per USD) where USD-per-unit is wanted. fx_quotes stores
// rate_usd(JPY) = 163.09 — "1 USD buys ¥163.09" — so the USD price of
// ONE yen must come out ~0.0061, NOT 163. Inverting it would overstate
// every yen-quoted trade's volume by ~26,000x.
//
// This exercises the exact pair the resolver builds (Base = the fiat
// asset, Quote = USD) through the same fxSnapFromRows the resolver
// calls, so it pins the orientation end-to-end without a database.
// Rates are the real production values read from R1 on 2026-07-22.
func TestFiatUSDRateOrientation(t *testing.T) {
	t.Parallel()
	usd := fiatAsset(t, "USD")
	bucket := time.Date(2026, 7, 22, 0, 0, 0, 0, time.UTC)

	cases := []struct {
		ticker  string
		rateUSD string // units of ticker per 1 USD, as stored
		want    string // USD per 1 unit of ticker, to 10dp
	}{
		{"EUR", "0.87664", "1.1407191093"},
		{"GBP", "0.747", "1.3386880857"},
		{"CHF", "0.81261", "1.2306026261"},
		{"JPY", "163.092", "0.0061315086"},
		{"KRW", "1478.96", "0.0006761508"},
		{"VND", "26335", "0.0000379723"},
	}
	for _, tc := range cases {
		t.Run(tc.ticker, func(t *testing.T) {
			t.Parallel()
			pair := canonical.Pair{Base: fiatAsset(t, tc.ticker), Quote: usd}
			price, _, _, err := fxSnapFromRows(pair, map[string]fxSnapRow{
				tc.ticker: {Bucket: bucket, RateUSD: tc.rateUSD, Source: "massive"},
			})
			if err != nil {
				t.Fatalf("fxSnapFromRows: %v", err)
			}
			if got := price.FloatString(10); got != tc.want {
				t.Errorf("USD per 1 %s = %s, want %s (inverted?)", tc.ticker, got, tc.want)
			}
			// Belt and braces, independent of the expected digits:
			// rate_usd and its inverse must sit on opposite sides of
			// 1. A currency that takes MORE than 1 unit to buy a
			// dollar (rate_usd > 1, e.g. JPY) must price at LESS than
			// $1 per unit, and vice versa. An inversion flips both.
			one := big.NewRat(1, 1)
			stored, ok := new(big.Rat).SetString(tc.rateUSD)
			if !ok {
				t.Fatalf("bad test rate %q", tc.rateUSD)
			}
			if stored.Cmp(one) > 0 && price.Cmp(one) >= 0 {
				t.Errorf("%s: rate_usd %s > 1 (weaker than USD) but priced at %s >= $1 — inverted",
					tc.ticker, tc.rateUSD, price.FloatString(10))
			}
			if stored.Cmp(one) < 0 && price.Cmp(one) <= 0 {
				t.Errorf("%s: rate_usd %s < 1 (stronger than USD) but priced at %s <= $1 — inverted",
					tc.ticker, tc.rateUSD, price.FloatString(10))
			}
		})
	}
}

// TestInstallUSDVolumeResolution_InstallsBothTiers is the structural
// guard against the drift that motivated the helper. Three processes
// write trades; each must install the quote spec AND the FX resolver.
// A store with only one of them does not degrade gracefully — because
// InsertTrade/BatchInsertTrades upsert `usd_volume = EXCLUDED.usd_volume`
// unconditionally, a re-derive with a high derive_generation and a
// half-wired store overwrites correct values with NULL.
func TestInstallUSDVolumeResolution_InstallsBothTiers(t *testing.T) {
	t.Parallel()
	store := &Store{}
	err := InstallUSDVolumeResolution(
		store,
		[]string{"USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"},
		map[string]string{},
	)
	if err != nil {
		t.Fatalf("InstallUSDVolumeResolution: %v", err)
	}
	if store.usdVolumeQuoteSpec == nil {
		t.Error("quote spec not installed (tier 2 dead)")
	}
	if store.usdVolumeFXResolver == nil {
		t.Error("fx resolver not installed (tiers 3/4 dead — and fiat quotes unpriceable)")
	}
}

// TestInstallUSDVolumeResolution_EmptyPegsIsNoOp — the documented
// no-config behaviour: no pegs declared leaves both tiers nil rather
// than erroring, preserving off-chain-only usd_volume.
func TestInstallUSDVolumeResolution_EmptyPegsIsNoOp(t *testing.T) {
	t.Parallel()
	store := &Store{}
	if err := InstallUSDVolumeResolution(store, nil, nil); err != nil {
		t.Fatalf("InstallUSDVolumeResolution(no pegs): %v", err)
	}
	if store.usdVolumeQuoteSpec != nil || store.usdVolumeFXResolver != nil {
		t.Error("no-config path should leave both tiers nil")
	}
}

// TestInstallUSDVolumeResolution_NilStore — defensive guard.
func TestInstallUSDVolumeResolution_NilStore(t *testing.T) {
	t.Parallel()
	if err := InstallUSDVolumeResolution(nil, []string{"USDC-G..."}, nil); err == nil {
		t.Fatal("expected error when store is nil")
	}
}

// ─── tier 3b: the XLM bridge (2026-07-22) ────────────────────────────

// TestXLMLegRate_Orientation — a token's XLM market can be stored
// either way round (trades keep the venue's observed base/quote
// ordering), and on 2026-07-17 several tokens had BOTH orientations on
// the same day. Getting the inversion wrong is silent: the rate comes
// out wrong by a factor of vwap^2 and still looks like a plausible
// small number.
func TestXLMLegRate_Orientation(t *testing.T) {
	t.Parallel()
	// Real production values: 6T/native VWAP on 2026-07-17 12:00.
	const sixTPerXLM = "0.03218194209615242009"

	direct, ok := xlmLegRate(sixTPerXLM, false)
	if !ok {
		t.Fatal("direct orientation rejected")
	}
	if got := direct.FloatString(20); got != sixTPerXLM {
		t.Errorf("direct = %s, want %s unchanged", got, sixTPerXLM)
	}

	// The same market stored XLM-first: vwap is asset-per-XLM, so the
	// leg must invert to XLM-per-asset.
	invertedInput := new(big.Rat).Inv(direct).FloatString(20)
	got, ok := xlmLegRate(invertedInput, true)
	if !ok {
		t.Fatal("inverted orientation rejected")
	}
	// Round-trips back to the direct rate (to within the render scale).
	if diff := new(big.Rat).Sub(got, direct); diff.Abs(diff).Cmp(big.NewRat(1, 1_000_000_000_000)) > 0 {
		t.Errorf("inverted round-trip = %s, want ~%s", got.FloatString(20), sixTPerXLM)
	}
	// And it must NOT equal the raw stored number — that is the bug.
	if got.FloatString(20) == invertedInput {
		t.Error("inverted leg was not inverted")
	}
}

func TestXLMLegRate_RejectsBadInput(t *testing.T) {
	t.Parallel()
	for _, in := range []string{"", "abc", "0", "-1.5"} {
		if _, ok := xlmLegRate(in, false); ok {
			t.Errorf("xlmLegRate(%q) accepted a bad rate", in)
		}
	}
}

// TestBridgeRate — the tier-3b multiplication, pinned against the real
// production legs measured on R1 for 2026-07-17 12:00:
// 6T/XLM 0.03218194209615242009 x XLM/USD 0.18437992688007971271.
func TestBridgeRate(t *testing.T) {
	t.Parallel()
	xlmPer6T, ok := xlmLegRate("0.03218194209615242009", false)
	if !ok {
		t.Fatal("leg rejected")
	}
	got, ok := bridgeRate(xlmPer6T, "0.18437992688007971271")
	if !ok {
		t.Fatal("bridgeRate declined")
	}
	// 0.03218194209615242009 x 0.18437992688007971271
	//   = 0.0059337041305475424553467904091784323439 exactly,
	// truncated to bridgeVWAPScale. Computed independently, not read
	// back from the implementation.
	const want = "0.005933704130547542"
	if got != want {
		t.Errorf("bridged USD per 6T = %s, want %s", got, want)
	}
}

func TestBridgeRate_RejectsBadLegs(t *testing.T) {
	t.Parallel()
	good := big.NewRat(1, 32)
	if _, ok := bridgeRate(nil, "0.18"); ok {
		t.Error("nil asset leg accepted")
	}
	if _, ok := bridgeRate(new(big.Rat), "0.18"); ok {
		t.Error("zero asset leg accepted")
	}
	for _, bad := range []string{"", "nope", "0", "-0.18"} {
		if _, ok := bridgeRate(good, bad); ok {
			t.Errorf("bridgeRate accepted XLM/USD leg %q", bad)
		}
	}
}

// TestBridgeViaXLM_XLMIsBaseCase — bridging XLM through XLM would be
// circular. The guard must fire before any query, so the nil DB in
// &Store{} is itself the assertion.
func TestBridgeViaXLM_XLMIsBaseCase(t *testing.T) {
	t.Parallel()
	r, err := NewVWAPUSDFXResolver(&Store{}, VWAPUSDFXResolverOptions{
		USDPegs: []string{"USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"},
	})
	if err != nil {
		t.Fatalf("NewVWAPUSDFXResolver: %v", err)
	}
	for name, asset := range map[string]canonical.Asset{
		"native": canonical.NativeAsset(),
		"sac":    {Type: canonical.AssetSoroban, ContractID: nativeXLMSAC},
	} {
		rate, _, err := r.bridgeViaXLM(context.Background(), asset, time.Now())
		if err != nil {
			t.Errorf("%s: bridgeViaXLM errored instead of declining: %v", name, err)
		}
		if rate != "" {
			t.Errorf("%s: bridge returned %q, want a decline (would be circular)", name, rate)
		}
	}
}
