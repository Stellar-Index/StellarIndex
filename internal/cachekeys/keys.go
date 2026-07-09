package cachekeys

import (
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/StellarIndex/stellar-index/internal/canonical"
)

// ─── Typed-key mechanism (guard-debt: ROADMAP #48) ─────────────────
//
// Every key family below has its own named string type
// (`PriceKey`, `VWAPKey`, `ConfidenceKey`, …) instead of a bare
// `string`. This closes the gap the string-typed builders left
// open: a bare `string` return type means a caller can hand a
// hand-rolled `fmt.Sprintf("price:%s", ...)` — or another family's
// key — to anything that "just wants a string", and the compiler
// has no way to object. Distinct named types make that a type
// error: `redisClient.Get(ctx, someVWAPKey)` requires a `string`
// argument, and Go does NOT implicitly convert between two named
// string types (or between a named type and a plain `string`) even
// though their underlying type is identical — see
// https://go.dev/ref/spec#Assignability. Passing the wrong family,
// or a raw ad-hoc string, now needs an explicit, grep-able
// conversion instead of silently type-checking.
//
// Call sites that need the raw wire bytes (to hand to
// `redis.Cmdable`, which only knows `string`) call `.String()`
// explicitly — that's the one place per call where "this typed key
// crosses into the untyped Redis wire protocol" is visible in the
// diff.
//
// This is the Redis-key analogue of the in-process
// `internal/api/v1.cacheKey` builder (same drift class: prewarm vs.
// handler key construction diverging). See that package's doc
// comment for the sibling design.
//
// Wire bytes are UNCHANGED by this: every `.String()` value is
// byte-identical to what the pre-typed `string`-returning functions
// produced (see keys_test.go's golden-string tests) — this is a
// compile-time-only hardening, not a cache-invalidating migration.

// ─── Price — latest aggregated price per asset ────────────────────
//
// Wire shape: `price:<asset_id>`
// Writer: aggregator
// Reader: api
// TTL: 60 s (refreshed on every aggregation cycle).

// PriceKey is the typed Redis key for the `price:<asset_id>` family.
type PriceKey string

// String returns the wire-format key. Explicit conversion point for
// handing the key to a `string`-typed Redis client method.
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
// TTL matches window.

// VWAPKey is the typed Redis key for the
// `vwap:<base>:<quote>:<window-seconds>` family.
type VWAPKey string

// String returns the wire-format key.
func (k VWAPKey) String() string { return string(k) }

// VWAP returns the cache key for a rolling VWAP over window for the
// given pair.
func VWAP(base, quote canonical.Asset, window time.Duration) VWAPKey {
	return VWAPKey(fmt.Sprintf("vwap:%s:%s:%d",
		base.String(), quote.String(), int(window.Seconds())))
}

// VWAPTTL is the TTL for a VWAP key — equal to its window. Returns 0
// for zero window (callers should treat as "don't cache").
func VWAPTTL(window time.Duration) time.Duration { return window }

// ─── VWAP Provenance — was this VWAP triangulated? ──────────────────
//
// Wire shape: `vwap:<base>:<quote>:<window-seconds>:provenance`
// TTL: matches the VWAP value key.
//
// Writer: aggregator's triangulation worker writes "triangulated"
// alongside the value key. Per-pair direct refresh does NOT write
// this — absence == direct (or unknown). Reader treats empty / nil
// as "not triangulated".
//
// Used by the API's price handler to set `flags.triangulated`
// (per ADR-0018 + the "triangulation in serving path" remediation
// for audit F-0014). When the API serves from a Redis-fallback
// path (because the pair has no direct prices_1m row but has a
// triangulated implied value), it consults this key to populate
// the flag.

// VWAPProvenanceKey is the typed Redis key for the
// `vwap:<base>:<quote>:<window>:provenance` family. Deliberately a
// DISTINCT type from [VWAPKey] — the two keys are siblings written
// together but read for different purposes (value vs. marker); a
// reader that wants the marker must not be able to silently accept
// the value key or vice versa.
type VWAPProvenanceKey string

// String returns the wire-format key.
func (k VWAPProvenanceKey) String() string { return string(k) }

