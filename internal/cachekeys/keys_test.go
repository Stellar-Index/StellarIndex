package cachekeys_test

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"gopkg.in/yaml.v3"

	"github.com/Stellar-Index/StellarIndex/internal/cachekeys"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/ratelimit"
)

// usdcIssuer is the Circle USDC issuer — reused as a realistic G-address
// fixture across tests.
const usdcIssuer = "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"

// Golden-string tests: each wire string is byte-identical to what the untyped string builders produced, since the typed-key
// migration (ADR-0007) must not invalidate a live Redis key on deploy. Each assertion calls [*.String] as a real call site does
// at the redis.Cmdable boundary.

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
	// internal/ratelimit builds "rl:<escape(key)>:<bucket>" inside Bucket.Charge and cachekeys mirrors it for readers. A hardcoded
	// prefix assertion would not catch drift, so drive a real *ratelimit.Bucket against miniredis and diff the actual key with
	// cachekeys.RateLimitKey.
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	now := time.Unix(1_750_000_000, 0).UTC()
	subject := "2001:db8::1"
	b := ratelimit.New(rdb, 5, time.Minute, ratelimit.WithClock(func() time.Time { return now }))
	if _, err := b.Take(context.Background(), subject); err != nil {
		t.Fatalf("Bucket.Take: %v", err)
	}

	keys := mr.Keys()
	if len(keys) != 1 {
		t.Fatalf("expected exactly one key written by Bucket.Take, got %v", keys)
	}
	want := keys[0]
	got := cachekeys.RateLimitKey(subject, now, time.Minute).String()
	if got != want {
		t.Errorf("cachekeys.RateLimitKey drifted from internal/ratelimit's actual write: got %q, want %q (the real key Bucket.Take wrote)", got, want)
	}
}

