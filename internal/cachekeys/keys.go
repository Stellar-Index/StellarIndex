package cachekeys

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// ─── Typed-key mechanism ───
//
// Every key family has its own named string type, so a hand-rolled fmt.Sprintf key or another
// family's key handed to a Redis call is a compile error; crossing to the untyped wire calls
// .String() explicitly. This stops prewarm and handler key construction from drifting apart.
// Every .String() value is pinned byte-for-byte by keys_test.go golden strings.

// ─── Price — latest aggregated price per asset ────────────────────
//
// Wire shape: `price:<asset_id>`
// Writer: aggregator
// Reader: api
// TTL: 60 s (refreshed on every aggregation cycle).

// PriceKey is the typed Redis key for the `price:<asset_id>` family.
type PriceKey string

// String returns the wire-format key.
func (k PriceKey) String() string { return string(k) }

// Price returns the cache key for the latest aggregated price of asset.
func Price(asset canonical.Asset) PriceKey {
	return PriceKey("price:" + asset.String())
}

// PriceTTL is the expiry for price: keys.
const PriceTTL = 60 * time.Second

// ─── VWAP — per-pair + window pre-compute ─────────────────────────
//
// Wire shape: `vwap:<base>:<quote>:<window-seconds>`
// TTL: the window, bounded by [VWAPMaxAge] — see [VWAPTTL].

// VWAPKey is the typed Redis key for the `vwap:<base>:<quote>:<window-seconds>` family.
type VWAPKey string

// String returns the wire-format key.
func (k VWAPKey) String() string { return string(k) }

// VWAP returns the cache key for a rolling VWAP over window for the given pair.
func VWAP(base, quote canonical.Asset, window time.Duration) VWAPKey {
	return VWAPKey(fmt.Sprintf("vwap:%s:%s:%d",
		base.String(), quote.String(), int(window.Seconds())))
}

// VWAPMaxAge is the SILENCE GRACE on a published VWAP: how long a cached VWAP may keep serving
// after the aggregator stops refreshing it. It is NOT the aggregation window: every (pair, window)
// is rewritten on every tick ([orchestrator.DefaultInterval] = 30 s), so an old value means
// nobody is publishing. Keying the TTL to the window would let a 24 h key outlive its writer by
// 24 h, and /v1/price stamps observed_at at request time, so it would be served as current.
//
// No page precedes the expiry: customers see 404s first. The page for a dead publisher is
// `stellarindex_aggregator_silent`, which fires about 10 minutes after the last write while the
// process is still scraped and about 15 minutes after it when the process is gone, so the 404
// window opens 5 to 10 minutes before anyone is paged. The freshness alert,
// `stellarindex_api_price_stale`, is a ticket and reads a gauge this same aggregator emits.
// Tighten the alert, not this grace.
const VWAPMaxAge = 5 * time.Minute

// VWAPTTL is the TTL for a VWAP key: its window, bounded by [VWAPMaxAge] so a value never outlives its publisher by more than the silence grace; 0 for a zero window (don't cache).
func VWAPTTL(window time.Duration) time.Duration {
	return VWAPTTLWithMaxAge(window, VWAPMaxAge)
}

// VWAPTTLWithMaxAge is [VWAPTTL] with an explicit silence-grace ceiling in place of the package default, for a caller whose tick interval differs from [orchestrator.DefaultInterval].
func VWAPTTLWithMaxAge(window, maxAge time.Duration) time.Duration {
	if window <= 0 || window < maxAge {
		return window
	}
	return maxAge
}

// ─── VWAP Provenance — was this VWAP triangulated? ──────────────────
//
// Wire shape: `vwap:<base>:<quote>:<window-seconds>:provenance`; TTL matches the value key. Only
// the triangulation worker writes "triangulated"; the direct refresh does not, so absence means
// direct (or unknown) and readers treat empty as not triangulated (ADR-0018).

// VWAPProvenanceKey is the typed Redis key for the `vwap:<base>:<quote>:<window>:provenance` family, a DISTINCT type from [VWAPKey] so marker and value keys are not interchangeable.
type VWAPProvenanceKey string