// VWAPProvenance returns the cache key marker for whether a
// `vwap:<base>:<quote>:<window>` value came from the triangulation
// worker (vs. the direct per-pair refresh).
func VWAPProvenance(base, quote canonical.Asset, window time.Duration) VWAPProvenanceKey {
	return VWAPProvenanceKey(fmt.Sprintf("vwap:%s:%s:%d:provenance",
		base.String(), quote.String(), int(window.Seconds())))
}

// VWAPProvenanceTriangulated is the value the triangulation worker
// stamps into the [VWAPProvenance] key. The API reader matches by
// byte-equality. This is a cache VALUE, not a key, so it stays a
// plain string (values are opaque payloads; only keys get the
// typed-family treatment).
const VWAPProvenanceTriangulated = "triangulated"

// ─── Confidence — multi-factor score per (pair, window) ───────────
//
// Wire shape: `confidence:<base>:<quote>:<window-seconds>`
// Writer: aggregator (alongside the corresponding vwap: key).
// Reader: api (`/v1/price` envelope's confidence field).
// TTL: matches the VWAP key — confidence becomes meaningless once
// the VWAP it scored expires.
//
// Value is a JSON-encoded confidence.Score (Confidence + Factors)
// rather than a bare float so the API can ship the full
// decomposition without a second lookup.

// ConfidenceKey is the typed Redis key for the
// `confidence:<base>:<quote>:<window-seconds>` family.
type ConfidenceKey string

// String returns the wire-format key.
func (k ConfidenceKey) String() string { return string(k) }

// Confidence returns the cache key for the confidence score on the
// given (pair, window).
func Confidence(base, quote canonical.Asset, window time.Duration) ConfidenceKey {
	return ConfidenceKey(fmt.Sprintf("confidence:%s:%s:%d",
		base.String(), quote.String(), int(window.Seconds())))
}

// ConfidenceTTL is the TTL for a confidence: key. Matches VWAPTTL —
// the score is tied to its underlying VWAP and should expire with it.
func ConfidenceTTL(window time.Duration) time.Duration { return window }

// ─── OHLC — one candle per (pair, granularity, bucket-start) ──────
//
// Wire shape: `ohlc:<base>:<quote>:<granularity>:<bucket-epoch>`
// Where granularity is "1m" / "15m" / "1h" / "4h" / "1d" / "1w" / "1mo"
// and bucket-epoch is the Unix seconds of the candle start.
//
// Closed candles are immutable — cached with NO TTL (CDN-pinned).
// Open candles TTL is a safety-net upper bound; in practice the
// aggregator overwrites the key on every refresh cycle (30 s for 1m,
// longer for coarser grains per migration 0002), so the cached value
// is much fresher than the TTL suggests.

// OHLCKey is the typed Redis key for the
// `ohlc:<base>:<quote>:<granularity>:<bucket-epoch>` family.
type OHLCKey string

// String returns the wire-format key.
func (k OHLCKey) String() string { return string(k) }

// OHLC returns the cache key for one OHLC candle.
func OHLC(base, quote canonical.Asset, granularity string, bucketStart time.Time) OHLCKey {
	return OHLCKey(fmt.Sprintf("ohlc:%s:%s:%s:%d",
		base.String(), quote.String(),
		granularity, bucketStart.Unix()))
}

// OHLCOpenTTL is the SAFETY-NET TTL for the currently-open candle at
// any granularity. Matches ADR-0007. The aggregator refreshes each
// candle on a cadence tied to its granularity (sub-1m; sub-15m;
// sub-1h; …), so the cached value rolls well before this TTL fires.
// The TTL exists only so that if the aggregator stops writing, stale
// open-candle data doesn't live indefinitely.
const OHLCOpenTTL = time.Hour

// OHLCClosedTTL is the TTL for a closed (historical) candle.
// Zero = no expiry (the candle is immutable; CDN pins it upstream).
const OHLCClosedTTL = time.Duration(0)

// ─── Rate-limit counters — one per (key, window) ──────────────────
//
// The rl: family is OWNED by internal/ratelimit, which writes keys
// atomically via a Lua script. The functions below are mirrors of
// that shape for read-only access (e.g. admin dashboards showing
// current usage) and CI consistency checks.
//
// Wire shape: `rl:<subject>:<window-bucket>` where subject is an
// API-key hash or IP address.

