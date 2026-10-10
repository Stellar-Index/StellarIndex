package timescale

import (
	"context"
	"database/sql/driver"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// With no USDPegs the resolver is a no-op: ok=false without touching the DB.
func TestVWAPUSDFXResolver_NoPegs(t *testing.T) {
	r, err := NewVWAPUSDFXResolver(&Store{}, VWAPUSDFXResolverOptions{})
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

func TestVWAPUSDFXResolver_NilStore(t *testing.T) {
	_, err := NewVWAPUSDFXResolver(nil, VWAPUSDFXResolverOptions{})
	if err == nil {
		t.Fatal("expected error when store is nil")
	}
	if !strings.Contains(err.Error(), "store is required") {
		t.Errorf("error = %v, want the nil-store guard's error", err)
	}
}

func TestVWAPUSDFXResolver_DefaultsApplied(t *testing.T) {
	r, err := NewVWAPUSDFXResolver(&Store{}, VWAPUSDFXResolverOptions{
		USDPegs: []string{usdcPeg},
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

const usdcPeg = "USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"

// Cache hits (positive and negative) are served within the TTL without touching
// the DB; the nil DB in &Store{} would panic on a query.
func TestVWAPUSDFXResolver_CacheHits(t *testing.T) {
	now := time.Date(2026, 5, 12, 14, 30, 0, 0, time.UTC)
	for _, tc := range []struct {
		name, code, rate string
		wantOK           bool
	}{
		{"positive", "EURC", "1.0850", true},
		{"negative", "MXNe", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, err := NewVWAPUSDFXResolver(&Store{}, VWAPUSDFXResolverOptions{
				USDPegs: []string{usdcPeg},
				Clock:   func() time.Time { return now },
			})
			if err != nil {
				t.Fatalf("NewVWAPUSDFXResolver: %v", err)
			}
			a, _ := canonical.NewClassicAsset(tc.code, "GDHU6WRG4IEQXM5NZ4BMPKOXHW76MZM4Y2IEMFDVXBSDP6SJY4ITNPP2")
			// Minute-floored key: trades in the same minute share one entry.
			r.cache[fxCacheKey{asset: a.String(), bucketMs: now.Truncate(time.Minute).UnixMilli()}] = fxCacheEntry{rate: tc.rate, cachedAt: now}

			got, ok, err := r.USDPriceAt(context.Background(), a, now)
			if err != nil {
				t.Fatalf("USDPriceAt: %v", err)
			}
			if ok != tc.wantOK || got != tc.rate {
				t.Errorf("USDPriceAt = (%q, %t), want (%q, %t)", got, ok, tc.rate, tc.wantOK)
			}
		})
	}
}

// An entry older than CacheTTL is a miss.
func TestVWAPUSDFXResolver_CacheTTLExpiry(t *testing.T) {
	now := time.Date(2026, 5, 12, 14, 30, 0, 0, time.UTC)
	r, err := NewVWAPUSDFXResolver(&Store{}, VWAPUSDFXResolverOptions{
		USDPegs:  []string{usdcPeg},
		CacheTTL: 5 * time.Minute,
		Clock:    func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("NewVWAPUSDFXResolver: %v", err)
	}
	asset, _ := canonical.NewClassicAsset("USDX", "GDHU6WRG4IEQXM5NZ4BMPKOXHW76MZM4Y2IEMFDVXBSDP6SJY4ITNPP2")
	key := fxCacheKey{asset: asset.String(), bucketMs: now.Truncate(time.Minute).UnixMilli()}
	r.cache[key] = fxCacheEntry{rate: "0.95", cachedAt: now.Add(-10 * time.Minute)}

	if got, isCached := r.lookupCache(key); isCached {
		t.Errorf("expired entry should miss; got rate=%q", got)
	}
}

// NUMERIC::text keeps the column scale ("1.085000..."); the resolver trims it.
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

// Freshness: 0 defaults to 1h, negative disables, positive is used as-is.
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

// fiatAsset is a test helper for the fx-quote-resolved fiat side.
func fiatAsset(t *testing.T, code string) canonical.Asset {
	t.Helper()
	a, err := canonical.NewFiatAsset(code)
	if err != nil {
		t.Fatalf("NewFiatAsset(%q): %v", code, err)
	}
	return a
}

// USD is the rate_usd anchor: exactly 1, no DB access (a query on the nil DB would panic).
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

// The fiat branch must run before the empty-pegs early return, or every fiat
// rate is ok=false (NULL usd_volume) on a deployment with no pegs.
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

// fx_quotes holds one row per (date, ticker) at UTC midnight, so the fiat cache
// key floors to the UTC day: every trade that day shares one entry.
func TestVWAPUSDFXResolver_FiatCacheKeyIsUTCDay(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 17, 0, 0, 0, 0, time.UTC)
	r, err := NewVWAPUSDFXResolver(&Store{}, VWAPUSDFXResolverOptions{
		Clock:    func() time.Time { return now },
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

// rate_usd is units of ticker per USD (JPY 163.09); the USD price of one yen
// must be ~0.0061, not 163 (an inversion overstates volume ~26,000x). Rates are
// production values.
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
			// rate_usd and its price sit on opposite sides of 1; an inversion flips both.
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

// Every trade writer must install both the quote spec and the FX resolver: a
// half-wired store's unconditional usd_volume upsert would overwrite correct values with NULL.
func TestInstallUSDVolumeResolution_InstallsBothTiers(t *testing.T) {
	t.Parallel()
	store := &Store{}
	err := InstallUSDVolumeResolution(
		store,
		[]string{usdcPeg},
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

// No pegs declared leaves both tiers nil (off-chain-only usd_volume).
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

func TestInstallUSDVolumeResolution_NilStore(t *testing.T) {
	t.Parallel()
	if err := InstallUSDVolumeResolution(nil, []string{"USDC-G..."}, nil); err == nil {
		t.Fatal("expected error when store is nil")
	}
}

// A token's XLM market can be stored either way round; a wrong inversion is
// silent (off by vwap^2). Values are production.
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

// A drifting stroop-to-unit divisor moves the tier-3a dust floor by a power of ten.
func TestPegQuoteScaleDenominator(t *testing.T) {
	t.Parallel()
	want := scaleDenominator(stellarClassicDecimals)
	if got := big.NewInt(pegQuoteScaleDenominator); got.Cmp(want) != 0 {
		t.Errorf("pegQuoteScaleDenominator = %s, want 10^%d = %s",
			got, stellarClassicDecimals, want)
	}
}

// Tier-3a and 3b dust floors must agree on where dust starts ($0.01, as the OHLC extremes).
func TestDirectLegMinQuoteVolume_MatchesBridgeFloor(t *testing.T) {
	t.Parallel()
	if directLegMinQuoteVolume != bridgeLegMinUSDVolume {
		t.Errorf("directLegMinQuoteVolume = %q, bridgeLegMinUSDVolume = %q — the two dust floors must agree",
			directLegMinQuoteVolume, bridgeLegMinUSDVolume)
	}
}

// Tier-3b multiplication on production legs: 6T/XLM 0.03218194209615242009 x
// XLM/USD 0.18437992688007971271.
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
	// Exact product 0.0059337041305475424553467904091784323439, rendered at 28 places
	// (3 leading zeros + 25 significant digits); computed independently.
	const want = "0.0059337041305475424553467904"
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

// Bridging XLM through XLM is circular: declined before any query (nil DB asserts it).
func TestBridgeViaXLM_XLMIsBaseCase(t *testing.T) {
	t.Parallel()
	r, err := NewVWAPUSDFXResolver(&Store{}, VWAPUSDFXResolverOptions{
		USDPegs: []string{usdcPeg},
	})
	if err != nil {
		t.Fatalf("NewVWAPUSDFXResolver: %v", err)
	}
	for name, asset := range map[string]canonical.Asset{
		"native": canonical.NativeAsset(),
		"sac":    {Type: canonical.AssetSoroban, ContractID: canonical.XLMSacContractID},
	} {
		rate, err := r.bridgeViaXLM(context.Background(), asset, time.Now())
		if err != nil {
			t.Errorf("%s: bridgeViaXLM errored instead of declining: %v", name, err)
		}
		if rate != "" {
			t.Errorf("%s: bridge returned %q, want a decline (would be circular)", name, rate)
		}
	}
}

// Fresh entries above the sweep threshold are not rescanned on every insert.
func TestStoreCache_SweepIsAmortised(t *testing.T) {
	t.Parallel()
	now := time.Unix(1_700_000_000, 0)
	clockReads := 0
	r, err := NewVWAPUSDFXResolver(&Store{}, VWAPUSDFXResolverOptions{
		Clock: func() time.Time { clockReads++; return now },
	})
	if err != nil {
		t.Fatalf("NewVWAPUSDFXResolver: %v", err)
	}
	const inserts = 3 * fxCacheSweepThreshold
	for i := range inserts {
		r.storeCache(fxCacheKey{asset: "A", bucketMs: int64(i)}, fxCacheEntry{cachedAt: now})
	}
	if got := len(r.cache); got != inserts {
		t.Fatalf("cache size = %d, want %d (every entry is fresh)", got, inserts)
	}
	if clockReads > 2 {
		t.Errorf("storeCache swept %d times over %d fresh inserts, want at most 2", clockReads, inserts)
	}
}

// Entries aged past the TTL are dropped by the next sweep.
func TestStoreCache_SweepEvictsExpired(t *testing.T) {
	t.Parallel()
	now := time.Unix(1_700_000_000, 0)
	r, err := NewVWAPUSDFXResolver(&Store{}, VWAPUSDFXResolverOptions{
		Clock: func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("NewVWAPUSDFXResolver: %v", err)
	}
	for i := range 3 * fxCacheSweepThreshold {
		r.storeCache(fxCacheKey{asset: "A", bucketMs: int64(i)}, fxCacheEntry{cachedAt: now})
	}
	now = now.Add(r.cacheTTL + time.Second)
	for i := range 3 * fxCacheSweepThreshold {
		r.storeCache(fxCacheKey{asset: "B", bucketMs: int64(i)}, fxCacheEntry{cachedAt: now})
	}
	for k := range r.cache {
		if k.asset == "A" {
			t.Fatal("expired entries survived a full refill; the sweep never ran")
		}
	}
}

// The single declared USD peg on r1, its SAC wrapper, and one wrapper for
// a classic that is NOT a peg — so a test can tell "expanded the peg set"
// from "swept in every wrapper the operator declared".
var (
	usdcClassicPeg = "USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
	usdcSAC        = "CCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUZPUTHXSTZLEO7SJMI75"
	aquaSAC        = "CAUIKL3IYGMERDRUN6YSCLWVAKIFG5Q4YJHUKM4S4NJZQIA3BAS6OJPK"

	pegSACWrappersFixture = map[string]string{
		usdcSAC: "USDC:GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN",
		aquaSAC: "AQUA:GBNZILSTVQZ4R7IKQDGHYGY2QXL5QOFJYQMXPKWRRM5PAV7Y4M67AQUA",
	}
)

// TestVWAPUSDFXResolver_XLMAnchorResolvesThroughItsSorobanForms is the
// alias regression: the tier-3/4 XLM/USD anchor was bound to ONE
// spelling of XLM (`native`) against ONE spelling of the peg (the
// classic), and XLM's USD markets are split across its three canonical
// identities BY VENUE. Measured on r1, prices_1m buckets
// clearing this query's own dust floor:
//
//	native      / USDC-GA5Z… (classic)  215,790 buckets, recent
//	CAS3J7…SAC  / CCW67T…SAC (the SAC)  291,883 buckets, since 2024
//	every other combination of those forms                0
//
// so a single-spelling predicate could not price ANY on-chain XLM trade before
// the alias fix — 28,534 sdex/AMM XLM-base rows in one month, moving
// 19,174,885 XLM, all stored with usd_volume NULL and served as $0.00.
//
// The scripted driver replays that exact shape: `native` misses,
// `crypto:XLM` misses, the SAC form hits. The assertion is the RESOLVED
// RATE, not merely that something came back.
func TestVWAPUSDFXResolver_XLMAnchorResolvesThroughItsSorobanForms(t *testing.T) {
	t.Parallel()
	at := time.Date(2025, 6, 15, 12, 0, 0, 0, time.UTC)
	bucket := at.Add(-3 * time.Minute)
	// A real XLM/USDC VWAP shape: NUMERIC::text arrives fully scaled.
	const storedVWAP = "0.24170000000000000000"
	const wantRate = "0.2417"

	store, conn := newScriptedStore(t,
		scriptedResult{cols: []string{"bucket", "vwap"}}, // native     → miss
		scriptedResult{cols: []string{"bucket", "vwap"}}, // crypto:XLM → miss
		scriptedResult{ // the SAC form → hit
			cols: []string{"bucket", "vwap"},
			rows: [][]driver.Value{{bucket, storedVWAP}},
		},
	)
	r, err := NewVWAPUSDFXResolver(store, VWAPUSDFXResolverOptions{
		USDPegs:        []string{usdcClassicPeg},
		PegSACWrappers: pegSACWrappersFixture,
		Clock:          func() time.Time { return at },
	})
	if err != nil {
		t.Fatalf("NewVWAPUSDFXResolver: %v", err)
	}

	got, ok, err := r.USDPriceAt(context.Background(), canonical.NativeAsset(), at)
	if err != nil {
		t.Fatalf("USDPriceAt: %v", err)
	}
	if !ok {
		t.Fatal("XLM/USD anchor declined: the Soroban XLM book was never asked for " +
			"(pre-#372-F4 behaviour — every 2024/2025 on-chain XLM trade reads $0.00)")
	}
	if got != wantRate {
		t.Errorf("USDPriceAt(native) = %q, want %q", got, wantRate)
	}

	// The base side must be tried in canonical PRIORITY order, SAC last:
	// a set-shaped `= ANY(forms)` would let the thin Soroban pool outrank
	// the deep SDEX book whenever it printed more recently.
	wantBases := []string{"native", "crypto:XLM", canonical.XLMSacContractID}
	if len(conn.stmts) != len(wantBases) {
		t.Fatalf("issued %d direct-leg statements, want %d (one per XLM form)", len(conn.stmts), len(wantBases))
	}
	for i, want := range wantBases {
		if got := conn.stmts[i].arg(t, 1); got != want {
			t.Errorf("statement %d bound base_asset %v, want %q", i, got, want)
		}
	}

	// And the peg side must carry the SAC wrapper of the declared peg —
	// without it the SAC-base query matches nothing, since the Soroban
	// book quotes in the USDC SAC, not in classic USDC.
	pegArg := conn.stmts[len(conn.stmts)-1].arg(t, 2)
	pegs, isSlice := pegArg.([]string)
	if !isSlice {
		t.Fatalf("peg arg is %T, want []string", pegArg)
	}
	if len(pegs) != 2 || pegs[0] != usdcClassicPeg || pegs[1] != usdcSAC {
		t.Errorf("bound peg forms = %q, want [%q %q]", pegs, usdcClassicPeg, usdcSAC)
	}
}

// TestVWAPUSDFXResolver_AliasLoopStopsAtTheEstablishedForm pins the other
// half of the alias contract: the loop is ADDITIVE. Where the
// pre-existing `native` form already answers — every date after
// the alias fix — it answers FIRST, no further form is consulted, and the result is
// what the single-spelling code returned. This is the manipulation guard
// [canonical.AssetAliases] documents (SAC form LAST) expressed as
// behaviour: the fresher, 99.00 Soroban print must not win.
func TestVWAPUSDFXResolver_AliasLoopStopsAtTheEstablishedForm(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 8, 15, 9, 30, 0, 0, time.UTC)
	bucket := at.Add(-1 * time.Minute)

	store, conn := newScriptedStore(t,
		scriptedResult{cols: []string{"bucket", "vwap"}, rows: [][]driver.Value{{bucket, "0.31500000"}}},
		// A second scripted row a later form would consume if the loop
		// wrongly kept going / wrongly preferred the fresher bucket.
		scriptedResult{cols: []string{"bucket", "vwap"}, rows: [][]driver.Value{{at, "99.00000000"}}},
	)
	r, err := NewVWAPUSDFXResolver(store, VWAPUSDFXResolverOptions{
		USDPegs:        []string{usdcClassicPeg},
		PegSACWrappers: pegSACWrappersFixture,
		Clock:          func() time.Time { return at },
	})
	if err != nil {
		t.Fatalf("NewVWAPUSDFXResolver: %v", err)
	}

	got, ok, err := r.USDPriceAt(context.Background(), canonical.NativeAsset(), at)
	if err != nil || !ok {
		t.Fatalf("USDPriceAt = (%q, %t, %v), want a rate", got, ok, err)
	}
	if got != "0.315" {
		t.Errorf("USDPriceAt(native) = %q, want 0.315 (the established SDEX form)", got)
	}
	if len(conn.stmts) != 1 {
		t.Fatalf("issued %d statements, want exactly 1 — the loop must stop at the first hit", len(conn.stmts))
	}
	if got := conn.stmts[0].arg(t, 1); got != "native" {
		t.Errorf("bound base_asset %v, want \"native\"", got)
	}
}

// TestUSDPegForms_MatchesTheExactTierPegSet is the lockstep guard between
// the package's two spellings of "is this quote a declared dollar":
// [USDVolumeQuoteSpec.QuoteUSDPegInfo] (tiers 1/2) and [usdPegForms]
// (tiers 3/4). They drifted — the exact tier let a SAC INHERIT its
// classic's peg while the FX tier bound the classic strings alone — and
// that drift is half of what made the XLM anchor blind to Soroban.
func TestUSDPegForms_MatchesTheExactTierPegSet(t *testing.T) {
	t.Parallel()
	pegs := []string{usdcClassicPeg}
	forms, err := usdPegForms(pegs, pegSACWrappersFixture)
	if err != nil {
		t.Fatalf("usdPegForms: %v", err)
	}
	want := []string{usdcClassicPeg, usdcSAC}
	if len(forms) != len(want) {
		t.Fatalf("usdPegForms = %q, want %q", forms, want)
	}
	for i := range want {
		if forms[i] != want[i] {
			t.Fatalf("usdPegForms = %q, want %q", forms, want)
		}
	}

	spec, err := NewUSDVolumeQuoteSpec(pegs, pegSACWrappersFixture)
	if err != nil {
		t.Fatalf("NewUSDVolumeQuoteSpec: %v", err)
	}
	for _, form := range forms {
		asset, err := canonical.ParseAsset(form)
		if err != nil {
			t.Fatalf("ParseAsset(%q): %v", form, err)
		}
		if _, pegged := spec.QuoteUSDPegInfo(asset); !pegged {
			t.Errorf("usdPegForms bound %q, which the exact tier does NOT treat as a peg", form)
		}
	}
	// The converse: a declared wrapper whose classic is not a peg must
	// stay out. AQUA's SAC is in the fixture precisely to catch a fix
	// that widened the set to "every sac_wrapper".
	for _, form := range forms {
		if form == aquaSAC {
			t.Error("usdPegForms admitted a non-peg classic's SAC wrapper")
		}
	}
}

// TestUSDPegForms_RejectsMalformedWrapper — strict on the WRAPPER, like
// [NewUSDVolumeQuoteSpec]: a wrapper we cannot parse is a peg form we
// would silently never bind, i.e. invisible under-counted volume.
//
// The declared peg strings themselves are deliberately NOT re-validated:
// they are bound verbatim into the SQL exactly as they were before this
// expansion existed, and [NewUSDVolumeQuoteSpec] already rejects an
// unparseable one on the production wiring path.
func TestUSDPegForms_RejectsMalformedWrapper(t *testing.T) {
	t.Parallel()
	if _, err := usdPegForms([]string{usdcClassicPeg}, map[string]string{usdcSAC: "not-an-asset"}); err == nil {
		t.Fatal("expected an error for an unparseable sac_wrapper target")
	}
	forms, err := usdPegForms([]string{"USDC-G..."}, pegSACWrappersFixture)
	if err != nil {
		t.Fatalf("an unrecognised peg spelling must pass through, not error: %v", err)
	}
	if len(forms) != 1 || forms[0] != "USDC-G..." {
		t.Errorf("usdPegForms = %q, want the peg bound verbatim with no expansion", forms)
	}
}

// TestUSDPegForms_NormalisesColonSpelling — config and the quote spec both
// accept "CODE:ISSUER"; the FX tier must bind the "CODE-ISSUER" form
// prices_1m stores, and still expand the peg's SAC wrapper.
func TestUSDPegForms_NormalisesColonSpelling(t *testing.T) {
	t.Parallel()
	const colon = "USDC:GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
	forms, err := usdPegForms([]string{colon}, pegSACWrappersFixture)
	if err != nil {
		t.Fatalf("usdPegForms: %v", err)
	}
	if len(forms) != 2 || forms[0] != usdcClassicPeg || forms[1] != usdcSAC {
		t.Errorf("usdPegForms(%q) = %q, want [%s %s]", colon, forms, usdcClassicPeg, usdcSAC)
	}
}