// String returns the wire-format key.
func (k VWAPProvenanceKey) String() string { return string(k) }

// VWAPProvenance returns the cache key marker for whether a `vwap:<base>:<quote>:<window>` value came from the triangulation worker.
func VWAPProvenance(base, quote canonical.Asset, window time.Duration) VWAPProvenanceKey {
	return VWAPProvenanceKey(fmt.Sprintf("vwap:%s:%s:%d:provenance",
		base.String(), quote.String(), int(window.Seconds())))
}

// VWAPProvenanceTriangulated is the value stamped into the [VWAPProvenance] key; the API matches it byte-for-byte.
const VWAPProvenanceTriangulated = "triangulated"

// ─── VWAP Observed-At — when the served value was observed ─────────
//
// Wire shape: `vwap:<base>:<quote>:<window-seconds>:observed_at`; TTL matches the value key,
// freeze keep-alive included. Value: RFC 3339 UTC ([FormatVWAPObservedAt]) of the closed bucket
// boundary the window ends at, written by both value writers inside the value's MULTI/EXEC. A
// freeze extends the value's TTL without rewriting it, so key existence is not freshness: a value
// with no readable stamp is not served.

// VWAPObservedAtKey is the typed Redis key for the `vwap:<base>:<quote>:<window>:observed_at` family.
type VWAPObservedAtKey string

// String returns the wire-format key.
func (k VWAPObservedAtKey) String() string { return string(k) }

// VWAPObservedAt returns the cache key for the observation time of the `vwap:<base>:<quote>:<window>` value.
func VWAPObservedAt(base, quote canonical.Asset, window time.Duration) VWAPObservedAtKey {
	return VWAPObservedAtKey(fmt.Sprintf("vwap:%s:%s:%d:observed_at",
		base.String(), quote.String(), int(window.Seconds())))
}

// FormatVWAPObservedAt encodes t as the [VWAPObservedAt] cache value.
func FormatVWAPObservedAt(t time.Time) string {
	return t.UTC().Format(time.RFC3339Nano)
}

// ParseVWAPObservedAt decodes a [VWAPObservedAt] cache value.
func ParseVWAPObservedAt(raw string) (time.Time, error) {
	t, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		return time.Time{}, fmt.Errorf("cachekeys: decode vwap observed_at: %w", err)
	}
	return t.UTC(), nil
}

// ─── VWAP Coverage — did the value read its whole window? ──────────
//
// Wire shape: `vwap:<base>:<quote>:<window-seconds>:coverage`; TTL matches the value key. Value
// ([FormatVWAPCoverage]): "complete", or the RFC 3339 covered_from of a window whose trade read
// hit the per-query row cap. The direct refresh writes it in the value's MULTI/EXEC; the
// triangulation pass deletes it (a composite does not track its legs' coverage). An absent or
// unreadable key is "unknown", never "complete".

// VWAPCoverageKey is the typed Redis key for the `vwap:<base>:<quote>:<window>:coverage` family.
type VWAPCoverageKey string

// String returns the wire-format key.
func (k VWAPCoverageKey) String() string { return string(k) }

// VWAPCoverage returns the cache key for the window coverage of the `vwap:<base>:<quote>:<window>` value.
func VWAPCoverage(base, quote canonical.Asset, window time.Duration) VWAPCoverageKey {
	return VWAPCoverageKey(fmt.Sprintf("vwap:%s:%s:%d:coverage",
		base.String(), quote.String(), int(window.Seconds())))
}

// WindowCoverage is how much of its window a published VWAP was computed over.
type WindowCoverage struct {
	// Truncated is true when a trade read hit the row cap, so trades
	// before CoveredFrom may be missing from the value.
	Truncated bool
	// CoveredFrom is the newest of the capped reads' oldest trade
	// timestamps: every trade after it is in the value. Zero unless
	// Truncated.
	CoveredFrom time.Time
}

const vwapCoverageComplete = "complete"

// FormatVWAPCoverage encodes c as the [VWAPCoverage] cache value.
func FormatVWAPCoverage(c WindowCoverage) string {
	if !c.Truncated {
		return vwapCoverageComplete
	}
	return c.CoveredFrom.UTC().Format(time.RFC3339Nano)
}