// RateLimitCounterKey is the typed Redis key for the
// `rl:<subject>:<window-bucket>` family. Named distinctly from the
// [RateLimitKey] builder function (Go doesn't allow a type and a
// func to share an identifier in the same package).
type RateLimitCounterKey string

// String returns the wire-format key.
func (k RateLimitCounterKey) String() string { return string(k) }

// RateLimitKey returns the cache key for a rate-limit counter.
// Deliberately named "...Key" not just "RateLimit" because callers
// are usually reading this for display, not as the write-path.
// window is the fixed-window size (typically 60 s).
//
// Subject is url.QueryEscape'd for parity with the writer in
// internal/ratelimit/bucket.go — IPv6 addresses contain `:` and
// without escaping two distinct subjects could land on the same
// Redis slot. Keep this in lock-step with the writer; the tests
// round-trip a sample subject to detect drift.
func RateLimitKey(subject string, now time.Time, window time.Duration) RateLimitCounterKey {
	bucket := now.Unix() / int64(window.Seconds())
	return RateLimitCounterKey(fmt.Sprintf("rl:%s:%d", url.QueryEscape(subject), bucket))
}

// RateLimitTTL is the TTL set on rl: keys. 2× window, per ADR-0007
// (keys drain naturally; counter resets at window rollover).
func RateLimitTTL(window time.Duration) time.Duration { return 2 * window }

// ─── SEP-1 / home-domain cache ────────────────────────────────────
//
// Wire shape: `toml:<home-domain>`
// Cached stellar.toml parse result. Lazy-populated by API handlers
// on miss; also invalidated when the home-domain field of a
// classic-asset record changes.

// TOMLKey is the typed Redis key for the `toml:<home-domain>` family.
type TOMLKey string

// String returns the wire-format key.
func (k TOMLKey) String() string { return string(k) }

// TOML returns the cache key for a SEP-1 home-domain record.
func TOML(homeDomain string) TOMLKey {
	return TOMLKey("toml:" + strings.ToLower(homeDomain))
}

// TOMLTTL is the expiry for toml: keys — the cached SEP-1
// `stellar.toml` overlay for /v1/assets/{id}.
//
// 24h, not minutes: a stellar.toml is issuer-controlled reference
// data (org name, currency descriptions, image URLs) that changes
// on the order of weeks-to-never. A short TTL just means every
// cold /v1/assets/{id} for a domain whose entry has aged out pays
// a fresh ~500ms upstream HTTPS fetch on the request path. A 24h
// TTL collapses that to ~once per domain per day; explicit
// busting is still available via Cache.Invalidate.
const TOMLTTL = 24 * time.Hour

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

// ─── SSE subscriber registry ──────────────────────────────────────
//
// Wire shape: `sub:<channel>:<subscriber-id>`
// Value: "1" (presence marker).
// TTL: renewed by the subscriber's heartbeat every 60 s; key expires
// 60 s after the last heartbeat.

// SubscriberKey is the typed Redis key for the
// `sub:<channel>:<subscriber-id>` family.
type SubscriberKey string

// String returns the wire-format key.
func (k SubscriberKey) String() string { return string(k) }

// Subscriber returns the cache key for an SSE subscriber presence
// marker. channel is typically a price-stream channel name; subID
// is the opaque subscriber identifier.
func Subscriber(channel, subID string) SubscriberKey {
	return SubscriberKey(fmt.Sprintf("sub:%s:%s", channel, subID))
}

// SubscriberTTL is the expiry for sub: keys — matches the
// heartbeat cadence with headroom.
const SubscriberTTL = 60 * time.Second

// ─── Divergence detector output ───────────────────────────────────
//
// Wire shape: `div:<base>/<quote>`
// Value: JSON with sources compared + max deviation + threshold.
// Written by the divergence worker after each check cycle.
//
// F-1344 (G16-03): the key is per-PAIR, not per-base-asset. The
// orchestrator's divergence refresh loops every configured pair
// (XLM/fiat:USD, XLM/fiat:EUR, XLM/fiat:GBP, …) and each one calls
// RefreshPair. The pre-fix key was `div:<base>` so the last pair in
// iteration order clobbered the asset's divergence result — if
// XLM/USD diverged but XLM/GBP didn't, the later XLM/GBP refresh
// cleared the warning and /v1/price for XLM/USD served
// divergence_warning=false. Keying by pair makes every pair's result
// independent; the by-asset API reader (DivergenceFiringFor) ORs the
// per-pair WarningFired flags via the [DivergenceBaseIndex] set so
// "firing if ANY quote diverges" holds regardless of refresh order.