func TestRateLimitKey_EscapesSubjectForParityWithBucket(t *testing.T) {
	// Subjects containing `:` (e.g. IPv6 addresses) are url.QueryEscape'd by
	// internal/ratelimit/bucket.go's Take() to prevent cross-subject collisions. This mirror
	// MUST escape identically or the two sides produce different keys for one subject.
	now := time.Unix(1_750_000_000, 0).UTC()
	k := cachekeys.RateLimitKey("2001:db8::1", now, time.Minute)
	if !strings.HasPrefix(k.String(), "rl:2001%3Adb8%3A%3A1:") {
		t.Errorf("RateLimitKey did not escape `:` in IPv6 subject: got %q", k.String())
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

// TestVWAPProvenance covers the cache key and the constant marker the triangulation worker stamps; the API reads it by byte
// equality, so both sides must change together or `flags.triangulated` breaks.
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
	// Sibling-key check: the provenance key must be the VWAP key plus `:provenance`, as the aggregator writes both atomically and a
	// mismatched suffix orphans the marker. The types are distinct, so compare via .String() on both sides.
	vwap := cachekeys.VWAP(xlm, usdc, 5*time.Minute)
	if got.String() != vwap.String()+":provenance" {
		t.Errorf("VWAPProvenance %q is not VWAP %q + :provenance", got.String(), vwap.String())
	}
	if cachekeys.VWAPProvenanceTriangulated != "triangulated" {
		t.Errorf("marker = %q, want 'triangulated' (API matches by byte-equality)",
			cachekeys.VWAPProvenanceTriangulated)
	}
}

// TestVWAPCompositeMeta pins the composite-meta marker: the VWAP value key plus `:composite_meta`. A mismatched suffix would
// orphan the router quality flags Step 3 reads.
func TestVWAPCompositeMeta(t *testing.T) {
	xlm := canonical.NativeAsset()
	gbp := canonical.Asset{Type: canonical.AssetFiat, Code: "GBP"}

	got := cachekeys.VWAPCompositeMeta(xlm, gbp, 5*time.Minute)
	vwap := cachekeys.VWAP(xlm, gbp, 5*time.Minute)
	if got.String() != vwap.String()+":composite_meta" {
		t.Errorf("VWAPCompositeMeta %q is not VWAP %q + :composite_meta", got.String(), vwap.String())
	}
}

// TestVWAPObservedAt pins the observed-at sibling's wire shape and
// that its value round-trips: the aggregator and the API are separate
// binaries, so both the suffix and the encoding are a wire contract.
func TestVWAPObservedAt(t *testing.T) {
	xlm := canonical.NativeAsset()
	gbp := canonical.Asset{Type: canonical.AssetFiat, Code: "GBP"}

	got := cachekeys.VWAPObservedAt(xlm, gbp, 5*time.Minute)
	vwap := cachekeys.VWAP(xlm, gbp, 5*time.Minute)
	if got.String() != vwap.String()+":observed_at" {
		t.Errorf("VWAPObservedAt %q is not VWAP %q + :observed_at", got.String(), vwap.String())
	}

	at := time.Date(2026, 7, 25, 12, 3, 0, 0, time.FixedZone("x", 3600))
	back, err := cachekeys.ParseVWAPObservedAt(cachekeys.FormatVWAPObservedAt(at))
	if err != nil || !back.Equal(at) || back.Location() != time.UTC {
		t.Errorf("round trip of %v = (%v, %v), want the same instant in UTC", at, back, err)
	}
	if _, err := cachekeys.ParseVWAPObservedAt("0.124200000000"); err == nil {
		t.Error("a VWAP value parsed as an observed_at stamp")
	}
}

// TestVWAPCoverage pins the coverage sibling's wire shape and its value
// encoding, which the aggregator and the API share across binaries.
func TestVWAPCoverage(t *testing.T) {
	xlm := canonical.NativeAsset()
	gbp := canonical.Asset{Type: canonical.AssetFiat, Code: "GBP"}

	got := cachekeys.VWAPCoverage(xlm, gbp, time.Hour)
	if want := cachekeys.VWAP(xlm, gbp, time.Hour).String() + ":coverage"; got.String() != want {
		t.Errorf("VWAPCoverage = %q, want %q", got.String(), want)
	}

	from := time.Date(2026, 7, 25, 10, 3, 0, 500, time.FixedZone("x", 3600))
	for _, c := range []cachekeys.WindowCoverage{{}, {Truncated: true, CoveredFrom: from}} {
		back, err := cachekeys.ParseVWAPCoverage(cachekeys.FormatVWAPCoverage(c))
		if err != nil || back.Truncated != c.Truncated || !back.CoveredFrom.Equal(c.CoveredFrom) {
			t.Errorf("round trip of %+v = (%+v, %v)", c, back, err)
		}
	}
	for _, raw := range []string{"", "0.124200000000", "true"} {
		if _, err := cachekeys.ParseVWAPCoverage(raw); err == nil {
			t.Errorf("ParseVWAPCoverage(%q) succeeded; an unreadable value must not read as complete", raw)
		}
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

	// The override tombstone shares the ACL-covered `freeze:` prefix and
	// must never be read as a marker: no asset id parses as "override".
	if got, want := cachekeys.FreezeOverride(xlm, usdc).String(), "freeze:override:native:USDC-"+usdcIssuer; got != want {
		t.Errorf("FreezeOverride = %q, want %q", got, want)
	}
	if _, err := canonical.ParseAsset("override"); err == nil {
		t.Error(`"override" parses as an asset id, so freeze:override:* can collide with a freeze marker`)
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
		t.Errorf("APIKeyTTL = %v, want 0 (operator-issued keys are persistent; revocation in payload, not Redis TTL)",
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
		"meta":       cachekeys.Metadata(xlm).String(),
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

// TestMarketsListFamily golden-pins the four `markets:list:*` shapes. The canonical builders spare cmd/stellarindex-api/main.go
// from hand-appending suffixes (a compile error against the typed [cachekeys.MarketsListKey]); the goldens assert identical wire
// bytes.
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

// TestTypedKeysAreDistinctFamilies shows the compile-time misuse protection of typed keys. The positive case (PriceKey into a
// PriceKey parameter) runs below; the negative cases are commented out since a _test.go file must compile.
func TestTypedKeysAreDistinctFamilies(t *testing.T) {
	xlm := canonical.NativeAsset()
	usdc, _ := canonical.NewClassicAsset("USDC", usdcIssuer)

	// Compiles: PriceKey is a PriceKey.
	wantsPriceKey(cachekeys.Price(xlm))

	// Does NOT compile: a VWAPKey is not a PriceKey, though both are `string` underneath.
	//
	//   wantsPriceKey(cachekeys.VWAP(xlm, usdc, time.Minute))
	//   // error: cannot use cachekeys.VWAP(xlm, usdc, time.Minute)
	//   // (value of type cachekeys.VWAPKey) as cachekeys.PriceKey
	//   // value in argument to wantsPriceKey

	// Does NOT compile: a hand-rolled ad-hoc string (the bug class this package closes) is not
	// a PriceKey, though it has identical wire bytes.
	//
	//   wantsPriceKey("price:" + xlm.String())
	//   // error: cannot use "price:" + xlm.String() (value of
	//   // type string) as cachekeys.PriceKey value in argument
	//   // to wantsPriceKey

	// Does NOT compile: go-redis's Cmdable methods take a plain `string`, so a typed key
	// passed without .String() fails the same way. That is deliberate: each call site's
	// `.String()` is a grep-able "this key just left the typed world" marker.
	//
	//   var rdb redis.Cmdable
	//   rdb.Get(ctx, cachekeys.Price(xlm))
	//   // error: cannot use cachekeys.Price(xlm) (value of type
	//   // cachekeys.PriceKey) as string value in argument to
	//   // rdb.Get

	_ = usdc // keep the import path exercised even if unused above
}

// TestAPIKeyRecords_NotOnAnEvictingInstance: `apikey:` records are the credential of record (plaintext is unrecoverable), so the
// instance must never evict them: noeviction, or volatile-* while records carry no TTL. allkeys-* evicts regardless of TTL,
// silently deleting live credentials. Both codified Redis deployments are checked.
func TestAPIKeyRecords_NotOnAnEvictingInstance(t *testing.T) {
	policies := map[string]string{
		"redis-sentinel defaults": sentinelDefaultsPolicy(t),
		"docker-compose dev":      composeDevPolicy(t),
	}
	for where, policy := range policies {
		switch {
		case policy == "noeviction":
		case strings.HasPrefix(policy, "volatile-") && cachekeys.APIKeyTTL == 0:
		default:
			t.Errorf("%s: maxmemory-policy %q can evict apikey: records written with APIKeyTTL=%s; an evicted record is an unrecoverable credential (silent permanent 401)", where, policy, cachekeys.APIKeyTTL)
		}
	}
}

// TestRedisRole_EnforcesPolicyOnRunningInstances pins the other half:
// redis.conf is rendered on first run only, so without a live
// CONFIG SET a corrected default never reaches a running cluster.
func TestRedisRole_EnforcesPolicyOnRunningInstances(t *testing.T) {
	raw, err := os.ReadFile("../../configs/ansible/roles/redis-sentinel/tasks/03-redis-configure.yml")
	if err != nil {
		t.Fatalf("read redis-configure tasks: %v", err)
	}
	for _, want := range []string{
		"CONFIG SET maxmemory-policy {{ redis_maxmemory_policy }}",
		"CONFIG REWRITE",
	} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("03-redis-configure.yml lacks %q: a policy change would only reach freshly built clusters", want)
		}
	}
}

func sentinelDefaultsPolicy(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile("../../configs/ansible/roles/redis-sentinel/defaults/main.yml")
	if err != nil {
		t.Fatalf("read redis-sentinel defaults: %v", err)
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if v, ok := strings.CutPrefix(line, "redis_maxmemory_policy:"); ok {
			return strings.TrimSpace(v)
		}
	}
	t.Fatal("redis_maxmemory_policy not found in redis-sentinel defaults")
	return ""
}

func composeDevPolicy(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile("../../deploy/docker-compose/dev.yaml")
	if err != nil {
		t.Fatalf("read dev compose: %v", err)
	}
	var doc struct {
		Services map[string]struct {
			Command []string `yaml:"command"`
		} `yaml:"services"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse dev compose: %v", err)
	}
	cmd := doc.Services["redis"].Command
	for i, arg := range cmd {
		if arg == "--maxmemory-policy" && i+1 < len(cmd) {
			return cmd[i+1]
		}
	}
	t.Fatal("--maxmemory-policy not found in the dev compose redis command")
	return ""
}

// The VWAPMaxAge doc comment tells an operator what notices a dead publisher and when.
// These tests derive the timings from both Prometheus rule trees and require the comment to
// state them, so tuning either side fails here until the prose follows.

var vwapAlertRuleTrees = []string{
	"../../deploy/monitoring/rules",
	"../../configs/prometheus/rules.r1",
}

type promRule struct {
	Alert  string            `yaml:"alert"`
	Expr   string            `yaml:"expr"`
	For    string            `yaml:"for"`
	Labels map[string]string `yaml:"labels"`
}

func loadPromRule(t *testing.T, file, alert string) promRule {
	t.Helper()
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("read %s: %v", file, err)
	}
	var doc struct {
		Groups []struct {
			Rules []promRule `yaml:"rules"`
		} `yaml:"groups"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse %s: %v", file, err)
	}
	for _, g := range doc.Groups {
		for _, r := range g.Rules {
			if r.Alert == alert {
				return r
			}
		}
	}
	t.Fatalf("alert %s not found in %s", alert, file)
	return promRule{}
}

// vwapMaxAgeDoc returns the VWAPMaxAge doc comment, whitespace-collapsed.
func vwapMaxAgeDoc(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile("keys.go")
	if err != nil {
		t.Fatalf("read keys.go: %v", err)
	}
	lines := strings.Split(string(raw), "\n")
	end := -1
	for i, l := range lines {
		if strings.HasPrefix(l, "const VWAPMaxAge ") {
			end = i
		}
	}
	if end < 0 {
		t.Fatal("const VWAPMaxAge not found in keys.go")
	}
	start := end
	for start > 0 && strings.HasPrefix(lines[start-1], "//") {
		start--
	}
	var b strings.Builder
	for _, l := range lines[start:end] {
		b.WriteString(strings.TrimPrefix(l, "//"))
		b.WriteByte(' ')
	}
	return strings.Join(strings.Fields(b.String()), " ")
}

var (
	promRangeRe = regexp.MustCompile(`\[(\d+[smh])\]`)
	promOrRe    = regexp.MustCompile(`(?i)\s+or\s+`)
)

// silentPageDelays returns how long after the last VWAP write the
// aggregator-silent page fires, via its rate leg (process still scraped)
// and its absent_over_time leg (process gone): range window + `for`.
func silentPageDelays(t *testing.T, r promRule) (scraped, gone time.Duration) {
	t.Helper()
	hold, err := time.ParseDuration(r.For)
	if err != nil {
		t.Fatalf("%s: for %q: %v", r.Alert, r.For, err)
	}
	for _, leg := range promOrRe.Split(r.Expr, -1) {
		m := promRangeRe.FindStringSubmatch(leg)
		if m == nil {
			t.Fatalf("%s: leg %q has no range window", r.Alert, leg)
		}
		w, err := time.ParseDuration(m[1])
		if err != nil {
			t.Fatalf("%s: window %q: %v", r.Alert, m[1], err)
		}
		if strings.Contains(leg, "absent_over_time(") {
			gone = w + hold
		} else {
			scraped = w + hold
		}
	}
	if scraped == 0 || gone == 0 {
		t.Fatalf("%s: expected a rate leg and an absent_over_time leg, got expr %q", r.Alert, r.Expr)
	}
	return scraped, gone
}

func TestVWAPMaxAgeDoc_NamesThePageAndItsDelay(t *testing.T) {
	doc := vwapMaxAgeDoc(t)
	if !strings.Contains(doc, "`stellarindex_aggregator_silent`") {
		t.Fatalf("VWAPMaxAge doc must name the page covering a dead publisher, stellarindex_aggregator_silent; doc: %q", doc)
	}
	for _, tree := range vwapAlertRuleTrees {
		r := loadPromRule(t, tree+"/aggregator.yml", "stellarindex_aggregator_silent")
		if got := r.Labels["severity"]; got != "page" {
			t.Errorf("%s: stellarindex_aggregator_silent severity = %q, want page", tree, got)
		}
		scraped, gone := silentPageDelays(t, r)
		if scraped <= cachekeys.VWAPMaxAge {
			t.Errorf("%s: page fires %v after the last write, not after VWAPMaxAge %v; the doc's \"customers see 404s first\" is now false", tree, scraped, cachekeys.VWAPMaxAge)
		}
		want := []string{
			fmt.Sprintf("about %d minutes after the last write while the process is still scraped", int(scraped.Minutes())),
			fmt.Sprintf("about %d minutes after it when the process is gone", int(gone.Minutes())),
			fmt.Sprintf("opens %d to %d minutes before anyone is paged",
				int((scraped - cachekeys.VWAPMaxAge).Minutes()), int((gone - cachekeys.VWAPMaxAge).Minutes())),
		}
		for _, w := range want {
			if !strings.Contains(doc, w) {
				t.Errorf("%s: VWAPMaxAge doc must state %q (derived from the rule); doc: %q", tree, w, doc)
			}
		}
	}
}

func TestVWAPMaxAgeDoc_PriceStaleIsATicket(t *testing.T) {
	doc := vwapMaxAgeDoc(t)
	if strings.Contains(doc, "pages at") {
		t.Errorf("VWAPMaxAge doc claims a staleness threshold pages; doc: %q", doc)
	}
	if !strings.Contains(doc, "`stellarindex_api_price_stale`, is a ticket") {
		t.Errorf("VWAPMaxAge doc must state stellarindex_api_price_stale is a ticket; doc: %q", doc)
	}
	for _, tree := range vwapAlertRuleTrees {
		r := loadPromRule(t, tree+"/api.yml", "stellarindex_api_price_stale")
		if got := r.Labels["severity"]; got != "ticket" {
			t.Errorf("%s: stellarindex_api_price_stale severity = %q; the VWAPMaxAge doc calls it a ticket", tree, got)
		}
	}
}

// TestAPIKeyIndex pins the wire strings of the API-key index family. Both keys sit under `apikey-index:` so one Redis ACL pattern
// (`~apikey-index:*`) admits the index and its build lock; neither sits under `apikey:`, whose credential walk GETs every hit and
// would fail every fallback lookup with WRONGTYPE on a HASH.
func TestAPIKeyIndex(t *testing.T) {
	if got, want := cachekeys.APIKeyIndex().String(), "apikey-index:v1"; got != want {
		t.Errorf("APIKeyIndex = %q, want %q", got, want)
	}
	if got, want := cachekeys.APIKeyIndexBuildLock().String(), "apikey-index:build-lock"; got != want {
		t.Errorf("APIKeyIndexBuildLock = %q, want %q", got, want)
	}
	recordPrefix := cachekeys.APIKey("").String()
	for _, k := range []string{cachekeys.APIKeyIndex().String(), cachekeys.APIKeyIndexBuildLock().String()} {
		if !strings.HasPrefix(k, "apikey-index:") {
			t.Errorf("%q is outside the apikey-index: family the ACL pattern admits", k)
		}
		if strings.HasPrefix(k, recordPrefix) {
			t.Errorf("%q is inside the %q record namespace the credential walk GETs", k, recordPrefix)
		}
	}
}