// ParseVWAPCoverage decodes a [VWAPCoverage] cache value.
func ParseVWAPCoverage(raw string) (WindowCoverage, error) {
	if raw == vwapCoverageComplete {
		return WindowCoverage{}, nil
	}
	t, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		return WindowCoverage{}, fmt.Errorf("cachekeys: decode vwap coverage: %w", err)
	}
	return WindowCoverage{Truncated: true, CoveredFrom: t.UTC()}, nil
}

// ─── VWAP Composite Meta — router quality flags for a composite ────
//
// Wire shape: `vwap:<base>:<quote>:<window-seconds>:composite_meta`; TTL matches the value key.
// Written by the triangulation pass when the graph router prices a target; absent means not
// router-priced this cycle. A low_confidence marker is written even when the composite is NOT
// published over the direct price, so "direct because the only routes were dust" differs from "no
// composite". Decoded by [DecodeCompositeMeta]; a malformed blob leaves flags unset.

// VWAPCompositeMetaKey is the typed Redis key for the `vwap:<base>:<quote>:<window>:composite_meta` family.
type VWAPCompositeMetaKey string

// String returns the wire-format key.
func (k VWAPCompositeMetaKey) String() string { return string(k) }

// VWAPCompositeMeta returns the cache key for a router-priced composite's quality flags on the given (pair, window).
func VWAPCompositeMeta(base, quote canonical.Asset, window time.Duration) VWAPCompositeMetaKey {
	return VWAPCompositeMetaKey(fmt.Sprintf("vwap:%s:%s:%d:composite_meta",
		base.String(), quote.String(), int(window.Seconds())))
}

// CompositeMeta is the read-side view of the blob under a [VWAPCompositeMeta] key, carrying only the signals /v1/price surfaces; other written fields are ignored.
type CompositeMeta struct {
	// Diverged is true when the composite came from routes that
	// disagreed (the router divergence signal).
	Diverged bool `json:"diverged"`
	// Rerouted is true when the composite substituted around a dry
	// configured chain leg (R3 leg-substitution).
	Rerouted bool `json:"rerouted"`
	// PivotUnverified is true when a priced leg was all stablecoin-proxy
	// prints, so its par-to-USD assumption went unchecked.
	PivotUnverified bool `json:"pivot_unverified"`
}

// DecodeCompositeMeta parses a [VWAPCompositeMeta] blob into [CompositeMeta]; best-effort: a malformed payload returns the zero value plus an error the caller may ignore.
func DecodeCompositeMeta(raw []byte) (CompositeMeta, error) {
	var m CompositeMeta
	if err := json.Unmarshal(raw, &m); err != nil {
		return CompositeMeta{}, fmt.Errorf("cachekeys: decode composite meta: %w", err)
	}
	return m, nil
}

// ─── Confidence — multi-factor score per (pair, window) ───────────
//
// Wire shape: `confidence:<base>:<quote>:<window-seconds>`
// Writer: aggregator, beside the vwap: key. Reader: /v1/price confidence field.
// TTL: matches the VWAP key; a score is meaningless once the VWAP it scored expires.
// Value is a JSON confidence.Score (not a bare float) so the API ships the full decomposition
// without a second lookup.

// ConfidenceKey is the typed Redis key for the `confidence:<base>:<quote>:<window-seconds>` family.
type ConfidenceKey string

// String returns the wire-format key.
func (k ConfidenceKey) String() string { return string(k) }

// Confidence returns the cache key for the confidence score on the given (pair, window).
func Confidence(base, quote canonical.Asset, window time.Duration) ConfidenceKey {
	return ConfidenceKey(fmt.Sprintf("confidence:%s:%s:%d",
		base.String(), quote.String(), int(window.Seconds())))
}

// ConfidenceTTL is the TTL for a confidence: key.
func ConfidenceTTL(window time.Duration) time.Duration { return VWAPTTL(window) }