// DivergenceKey is the typed Redis key for the `div:<base>/<quote>`
// family.
type DivergenceKey string

// String returns the wire-format key.
func (k DivergenceKey) String() string { return string(k) }

// Divergence returns the cache key for the latest divergence result
// for a (base, quote) pair.
func Divergence(pair canonical.Pair) DivergenceKey {
	return DivergenceKey("div:" + pair.String())
}

// DivergenceIndexKey is the typed Redis key for the `div:idx:<base>`
// family (the per-base SET of quote members). Distinct from
// [DivergenceKey] — one is a String value, the other is a Redis SET;
// a reader that expects one must not be able to silently accept the
// other's key.
type DivergenceIndexKey string

// String returns the wire-format key.
func (k DivergenceIndexKey) String() string { return string(k) }

// DivergenceBaseIndex returns the cache key for the Redis SET that
// enumerates which quote-asset strings have a live `div:<base>/<quote>`
// value for the given base. The divergence worker SADDs each quote it
// refreshes; the by-asset reader SMEMBERs this set to discover the
// per-pair keys to OR together — across processes (the aggregator
// writes, the API reads, they share only Redis), so an in-memory
// quote index on either side would not work.
//
// The set carries the same TTL as the value keys ([DivergenceTTL]),
// refreshed on every write, so a base whose pairs stop refreshing
// drains naturally rather than accumulating dead quote members.
func DivergenceBaseIndex(base canonical.Asset) DivergenceIndexKey {
	return DivergenceIndexKey("div:idx:" + base.String())
}

// DivergenceTTL is the expiry for div: keys.
const DivergenceTTL = 5 * time.Minute

// ─── Anomaly freeze marker (ADR-0019) ─────────────────────────────
//
// Wire shape: `freeze:<asset_id>:<quote_id>`
// Value: JSON with the underlying anomaly Decision (deviation_pct,
//   reason, expires_at). Presence of the key means the most-recent
//   bucket for the pair was frozen by the anomaly checker; the API
//   reads it via FrozenLooker to set flags.frozen=true.
//
// Writer: aggregator orchestrator at bucket-close, when
// anomaly.Checker.Evaluate returns ActionFreeze.
// Reader: internal/api/v1.FrozenLooker — production wiring is the
// freeze package's RedisLooker.
//
// TTL: 5 minutes — long enough that the next bucket close (1
// minute) sees the marker still in place if the anomaly persists,
// short enough that a transient anomaly clears within a few buckets
// of the underlying signal returning to normal.

// FreezeKey is the typed Redis key for the
// `freeze:<asset_id>:<quote_id>` family.
type FreezeKey string

// String returns the wire-format key.
func (k FreezeKey) String() string { return string(k) }

// Freeze returns the cache key for the freeze marker on an
// (asset, quote) pair. The marker's presence drives flags.frozen
// on /v1/price; the value carries diagnostic context (which class
// thresholds fired, observed deviation, last-known-good price).
func Freeze(asset, quote canonical.Asset) FreezeKey {
	return FreezeKey("freeze:" + asset.String() + ":" + quote.String())
}

// FreezeTTL is the expiry for freeze: keys.
const FreezeTTL = 5 * time.Minute

// ─── API-key records ──────────────────────────────────────────────
//
// Wire shape: `apikey:<sha256-hex>`
// Value: JSON record `{identifier, tier, scopes, expires_at?, revoked_at?}`.
// Writer: `/v1/account/keys` POST handler (self-service key
//         issuance) plus operator seeding scripts.
// Reader: `internal/auth/RedisAPIKeyValidator` on every authenticated
//         request when auth_mode=apikey.
//
// Plaintext keys are NEVER stored — the lookup hashes the
// caller-supplied bytes with SHA-256 (32-byte high-entropy keys are
// preimage-safe; HMAC with a server pepper is a future hardening if
// keys are ever shorter or operator-set). A Redis dump leaks
// metadata but not the keys themselves.
//
// No TTL: API keys are long-lived; expiry + revocation are encoded
// in the JSON record, not at the Redis layer. An operator rotating
// keys deletes the record explicitly.

