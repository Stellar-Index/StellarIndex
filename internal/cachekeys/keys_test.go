package cachekeys_test

import (
	"strings"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/cachekeys"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// usdcIssuer is the Circle USDC issuer — reused as a realistic G-address
// fixture across tests.
const usdcIssuer = "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"

// Golden-string tests: every wire string asserted below is
// byte-identical to what the pre-typed-key `string`-returning
// builders produced (captured before the ADR-0007 typed-key
// migration — ROADMAP #48). The migration is a compile-time-only
// hardening: it must NOT invalidate a single live Redis key on
// deploy. Every assertion calls [*.String] to cross into the raw
// wire string, mirroring what a real call site does at the
// redis.Cmdable boundary.

func TestPriceKey(t *testing.T) {
	xlm := canonical.NativeAsset()
	k := cachekeys.Price(xlm)
	if k.String() != "price:native" {
		t.Errorf("Price(XLM) = %q, want 'price:native'", k.String())
	}

	usdc, _ := canonical.NewClassicAsset("USDC", usdcIssuer)
	k2 := cachekeys.Price(usdc)
	if !strings.HasPrefix(k2.String(), "price:USDC-") {
		t.Errorf("Price(USDC) = %q, want prefix 'price:USDC-'", k2.String())
	}

	// TTL pinned to ADR-0007 (60 s). Mirrors the assertion style used
	// by the other key classes so a drift in either direction is
	// caught by the test suite.
	if cachekeys.PriceTTL != 60*time.Second {
		t.Errorf("PriceTTL = %v, want 60s (ADR-0007)", cachekeys.PriceTTL)
	}
}

func TestVWAP(t *testing.T) {
	xlm := canonical.NativeAsset()
	usdc, _ := canonical.NewClassicAsset("USDC", usdcIssuer)

	k := cachekeys.VWAP(xlm, usdc, 5*time.Minute)
	// Format: vwap:<base>:<quote>:<window-seconds>
	expected := "vwap:native:USDC-" + usdcIssuer + ":300"
	if k.String() != expected {
		t.Errorf("VWAP = %q, want %q", k.String(), expected)
	}

	if ttl := cachekeys.VWAPTTL(5 * time.Minute); ttl != 5*time.Minute {
		t.Errorf("VWAPTTL = %v", ttl)
	}
	if ttl := cachekeys.VWAPTTL(0); ttl != 0 {
		t.Errorf("VWAPTTL(0) = %v, want 0", ttl)
	}
}

func TestOHLC(t *testing.T) {
	xlm := canonical.NativeAsset()
	usdc, _ := canonical.NewClassicAsset("USDC", usdcIssuer)
	bucket := time.Unix(1_745_000_000, 0).UTC()

	k := cachekeys.OHLC(xlm, usdc, "15m", bucket)
	// Expected: ohlc:native:USDC-...:15m:1745000000
	if !strings.HasPrefix(k.String(), "ohlc:native:USDC-") {
		t.Errorf("OHLC key malformed: %q", k.String())
	}
	if !strings.HasSuffix(k.String(), ":15m:1745000000") {
		t.Errorf("OHLC key does not end with granularity:bucket: %q", k.String())
	}

	// Open-candle TTL is a safety-net upper bound matching ADR-0007;
	// closed is zero (immutable — CDN-pinned).
	if cachekeys.OHLCOpenTTL != time.Hour {
		t.Errorf("OHLCOpenTTL = %v, want 1h (ADR-0007)", cachekeys.OHLCOpenTTL)
	}
	if cachekeys.OHLCClosedTTL != 0 {
		t.Errorf("OHLCClosedTTL should be 0 (immutable), got %v", cachekeys.OHLCClosedTTL)
	}
}

func TestRateLimitKey(t *testing.T) {
	now := time.Unix(1_750_000_000, 0).UTC()
	k := cachekeys.RateLimitKey("rek_abc", now, time.Minute)
	// minute bucket = 1_750_000_000 / 60 = 29166666
	if k.String() != "rl:rek_abc:29166666" {
		t.Errorf("RateLimitKey = %q, want 'rl:rek_abc:29166666'", k.String())
	}

	// TTL is 2× window per ADR-0007.
	if ttl := cachekeys.RateLimitTTL(time.Minute); ttl != 2*time.Minute {
		t.Errorf("RateLimitTTL = %v, want 2m", ttl)
	}
}

func TestRateLimitKey_MatchesRatelimitPackagePrefix(t *testing.T) {
	// Consistency check: internal/ratelimit builds "rl:<escape(key)>:<bucket>"
	// directly; this package mirrors that shape. If someone changes
	// either side, this test highlights the drift.
	now := time.Unix(1_750_000_000, 0).UTC()
	k := cachekeys.RateLimitKey("x", now, time.Minute)
	if !strings.HasPrefix(k.String(), "rl:") {
		t.Errorf("RateLimitKey must use rl: prefix, got %q", k.String())
	}
}

func TestRateLimitKey_EscapesSubjectForParityWithBucket(t *testing.T) {
	// Subjects containing `:` (e.g. IPv6 addresses) are url.QueryEscape'd
	// by internal/ratelimit/bucket.go's Take() to prevent
	// cross-subject collisions on the Redis slot. This mirror
	// function MUST escape identically or the two sides produce
	// different keys for the same subject.
	now := time.Unix(1_750_000_000, 0).UTC()
	k := cachekeys.RateLimitKey("2001:db8::1", now, time.Minute)
	if !strings.HasPrefix(k.String(), "rl:2001%3Adb8%3A%3A1:") {
		t.Errorf("RateLimitKey did not escape `:` in IPv6 subject: got %q", k.String())
	}
}

func TestTOML(t *testing.T) {
	// Lowercasing is intentional — domain names are case-insensitive.
	if k := cachekeys.TOML("Circle.com"); k.String() != "toml:circle.com" {
		t.Errorf("TOML(Circle.com) = %q", k.String())
	}
	if k := cachekeys.TOML("lobstr.co"); k.String() != "toml:lobstr.co" {
		t.Errorf("TOML(lobstr.co) = %q", k.String())
	}
	// 24h — stellar.toml is slow-changing issuer reference data; a
	// short TTL just makes cold /v1/assets/{id} pay a fresh ~500ms
	// upstream fetch (#63).
	if cachekeys.TOMLTTL != 24*time.Hour {
		t.Errorf("TOMLTTL = %v", cachekeys.TOMLTTL)
	}
}

func TestMetadata(t *testing.T) {
	xlm := canonical.NativeAsset()
	if k := cachekeys.Metadata(xlm); k.String() != "meta:native" {
		t.Errorf("Metadata(XLM) = %q", k.String())
	}
	if cachekeys.MetadataTTL != 5*time.Minute {
		t.Errorf("MetadataTTL = %v", cachekeys.MetadataTTL)
	}
}

func TestSubscriber(t *testing.T) {
	k := cachekeys.Subscriber("price:XLM", "conn-42")
	if k.String() != "sub:price:XLM:conn-42" {
		t.Errorf("Subscriber = %q", k.String())
	}
	if cachekeys.SubscriberTTL != 60*time.Second {
		t.Errorf("SubscriberTTL = %v", cachekeys.SubscriberTTL)
	}
}

func TestDivergence(t *testing.T) {
	xlm := canonical.NativeAsset()
	usd, err := canonical.ParseAsset("fiat:USD")
	if err != nil {
		t.Fatalf("ParseAsset: %v", err)
	}
	pair, err := canonical.NewPair(xlm, usd)
	if err != nil {
		t.Fatalf("NewPair: %v", err)
	}
	if k := cachekeys.Divergence(pair); k.String() != "div:native/fiat:USD" {
		t.Errorf("Divergence(XLM/USD) = %q", k.String())
	}
	if k := cachekeys.DivergenceBaseIndex(xlm); k.String() != "div:idx:native" {
		t.Errorf("DivergenceBaseIndex(XLM) = %q", k.String())
	}
	if cachekeys.DivergenceTTL != 5*time.Minute {
		t.Errorf("DivergenceTTL = %v", cachekeys.DivergenceTTL)
	}
}

func TestHealth(t *testing.T) {
	if k := cachekeys.Health("soroswap"); k.String() != "health:soroswap" {
		t.Errorf("Health = %q", k.String())
	}
	if cachekeys.HealthTTL != 60*time.Second {
		t.Errorf("HealthTTL = %v", cachekeys.HealthTTL)
	}
}

// TestVWAPProvenance covers the cache key + the constant marker
// the triangulation worker stamps. The API reads the value via
// byte equality so the two must stay in lock-step — flipping
// either side without the other breaks `flags.triangulated`.
func TestVWAPProvenance(t *testing.T) {
	xlm := canonical.NativeAsset()
	usdc, err := canonical.NewClassicAsset("USDC", usdcIssuer)
	if err != nil {
		t.Fatalf("NewClassicAsset: %v", err)
	}

	got := cachekeys.VWAPProvenance(xlm, usdc, 5*time.Minute)
	want := "vwap:native:USDC-" + usdcIssuer + ":300:provenance"
	if got.String() != want {
		t.Errorf("VWAPProvenance = %q, want %q", got.String(), want)
	}
	// Sibling-key check: the provenance key MUST be the VWAP key
	// with `:provenance` suffixed. The aggregator writes both
	// atomically; a mismatched suffix would orphan the marker.
	//
	// [VWAPKey] and [VWAPProvenanceKey] are DISTINCT types — this
	// comparison necessarily goes through .String() on both sides;
	// `got != vwap` would not even compile (see
	// TestTypedKeysAreDistinctFamilies below), which is the point.
	vwap := cachekeys.VWAP(xlm, usdc, 5*time.Minute)
	if got.String() != vwap.String()+":provenance" {
		t.Errorf("VWAPProvenance %q is not VWAP %q + :provenance", got.String(), vwap.String())
	}
	if cachekeys.VWAPProvenanceTriangulated != "triangulated" {
		t.Errorf("marker = %q, want 'triangulated' (API matches by byte-equality)",
			cachekeys.VWAPProvenanceTriangulated)
	}
}

// TestConfidence pins the wire shape + ConfidenceTTL parity with
// VWAPTTL. The score is meaningless once the underlying VWAP
// expires, so the two TTLs must move together.
func TestConfidence(t *testing.T) {
	xlm := canonical.NativeAsset()
	usdc, err := canonical.NewClassicAsset("USDC", usdcIssuer)
	if err != nil {
		t.Fatalf("NewClassicAsset: %v", err)
	}

	got := cachekeys.Confidence(xlm, usdc, time.Hour)
	want := "confidence:native:USDC-" + usdcIssuer + ":3600"
	if got.String() != want {
		t.Errorf("Confidence = %q, want %q", got.String(), want)
	}
	if cachekeys.ConfidenceTTL(time.Hour) != cachekeys.VWAPTTL(time.Hour) {
		t.Errorf("ConfidenceTTL must equal VWAPTTL — score is tied to its underlying VWAP")
	}
}

// TestFreeze pins the marker key shape and the documented FreezeTTL
// of 5 minutes (long enough to span the next bucket-close, short
// enough to clear within a few buckets of recovery).
func TestFreeze(t *testing.T) {
	xlm := canonical.NativeAsset()
	usdc, err := canonical.NewClassicAsset("USDC", usdcIssuer)
	if err != nil {
		t.Fatalf("NewClassicAsset: %v", err)
	}

	got := cachekeys.Freeze(xlm, usdc)
	want := "freeze:native:USDC-" + usdcIssuer
	if got.String() != want {
		t.Errorf("Freeze = %q, want %q", got.String(), want)
	}
	if cachekeys.FreezeTTL != 5*time.Minute {
		t.Errorf("FreezeTTL = %v, want 5m (per cachekeys §Freeze design note)",
			cachekeys.FreezeTTL)
	}
}

// TestAPIKey pins the auth package's lookup contract: the key is
// `apikey:<sha256-hex>`, no TTL (revocation is encoded in the JSON
// payload, not at Redis).
func TestAPIKey(t *testing.T) {
	// Realistic-shape SHA-256 hex (64 chars).
	hash := "fc1908d72d0c4cdf3eaa45e8b3f2c2c4f6a3b7d29c4ad4e63b81a7e9be2c1cea"
	got := cachekeys.APIKey(hash)
	want := "apikey:" + hash
	if got.String() != want {
		t.Errorf("APIKey = %q, want %q", got.String(), want)
	}
	if cachekeys.APIKeyTTL != 0 {
		t.Errorf("APIKeyTTL = %v, want 0 (revocation in payload, not Redis TTL)",
			cachekeys.APIKeyTTL)
	}
}

// TestRateLimitTTL pins the 2× window relationship documented in
// ADR-0007. Anything less and the counter could still be live when
// the next window starts; anything more is wasted Redis memory.
func TestRateLimitTTL(t *testing.T) {
	for _, w := range []time.Duration{time.Second, 30 * time.Second, time.Minute, time.Hour} {
		if got := cachekeys.RateLimitTTL(w); got != 2*w {
			t.Errorf("RateLimitTTL(%v) = %v, want 2× = %v", w, got, 2*w)
		}
	}
}

func TestAllKeysHaveDistinctPrefixes(t *testing.T) {
	// Regression guard: every key class must have a unique first
	// segment so cluster-slot distribution is natural + grep-able.
	xlm := canonical.NativeAsset()
	usdc, _ := canonical.NewClassicAsset("USDC", usdcIssuer)
	now := time.Now()

	prefixes := map[string]string{
		"price":      cachekeys.Price(xlm).String(),
		"vwap":       cachekeys.VWAP(xlm, usdc, time.Minute).String(),
		"confidence": cachekeys.Confidence(xlm, usdc, time.Minute).String(),
		"ohlc":       cachekeys.OHLC(xlm, usdc, "1m", now).String(),
		"rl":         cachekeys.RateLimitKey("x", now, time.Minute).String(),
		"toml":       cachekeys.TOML("example.com").String(),
		"meta":       cachekeys.Metadata(xlm).String(),
		"sub":        cachekeys.Subscriber("c", "s").String(),
		"div":        cachekeys.Divergence(canonical.Pair{Base: xlm, Quote: usdc}).String(),
		"freeze":     cachekeys.Freeze(xlm, usdc).String(),
		"health":     cachekeys.Health("src").String(),
	}
	for want, got := range prefixes {
		first := strings.SplitN(got, ":", 2)[0]
		if first != want {
			t.Errorf("key %q should start with %q:", got, want)
		}
	}
}

// TestMarketsListFamily golden-pins the four `markets:list:*` shapes.
// These previously did NOT exist as canonical builders — call sites
// in cmd/stellarindex-api/main.go hand-appended
// `":order=" + marketsOrderKey(order)` (and further ad-hoc suffixes
// for source/asset/pools) onto [cachekeys.MarketsList]'s bare
// `string` result. That compiled cleanly against the old
// `string`-returning MarketsList because a canonical builder's
// output and a hand-rolled suffix were the SAME Go type — exactly
// the ad-hoc-key-construction bug class this package exists to
// close. Against the typed [cachekeys.MarketsListKey] that
// concatenation is a compile error (mismatched types
// MarketsListKey and string), which is what forced the migration to
// these dedicated builders. The golden strings below assert the
// wire bytes are UNCHANGED from what the concatenation used to
// produce.
func TestMarketsListFamily(t *testing.T) {
	base := cachekeys.MarketsList("cur1", 25)
	if base.String() != "markets:list:cur1:25" {
		t.Errorf("MarketsList = %q, want %q", base.String(), "markets:list:cur1:25")
	}

	ordered := cachekeys.MarketsListOrdered("cur1", 25, "vol_desc")
	wantOrdered := "markets:list:cur1:25:order=vol_desc"
	if ordered.String() != wantOrdered {
		t.Errorf("MarketsListOrdered = %q, want %q", ordered.String(), wantOrdered)
	}

	bySource := cachekeys.MarketsListBySource("cur1", 25, "vol_desc", "binance")
	wantBySource := "markets:list:cur1:25:order=vol_desc:source=binance"
	if bySource.String() != wantBySource {
		t.Errorf("MarketsListBySource = %q, want %q", bySource.String(), wantBySource)
	}

	byAsset := cachekeys.MarketsListByAsset("cur1", 25, "vol_desc", "native")
	wantByAsset := "markets:list:cur1:25:order=vol_desc:asset=native"
	if byAsset.String() != wantByAsset {
		t.Errorf("MarketsListByAsset = %q, want %q", byAsset.String(), wantByAsset)
	}

	pools := cachekeys.MarketsListPools("cur1", 25, "vol_desc",
		[]string{"aquarius", "comet", "phoenix", "sdex", "soroswap"},
		"native", "USDC-"+usdcIssuer, "")
	wantPools := "markets:list:cur1:25:order=vol_desc:pools=1:src=aquarius,comet,phoenix,sdex,soroswap:base=native:quote=USDC-" + usdcIssuer + ":asset="
	if pools.String() != wantPools {
		t.Errorf("MarketsListPools = %q, want %q", pools.String(), wantPools)
	}

	// Empty-sources edge case (unfiltered /v1/pools passes the full
	// DEX registry list, never nil, but the builder must still
	// behave sanely if ever called with none).
	emptySrc := cachekeys.MarketsListPools("", 5, "pair", nil, "", "", "")
	wantEmptySrc := "markets:list::5:order=pair:pools=1:src=:base=:quote=:asset="
	if emptySrc.String() != wantEmptySrc {
		t.Errorf("MarketsListPools(empty) = %q, want %q", emptySrc.String(), wantEmptySrc)
	}
}

func TestAssetsList(t *testing.T) {
	k := cachekeys.AssetsList("cur9", 100)
	if k.String() != "assets:list:cur9:100" {
		t.Errorf("AssetsList = %q, want %q", k.String(), "assets:list:cur9:100")
	}
	if cachekeys.CatalogueListTTL != 60*time.Second {
		t.Errorf("CatalogueListTTL = %v, want 60s", cachekeys.CatalogueListTTL)
	}
}

func TestOracleLatest(t *testing.T) {
	// Order-independence: the same asset-key set in a different
	// input order must land on the same cache key (the doc comment's
	// documented contract).
	a := cachekeys.OracleLatest([]string{"native", "USDC-" + usdcIssuer}, "")
	b := cachekeys.OracleLatest([]string{"USDC-" + usdcIssuer, "native"}, "")
	if a.String() != b.String() {
		t.Errorf("OracleLatest not order-independent: %q vs %q", a.String(), b.String())
	}
	want := "oracle:latest:USDC-" + usdcIssuer + "|native:"
	if a.String() != want {
		t.Errorf("OracleLatest = %q, want %q", a.String(), want)
	}
	filtered := cachekeys.OracleLatest([]string{"native"}, "reflector")
	wantFiltered := "oracle:latest:native:reflector"
	if filtered.String() != wantFiltered {
		t.Errorf("OracleLatest(filtered) = %q, want %q", filtered.String(), wantFiltered)
	}
	if cachekeys.OracleLatestTTL != 30*time.Second {
		t.Errorf("OracleLatestTTL = %v, want 30s", cachekeys.OracleLatestTTL)
	}
}

// wantsPriceKey exists only to prove [cachekeys.PriceKey] is usable
// as a distinct parameter type — the positive half of the
// compile-time misuse demonstration below.
func wantsPriceKey(cachekeys.PriceKey) {}

// TestTypedKeysAreDistinctFamilies is the compile-time misuse
// example the typed-key design exists to produce. The POSITIVE case
// (a PriceKey satisfies a PriceKey-typed parameter) actually
// compiles and runs, below. The NEGATIVE cases are commented out —
// per definition, a _test.go file must compile, so a real type
// error can't live as executable code; the comments show exactly
// what fails and why, mirroring the "commented code" option in this
// package's typed-key design brief.
func TestTypedKeysAreDistinctFamilies(t *testing.T) {
	xlm := canonical.NativeAsset()
	usdc, _ := canonical.NewClassicAsset("USDC", usdcIssuer)

	// Compiles: PriceKey is a PriceKey.
	wantsPriceKey(cachekeys.Price(xlm))

	// Does NOT compile — a VWAPKey is not a PriceKey, even though
	// both are `string` underneath:
	//
	//   wantsPriceKey(cachekeys.VWAP(xlm, usdc, time.Minute))
	//   // error: cannot use cachekeys.VWAP(xlm, usdc, time.Minute)
	//   // (value of type cachekeys.VWAPKey) as cachekeys.PriceKey
	//   // value in argument to wantsPriceKey

	// Does NOT compile — a hand-rolled ad-hoc string (the exact
	// bug class this package exists to close) is not a PriceKey
	// either, even though it has the identical wire bytes:
	//
	//   wantsPriceKey("price:" + xlm.String())
	//   // error: cannot use "price:" + xlm.String() (value of
	//   // type string) as cachekeys.PriceKey value in argument
	//   // to wantsPriceKey

	// Does NOT compile — go-redis's Cmdable methods take a plain
	// `string`, so handing them a typed key directly (skipping the
	// explicit .String() conversion) fails the same way; this is
	// deliberate — every redis.Cmdable call site's `.String()` is a
	// visible, grep-able "this key just left the typed world"
	// marker:
	//
	//   var rdb redis.Cmdable
	//   rdb.Get(ctx, cachekeys.Price(xlm))
	//   // error: cannot use cachekeys.Price(xlm) (value of type
	//   // cachekeys.PriceKey) as string value in argument to
	//   // rdb.Get

	_ = usdc // keep the import path exercised even if unused above
}