// ─── OHLC — one candle per (pair, granularity, bucket-start) ──────
//
// Wire shape: `ohlc:<base>:<quote>:<granularity>:<bucket-epoch>` (granularity "1m".."1mo";
// bucket-epoch is the Unix seconds of the candle start). Closed candles are immutable and cached
// with NO TTL (CDN-pinned); the open-candle TTL is only a safety-net bound, since the aggregator
// overwrites the key every refresh cycle.

// OHLCKey is the typed Redis key for the `ohlc:<base>:<quote>:<granularity>:<bucket-epoch>` family.
type OHLCKey string

// String returns the wire-format key.
func (k OHLCKey) String() string { return string(k) }

// OHLC returns the cache key for one OHLC candle.
func OHLC(base, quote canonical.Asset, granularity string, bucketStart time.Time) OHLCKey {
	return OHLCKey(fmt.Sprintf("ohlc:%s:%s:%s:%d",
		base.String(), quote.String(),
		granularity, bucketStart.Unix()))
}

// OHLCOpenTTL is the SAFETY-NET TTL for the open candle at any granularity; the aggregator refreshes well before it fires.
const OHLCOpenTTL = time.Hour

// OHLCClosedTTL is the TTL for a closed (historical) candle.
const OHLCClosedTTL = time.Duration(0)

// ─── Rate-limit counters — one per (key, window) ──────────────────
//
// The rl: family is OWNED by internal/ratelimit, which builds and writes keys itself. The
// builders here only pin the wire shape; the parity test diffs them against a real Bucket write.
//
// Wire shape: `rl:<subject>:<window-bucket>`, subject an API-key hash or IP address.

// RateLimitCounterKey is the typed Redis key for the `rl:<subject>:<window-bucket>` family.
type RateLimitCounterKey string

// String returns the wire-format key.
func (k RateLimitCounterKey) String() string { return string(k) }

// RateLimitKey returns the cache key for a rate-limit counter; window is the fixed-window size (typically 60 s).
func RateLimitKey(subject string, now time.Time, window time.Duration) RateLimitCounterKey {
	bucket := now.Unix() / int64(window.Seconds())
	return RateLimitCounterKey(fmt.Sprintf("rl:%s:%d", url.QueryEscape(subject), bucket))
}

// RateLimitTTL is the TTL set on rl: keys.
func RateLimitTTL(window time.Duration) time.Duration { return 2 * window }

// ─── Asset metadata — code/issuer/contract/decimals + SEP-1 overlay─
//
// Wire shape: `meta:<asset_id>`

// MetadataKey is the typed Redis key for the `meta:<asset_id>` family.
type MetadataKey string

// String returns the wire-format key.
func (k MetadataKey) String() string { return string(k) }

// Metadata returns the cache key for the per-asset metadata bundle.
func Metadata(asset canonical.Asset) MetadataKey {
	return MetadataKey("meta:" + asset.String())
}

// MetadataTTL is the expiry for meta: keys.
const MetadataTTL = 5 * time.Minute

// ─── Divergence detector output ───────────────────────────────────
//
// Wire shape: `div:<base>/<quote>`; value is JSON (sources compared, max deviation, threshold).
// The key is per-PAIR, not per-base-asset: a `div:<base>` key would let the last pair refreshed
// clobber the asset's result (XLM/USD diverged, a later XLM/GBP refresh clears it). The by-asset
// reader ORs the per-pair WarningFired flags via the [DivergenceBaseIndex] set, so "firing if ANY
// quote diverges" holds in any refresh order.

// DivergenceKey is the typed Redis key for the `div:<base>/<quote>` family.
type DivergenceKey string

// String returns the wire-format key.
func (k DivergenceKey) String() string { return string(k) }

// Divergence returns the cache key for the latest divergence result for a (base, quote) pair.
func Divergence(pair canonical.Pair) DivergenceKey {
	return DivergenceKey("div:" + pair.String())
}

// DivergenceIndexKey is the typed Redis key for the `div:idx:<base>` family, a per-base SET of quotes, distinct from [DivergenceKey] (a String value).
type DivergenceIndexKey string

// String returns the wire-format key.
func (k DivergenceIndexKey) String() string { return string(k) }