// APIKeyRecordKey is the typed Redis key for the
// `apikey:<sha256-hex>` family. Named distinctly from the [APIKey]
// builder function (Go doesn't allow a type and a func to share an
// identifier in the same package).
type APIKeyRecordKey string

// String returns the wire-format key.
func (k APIKeyRecordKey) String() string { return string(k) }

// APIKey returns the cache key for the API-key record identified by
// keyHash. keyHash MUST be hex-encoded SHA-256 of the plaintext key
// (the auth package does the hashing — callers don't construct this
// directly except in admin tooling that already has the hash), with
// one documented exception: callers doing a Redis SCAN pass a literal
// "*" glob (e.g. `APIKey("*")`) to build the `apikey:*` match pattern
// — SCAN's `match` argument shares the same wire-string type as a
// concrete key.
func APIKey(keyHash string) APIKeyRecordKey {
	return APIKeyRecordKey("apikey:" + keyHash)
}

// APIKeyTTL is the TTL for apikey: records. Zero — keys live until
// explicitly deleted; expiry/revocation are encoded in the JSON
// payload so the lookup can return the right error sentinel
// (ErrTokenExpired vs ErrUnauthorized).
const APIKeyTTL = time.Duration(0)

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

// HealthTTL is the expiry for health: keys. 60 s gives us one
// missed update before the gauge disappears.
const HealthTTL = 60 * time.Second

// ─── Oracle latest readings — read-through cache ─────────────────
//
// Wire shape: `oracle:latest:<asset-keys-joined>:<source-filter>`
// Writer: api (read-through; populated on cache miss)
// Reader: api
// TTL: 30 s — Reflector / Band / RedStone push every 1–5 minutes;
// a 30 s cached entry stays inside one push interval, so customers
// see fresh readings without paying the 285–580 ms full DISTINCT ON
// (source) sort on every poll.
//
// Asset list is sorted before joining so the same logical query
// hits the same cache key regardless of input order — the v1
// handler always passes [user-asset, translated-asset] but the
// hash is order-independent.

// OracleLatestKey is the typed Redis key for the
// `oracle:latest:<asset-keys-joined>:<source-filter>` family.
type OracleLatestKey string

// String returns the wire-format key.
func (k OracleLatestKey) String() string { return string(k) }

// OracleLatest returns the cache key for the multi-source latest
// reading of a deduped, sorted list of asset keys with optional
// source filter (empty = "every source").
func OracleLatest(assetKeys []string, sourceFilter string) OracleLatestKey {
	// Defensive copy + sort so the cache key is stable regardless
	// of the caller's argument order. The `|` separator can't
	// appear in a canonical asset_id (G-strkey + base32 alphabet,
	// `:` for class prefixes, contract `C…`).
	sorted := append([]string(nil), assetKeys...)
	sortStrings(sorted)
	return OracleLatestKey("oracle:latest:" + strings.Join(sorted, "|") + ":" + sourceFilter)
}

// OracleLatestTTL is the TTL for `oracle:latest:*` cache entries.
const OracleLatestTTL = 30 * time.Second

// ─── Assets list / Markets list — read-through cache ──────────────
//
// Wire shape:
//   `assets:list:<cursor>:<limit>`
//   `markets:list:<cursor>:<limit>[:order=<order>[:source=<source>|:asset=<asset>|:pools=1:src=<sources>:base=<base>:quote=<quote>:asset=<asset>]]`
//
// Writer: api (read-through; populated on cache miss)
// Reader: api
// TTL: 60 s — both endpoints derive from a 14-day rolling
// window over the trades hypertable; new assets/pairs appear on
// the human timescale of new listings (minutes-to-hours), so a
// 60 s entry stays well inside the human freshness expectation.
//
// Invalidation: TTL only — no explicit invalidation on insert.
// 60 s of staleness on a "new asset just got its first trade"
// is acceptable; a fresh listing isn't expected to surface
// instantly.
//
// The `markets:list:` family has more than one shape because
// /v1/markets and /v1/pools layer additional filter dimensions
// (sort order, source, asset, the /v1/pools filter bundle) on top
// of the base cursor+limit key. Each dimension gets its own
// canonical builder below rather than call sites hand-appending
// `":order=" + foo` onto [MarketsList]'s result — string
// concatenation onto a canonical builder's output is exactly the
// ad-hoc-key-construction bug class this package exists to close
// (it used to compile silently against the old `string` return
// type; against the typed [MarketsListKey] it's a compile error,
// which is what caught this during the ADR-0007 typed-key migration).

// AssetsListKey is the typed Redis key for the
// `assets:list:<cursor>:<limit>` family.
type AssetsListKey string

// String returns the wire-format key.
func (k AssetsListKey) String() string { return string(k) }

// AssetsList returns the cache key for one page of /v1/assets.
func AssetsList(cursor string, limit int) AssetsListKey {
	return AssetsListKey(fmt.Sprintf("assets:list:%s:%d", cursor, limit))
}

// MarketsListKey is the typed Redis key for the `markets:list:*`
// family — see the family-level doc comment above for the shapes
// [MarketsList], [MarketsListOrdered], [MarketsListBySource],
// [MarketsListByAsset], and [MarketsListPools] each produce.
type MarketsListKey string

// String returns the wire-format key.
func (k MarketsListKey) String() string { return string(k) }

// MarketsList returns the base cache key for one page of
// /v1/markets or /v1/pools with no further filter dimensions. Most
// callers want one of the more specific builders below instead —
// this is the shared prefix they all build on.
func MarketsList(cursor string, limit int) MarketsListKey {
	return MarketsListKey(fmt.Sprintf("markets:list:%s:%d", cursor, limit))
}

// MarketsListOrdered extends [MarketsList] with the sort-order
// dimension used by DistinctPairsExt's cache wrapper. order is the
// caller's own string discriminator for the timescale.MarketsOrder
// enum — cachekeys stays free of a storage-layer dependency (same
// rationale as internal/api/v1/cachekey.go's [cacheKey.order]), so
// callers convert their enum to a string before calling this.
func MarketsListOrdered(cursor string, limit int, order string) MarketsListKey {
	return MarketsListKey(fmt.Sprintf("markets:list:%s:%d:order=%s", cursor, limit, order))
}

// MarketsListBySource extends [MarketsListOrdered] with a source
// filter — the cache key for SourceMarkets (`/v1/markets?source=`).
func MarketsListBySource(cursor string, limit int, order, source string) MarketsListKey {
	return MarketsListKey(fmt.Sprintf("markets:list:%s:%d:order=%s:source=%s", cursor, limit, order, source))
}

// MarketsListByAsset extends [MarketsListOrdered] with an asset
// filter — the cache key for AssetMarkets (`/v1/markets?asset=`).
func MarketsListByAsset(cursor string, limit int, order, asset string) MarketsListKey {
	return MarketsListKey(fmt.Sprintf("markets:list:%s:%d:order=%s:asset=%s", cursor, limit, order, asset))
}

// MarketsListPools extends [MarketsListOrdered] with the /v1/pools
// filter bundle (DEX-source set + base/quote/asset pair filter) —
// the cache key for AllPools. sources is joined with `,` (not
// sorted — callers that need order-independence, e.g. a prewarm
// pass and a handler that could theoretically build the slice in a
// different order, must pass a pre-sorted slice; today's only
// caller passes the same fixed `v1.DexSourceNames()` / single-source
// slice on both the prewarm and handler paths, so this mirrors the
// pre-typed-key behavior exactly).
func MarketsListPools(cursor string, limit int, order string, sources []string, base, quote, asset string) MarketsListKey {
	return MarketsListKey(fmt.Sprintf("markets:list:%s:%d:order=%s:pools=1:src=%s:base=%s:quote=%s:asset=%s",
		cursor, limit, order, strings.Join(sources, ","), base, quote, asset))
}

// CatalogueListTTL is the shared TTL for both `assets:list:*` and
// `markets:list:*`. Tighter than the underlying 14-day window but
// loose enough to absorb polling fan-out.
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