// DivergenceBaseIndex returns the key of the Redis SET listing which quotes have a live `div:<base>/<quote>` value for base; it lives in Redis because the aggregator and API share nothing else.
func DivergenceBaseIndex(base canonical.Asset) DivergenceIndexKey {
	return DivergenceIndexKey("div:idx:" + base.String())
}

// DivergenceTTL is the floor expiry for div: keys; the aggregator extends it so a key never expires before its next write.
const DivergenceTTL = 5 * time.Minute

// ─── Anomaly freeze marker (ADR-0019) ─────────────────────────────
//
// Wire shape: `freeze:<asset_id>:<quote_id>`; value is the JSON anomaly Decision. Presence means
// the pair's latest bucket was frozen; the API reads it via FrozenLooker to set flags.frozen.
// [FreezeTTL] is the silence grace, not the freeze duration: markers are written with "remaining
// hold + FreezeTTL" and deleted outright on unfreeze.

// FreezeKey is the typed Redis key for the `freeze:<asset_id>:<quote_id>` family.
type FreezeKey string

// String returns the wire-format key.
func (k FreezeKey) String() string { return string(k) }

// Freeze returns the cache key for the freeze marker on an (asset, quote) pair.
func Freeze(asset, quote canonical.Asset) FreezeKey {
	return FreezeKey("freeze:" + asset.String() + ":" + quote.String())
}

// FreezeOverride returns the key `stellarindex-ops freeze-unfreeze` writes before clearing a marker, so the aggregator can tell a force-unfreeze from a lapsed marker; it cannot collide with [Freeze].
func FreezeOverride(asset, quote canonical.Asset) FreezeKey {
	return FreezeKey("freeze:override:" + asset.String() + ":" + quote.String())
}

// FreezeTTL is the SILENCE GRACE added to a marker's remaining hold (how long flags.frozen keeps serving after the aggregator stops writing), not the freeze duration, which lives in freeze.State.
const FreezeTTL = 5 * time.Minute

// ─── API-key records ──────────────────────────────────────────────
//
// Wire shape: `apikey:<sha256-hex>`; value is JSON `{identifier, tier, scopes, expires_at?,
// revoked_at?}`. Plaintext keys are NEVER stored: lookup SHA-256-hashes the caller's bytes
// (32-byte high-entropy keys are preimage-safe), so a Redis dump leaks metadata but not keys.
//
// No TTL: expiry and revocation live in the record. That does NOT protect it from an allkeys-*
// eviction policy, and the plaintext cannot be re-issued.

// APIKeyRecordKey is the typed Redis key for the `apikey:<sha256-hex>` family.
type APIKeyRecordKey string

// String returns the wire-format key.
func (k APIKeyRecordKey) String() string { return string(k) }

// APIKey returns the cache key for the API-key record identified by keyHash, which MUST be the hex SHA-256 of the plaintext key (the auth package hashes).
func APIKey(keyHash string) APIKeyRecordKey {
	return APIKeyRecordKey("apikey:" + keyHash)
}

// APIKeyTTL is the TTL for operator-issued apikey: records (self-service and register-mirror records carry auth.MirroredKeyIdleTTL instead).
const APIKeyTTL = time.Duration(0)

// APIKeyCacheKey is the typed Redis key for the `apikey-cache:<sha256-hex>` family: the postgres validator's finite-TTL read-through cache of api_keys rows.
type APIKeyCacheKey string

// String returns the wire-format key.
func (k APIKeyCacheKey) String() string { return string(k) }

// APIKeyCache returns the read-through cache key for keyHash, the hex-encoded SHA-256 of the plaintext key.
func APIKeyCache(keyHash string) APIKeyCacheKey {
	return APIKeyCacheKey("apikey-cache:" + keyHash)
}

// APIKeyCacheEvicted returns the short-lived tombstone an eviction writes for keyHash; while it lives the validator does not re-populate [APIKeyCache].
func APIKeyCacheEvicted(keyHash string) APIKeyCacheKey {
	return APIKeyCacheKey("apikey-cache:" + keyHash + ":evicted")
}

// ─── API-key lookup index ───
//
// Wire shape: `apikey-index:v1`, ONE Redis HASH, no TTL, with fields:
//
//	ready            → "1", written only by a COMPLETE index build
//	k:<key_id>       → <sha256-hex> of the record that KeyID names
//	o:<identifier>   → space-separated <sha256-hex> list of the records that owner holds
//
// ONE hash because under allkeys-* every key is independently evictable: an evicted per-owner set
// would hide live credentials from list, revoke and the tier clamp. Evicting this one key takes
// `ready` too, so readers fall back to the walk.
//
// NOT under `apikey:`: that walk's GET would fail WRONGTYPE on a HASH. It needs its own Redis ACL
// pattern (`~apikey-index:*`); without it every access is NOPERM and the store walks.

// APIKeyIndexKey is the typed Redis key for the `apikey-index:*` family.
type APIKeyIndexKey string

// String returns the wire-format key.
func (k APIKeyIndexKey) String() string { return string(k) }

// APIKeyIndex returns the key of the single-hash lookup index over the `apikey:<sha256-hex>` records.
func APIKeyIndex() APIKeyIndexKey { return APIKeyIndexKey("apikey-index:v1") }

// APIKeyIndexBuildLock returns the SET-NX lock that lets at most one process run the index build at a time.
func APIKeyIndexBuildLock() APIKeyIndexKey { return APIKeyIndexKey("apikey-index:build-lock") }

// APIKeyMintLock is the per-owner lock that serialises a capped mint's count and write across API instances (CreateCapped).
func APIKeyMintLock(identifier string) APIKeyIndexKey {
	return APIKeyIndexKey("apikey-index:mint-lock:" + identifier)
}

// ─── Per-source freshness gauge ───────────────────────────────────
//
// Wire shape: `health:<source>`
// Value: JSON with last_event_ts + lag_ledgers.
// Written by the indexer on every event; read by the API for
// /readyz + by Prometheus for scrape.

// HealthKey is the typed Redis key for the `health:<source>` family.
type HealthKey string

// String returns the wire-format key.
func (k HealthKey) String() string { return string(k) }

// Health returns the cache key for a source freshness gauge.
func Health(source string) HealthKey {
	return HealthKey("health:" + source)
}

// HealthTTL is the expiry for health: keys.
const HealthTTL = 60 * time.Second

// ─── Oracle latest readings — read-through cache ─────────────────
//
// Wire shape: `oracle:latest:<asset-keys-joined>:<source-filter>`; value JSON `{"computed_at": RFC
// 3339, "updates": [...]}` (a bare array is read as a miss). The asset list is sorted before
// joining so the same query hits the same key in any order. TTL 30 s stays inside one oracle push
// interval (1 to 5 minutes) while sparing the 285-580 ms DISTINCT ON (source) sort on every poll.

// OracleLatestKey is the typed Redis key for the `oracle:latest:<asset-keys-joined>:<source-filter>` family.
type OracleLatestKey string

// String returns the wire-format key.
func (k OracleLatestKey) String() string { return string(k) }

// OracleLatest returns the cache key for the multi-source latest reading of a deduped, sorted list of asset keys with optional source filter (empty = "every source").
func OracleLatest(assetKeys []string, sourceFilter string) OracleLatestKey {
	// Copy then sort so the key is order-independent; `|` cannot appear in a canonical asset_id.
	sorted := append([]string(nil), assetKeys...)
	sortStrings(sorted)
	return OracleLatestKey("oracle:latest:" + strings.Join(sorted, "|") + ":" + sourceFilter)
}

// OracleLatestTTL is the TTL for `oracle:latest:*` cache entries.
const OracleLatestTTL = 30 * time.Second

// ─── Assets list / Markets list: read-through cache ───
//
// Wire shape:
//
//	`assets:list:<cursor>:<limit>`
//	`markets:list:<cursor>:<limit>[:order=<order>[:source=<source>|:asset=<asset>|:pools=1:src=<sources>:base=<base>:quote=<quote>:asset=<asset>]]`
//
// TTL-only invalidation (60 s): both derive from a 14-day rolling window over trades. Markets has
// several shapes because /v1/markets and /v1/pools layer filters on the base key; each has its own
// builder, since concatenating onto [MarketsList] at a call site is the ad-hoc key bug class this
// package closes (ADR-0007).

// AssetsListKey is the typed Redis key for the `assets:list:<cursor>:<limit>` family.
type AssetsListKey string

// String returns the wire-format key.
func (k AssetsListKey) String() string { return string(k) }

// AssetsList returns the cache key for one page of /v1/assets.
func AssetsList(cursor string, limit int) AssetsListKey {
	return AssetsListKey(fmt.Sprintf("assets:list:%s:%d", cursor, limit))
}

// MarketsListKey is the typed Redis key for the `markets:list:*` family; the family comment above lists the shapes each builder produces.
type MarketsListKey string

// String returns the wire-format key.
func (k MarketsListKey) String() string { return string(k) }

// MarketsList returns the base cache key for one page of /v1/markets or /v1/pools with no further filter dimensions.
func MarketsList(cursor string, limit int) MarketsListKey {
	return MarketsListKey(fmt.Sprintf("markets:list:%s:%d", cursor, limit))
}

// MarketsListOrdered extends [MarketsList] with the sort order.
func MarketsListOrdered(cursor string, limit int, order string) MarketsListKey {
	return MarketsListKey(fmt.Sprintf("markets:list:%s:%d:order=%s", cursor, limit, order))
}

// MarketsListBySource extends [MarketsListOrdered] with a source filter — the cache key for SourceMarkets (`/v1/markets?source=`).
func MarketsListBySource(cursor string, limit int, order, source string) MarketsListKey {
	return MarketsListKey(fmt.Sprintf("markets:list:%s:%d:order=%s:source=%s", cursor, limit, order, source))
}

// MarketsListByAsset extends [MarketsListOrdered] with an asset filter — the cache key for AssetMarkets (`/v1/markets?asset=`).
func MarketsListByAsset(cursor string, limit int, order, asset string) MarketsListKey {
	return MarketsListKey(fmt.Sprintf("markets:list:%s:%d:order=%s:asset=%s", cursor, limit, order, asset))
}

// MarketsListPools extends [MarketsListOrdered] with the /v1/pools filter bundle, the key for AllPools; sources is joined unsorted, so callers must pre-sort it.
func MarketsListPools(cursor string, limit int, order string, sources []string, base, quote, asset string) MarketsListKey {
	return MarketsListKey(fmt.Sprintf("markets:list:%s:%d:order=%s:pools=1:src=%s:base=%s:quote=%s:asset=%s",
		cursor, limit, order, strings.Join(sources, ","), base, quote, asset))
}

// CatalogueListTTL is the shared TTL for both `assets:list:*` and `markets:list:*`.
const CatalogueListTTL = 60 * time.Second

// sortStrings is a tiny inline sort to avoid pulling sort into
// every cachekeys consumer.
func sortStrings(ss []string) {
	for i := 1; i < len(ss); i++ {
		for j := i; j > 0 && ss[j-1] > ss[j]; j-- {
			ss[j-1], ss[j] = ss[j], ss[j-1]
		}
	}
}

// ─── MEV detection heartbeat ─────────────────────────────────────
//
// Wire shape: `mev:heartbeat`. Value: [MEVHeartbeatFull] when the last run had the lake tx-order
// resolver (sandwich and oracle_sandwich detectors ran), [MEVHeartbeatPartial] when it ran without
// one; /v1/mev reports both partial and absent as flags.detectors_partial. Absence means no run
// completed within [MEVHeartbeatTTL], three worker ticks, so one missed tick is not a gap.

// MEVHeartbeatKey is the typed Redis key for the `mev:heartbeat` singleton.
type MEVHeartbeatKey string

// String returns the wire-format key.
func (k MEVHeartbeatKey) String() string { return string(k) }

// MEVHeartbeat returns the detection heartbeat key.
func MEVHeartbeat() MEVHeartbeatKey { return MEVHeartbeatKey("mev:heartbeat") }

// MEVHeartbeat values: whether the run had the tx-order resolver.
const (
	MEVHeartbeatFull    = "full"
	MEVHeartbeatPartial = "partial"
)

// MEVHeartbeatTTL is the heartbeat's expiry: three 5-minute worker ticks.
const MEVHeartbeatTTL = 15 * time.Minute
