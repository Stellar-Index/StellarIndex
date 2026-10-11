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
// Every key family below has its own named string type (`PriceKey`, `VWAPKey`,
// `ConfidenceKey`, ...) instead of a bare `string`, so handing a hand-rolled
// `fmt.Sprintf("price:%s", ...)` or another family's key to a Redis call is a
// compile error: Go does not implicitly convert between named string types. Call
// sites crossing into the untyped Redis wire protocol call `.String()`
// explicitly, which makes that crossing visible in the diff.
//
// This is the Redis-key analogue of the in-process `internal/api/v1.cacheKey`
// builder (same drift class: prewarm vs handler key construction diverging).
//
// Every `.String()` value is pinned byte-for-byte by keys_test.go's
// golden-string tests, so this is compile-time hardening, not a
// cache-invalidating migration.

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
// TTL: the window, bounded by [VWAPMaxAge] — see [VWAPTTL].

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

// VWAPMaxAge is the SILENCE GRACE on a published VWAP: how long a cached rolling
// VWAP may keep serving after the aggregator stops refreshing it (crash, deploy,
// OOM, Redis write failures, or a window with no trades left).
//
// It is NOT the aggregation window. Every configured (pair, window) is rewritten
// on EVERY tick (default [orchestrator.DefaultInterval] = 30 s), so an old value
// means "nobody is publishing". Keying the TTL to the window would let
// `vwap:<pair>:86400` outlive its writer by 24 hours, and /v1/price stamps
// `observed_at` at request time, so a stopped aggregator would be served as
// current with no age field to reveal it.
//
// No page precedes the expiry: customers see 404s first. The page for a dead
// publisher is `stellarindex_aggregator_silent`, which fires about 10 minutes
// after the last write while the process is still scraped and about 15 minutes
// after it when the process is gone, so the 404 window opens 5 to 10 minutes
// before anyone is paged. The freshness alert, `stellarindex_api_price_stale`, is
// a ticket and reads a gauge this same aggregator emits, so it is silent here.
// Raising this constant would serve a dead publisher's value as current;
// tighten the alert, not this grace.
const VWAPMaxAge = 5 * time.Minute

// VWAPTTL is the TTL for a VWAP key — its window, bounded by
// [VWAPMaxAge] so a value can never outlive its publisher by more than
// the silence grace. Returns 0 for zero window (callers should treat
// as "don't cache").
//
// This is the package-default grace only. A caller whose tick interval
// is not [orchestrator.DefaultInterval] — the "10 missed ticks"
// relationship above is stated in prose, not derived — should use
// [VWAPTTLWithMaxAge] instead: a deployment that raises the
// interval otherwise gets fewer missed ticks of grace, and one whose
// tick cycle exceeds VWAPMaxAge flaps between 200 and 404 for a reason
// nothing here surfaces.
func VWAPTTL(window time.Duration) time.Duration {
	return VWAPTTLWithMaxAge(window, VWAPMaxAge)
}

// VWAPTTLWithMaxAge is [VWAPTTL] with an explicit silence-grace ceiling
// in place of the package default, for a caller whose tick interval
// differs from [orchestrator.DefaultInterval].
func VWAPTTLWithMaxAge(window, maxAge time.Duration) time.Duration {
	if window <= 0 || window < maxAge {
		return window
	}
	return maxAge
}

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
// (per ADR-0018: triangulation in the serving path). When the API serves from a Redis-fallback
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

// ─── VWAP Observed-At — when the served value was observed ─────────
//
// Wire shape: `vwap:<base>:<quote>:<window-seconds>:observed_at`
// Value: RFC 3339 UTC timestamp ([FormatVWAPObservedAt]) — the closed
// bucket boundary the value's window ends at.
// TTL: matches the VWAP value key, including the freeze keep-alive.
//
// Writer: both writers of the value key (the direct per-pair refresh
// and the triangulation pass) write it in the value's MULTI/EXEC.
//
// Reader: the API stamps `observed_at` from it on every VWAP-cache
// serve. The value key alone cannot say how old it is: a freeze
// extends its TTL to cover the hold (tens of minutes) without
// rewriting it, so key existence is not freshness. A value with no
// readable stamp is not served.

// VWAPObservedAtKey is the typed Redis key for the
// `vwap:<base>:<quote>:<window>:observed_at` family.
type VWAPObservedAtKey string

// String returns the wire-format key.
func (k VWAPObservedAtKey) String() string { return string(k) }

// VWAPObservedAt returns the cache key for the observation time of the
// `vwap:<base>:<quote>:<window>` value.
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
// Wire shape: `vwap:<base>:<quote>:<window-seconds>:coverage`
// Value: [FormatVWAPCoverage] — "complete", or the RFC 3339 covered_from
// of a window whose trade read hit the per-query row cap.
// TTL: matches the VWAP value key, including the freeze keep-alive.
//
// Writer: the direct per-pair refresh writes it in the value's
// MULTI/EXEC; the triangulation pass deletes it there, because a
// composite does not track its legs' coverage.
//
// Reader: the API surfaces it as `truncated` / `covered_from`. An absent
// or unreadable key is "unknown", never "complete".

// VWAPCoverageKey is the typed Redis key for the
// `vwap:<base>:<quote>:<window>:coverage` family.
type VWAPCoverageKey string

// String returns the wire-format key.
func (k VWAPCoverageKey) String() string { return string(k) }

// VWAPCoverage returns the cache key for the window coverage of the
// `vwap:<base>:<quote>:<window>` value.
func VWAPCoverage(base, quote canonical.Asset, window time.Duration) VWAPCoverageKey {
	return VWAPCoverageKey(fmt.Sprintf("vwap:%s:%s:%d:coverage",
		base.String(), quote.String(), int(window.Seconds())))
}

// WindowCoverage is how much of its window a published VWAP was
// computed over.
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
// Wire shape: `vwap:<base>:<quote>:<window-seconds>:composite_meta`
// TTL: matches the VWAP value key.
//
// Writer: aggregator's triangulation pass writes a JSON blob
// ({path_count, combined_confidence, low_confidence, diverged,
// rerouted}) when it prices a target through the graph router. Absent = the
// pair was not priced through the router this cycle. A low_confidence
// marker is written even when the composite is NOT published over the
// direct price, so a consumer can distinguish "served direct because
// the only routes were dust" from "no composite at all".
//
// Reader: internal/api/v1's /v1/price handler decodes it via
// [DecodeCompositeMeta] on the TRIANGULATED serve path to set
// flags.diverged / flags.rerouted (best-effort: a miss / malformed
// blob leaves those flags unset), through the optional
// v1.CompositeMetaLooker capability on the wired triangulated looker.

// VWAPCompositeMetaKey is the typed Redis key for the
// `vwap:<base>:<quote>:<window>:composite_meta` family. Distinct from
// [VWAPKey] / [VWAPProvenanceKey] — sibling markers read for different
// purposes.
type VWAPCompositeMetaKey string

// String returns the wire-format key.
func (k VWAPCompositeMetaKey) String() string { return string(k) }

// VWAPCompositeMeta returns the cache key for a router-priced
// composite's quality flags on the given (pair, window).
func VWAPCompositeMeta(base, quote canonical.Asset, window time.Duration) VWAPCompositeMetaKey {
	return VWAPCompositeMetaKey(fmt.Sprintf("vwap:%s:%s:%d:composite_meta",
		base.String(), quote.String(), int(window.Seconds())))
}

// CompositeMeta is the read-side view of the JSON blob the aggregator
// writes to a [VWAPCompositeMeta] key. It models only the
// consumer-facing quality signals the /v1/price handler surfaces —
// path_count / combined_confidence / low_confidence are written by the
// aggregator but not read on this path, so they are intentionally
// omitted (json.Unmarshal ignores them). Kept deliberately separate
// from the aggregator's writer-side struct: this is the wire contract a
// reader depends on, nothing more.
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

// DecodeCompositeMeta parses the JSON blob stored under a
// [VWAPCompositeMeta] key into the reader-facing [CompositeMeta].
// Best-effort by contract: an empty or malformed payload returns the
// zero value plus a non-nil error the caller may log-and-ignore
// (absent / bad meta → quality flags unset, never a request failure).
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

// ConfidenceTTL is the TTL for a confidence: key. Delegates to
// [VWAPTTL] — the score is tied to its underlying VWAP and must expire
// with it, including under the [VWAPMaxAge] bound; a score that
// outlived the value it scored would be attached to whatever the next
// publish put there.
func ConfidenceTTL(window time.Duration) time.Duration { return VWAPTTL(window) }

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
// The rl: family is OWNED by internal/ratelimit, which builds and
// writes keys itself. No production code reads them through this
// package: the builders below pin the wire shape, and the parity test
// diffs them against a real Bucket write so a writer change is caught.
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

// RateLimitKey returns the cache key for a rate-limit counter; window
// is the fixed-window size (typically 60 s). Subject is
// url.QueryEscape'd as internal/ratelimit/bucket.go does, because IPv6
// subjects contain `:` and would otherwise collide across subjects.
func RateLimitKey(subject string, now time.Time, window time.Duration) RateLimitCounterKey {
	bucket := now.Unix() / int64(window.Seconds())
	return RateLimitCounterKey(fmt.Sprintf("rl:%s:%d", url.QueryEscape(subject), bucket))
}

// RateLimitTTL is the TTL set on rl: keys. 2× window, per ADR-0007
// (keys drain naturally; counter resets at window rollover).
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
// Wire shape: `div:<base>/<quote>`
// Value: JSON with sources compared + max deviation + threshold.
// Written by the divergence worker after each check cycle.
//
// The key is per-PAIR, not per-base-asset. The
// orchestrator's divergence refresh loops every configured pair
// (XLM/fiat:USD, XLM/fiat:EUR, XLM/fiat:GBP, …) and each one calls
// RefreshPair. A `div:<base>` key would let the last pair in
// iteration order clobber the asset's divergence result — if
// XLM/USD diverged but XLM/GBP didn't, the later XLM/GBP refresh
// would clear the warning and /v1/price for XLM/USD would serve
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

// DivergenceTTL is the floor expiry for div: keys. The aggregator's
// divergence Service extends it to its refresh cadence plus a worst-case
// pass, so a key never expires before its next write.
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
// TTL: see [FreezeTTL]. It is the SILENCE GRACE, not the freeze
// duration — the aggregator writes each marker with a TTL of
// "remaining hold + FreezeTTL" and deletes the key outright on
// unfreeze (see internal/aggregate/freeze's Policy).

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

// FreezeOverride returns the key `stellarindex-ops freeze-unfreeze`
// writes before it clears a pair's marker, so the aggregator can tell an
// operator's force-unfreeze from a marker that lapsed on its own. Under
// `freeze:` so the Redis ACL's `~freeze:*` covers it; no asset id
// parses as `override`, so it cannot collide with [Freeze]. TTL:
// [FreezeTTL], the aggregator's silence tolerance.
func FreezeOverride(asset, quote canonical.Asset) FreezeKey {
	return FreezeKey("freeze:override:" + asset.String() + ":" + quote.String())
}

// FreezeTTL is the SILENCE GRACE added to a freeze marker's
// remaining hold when the aggregator writes the key — i.e. how long
// `flags.frozen` keeps serving after the aggregator stops writing
// (crash, restart, deploy, a stalled tick).
//
// It is NOT the freeze duration: a flat TTL re-written on every fire
// would make "the freeze ends" mean "the fire condition stopped
// holding for one bucket" — a release band strictly wider than the
// fire band, on a single sample (ADR-0019). The
// duration lives in `freeze.State` (initial hold, extension
// ladder, escalation, two-consecutive-bucket auto-unfreeze) per
// ADR-0019 §"Freeze duration"; this constant only decides how much
// aggregator downtime a live freeze tolerates before the serving
// path forgets it.
//
// 5 minutes = 10 missed ticks at the default 30s cadence. Long
// enough to ride out a deploy; short enough that a genuinely dead
// aggregator does not pin a pair to a last-known-good price with
// nobody advancing the ladder.
const FreezeTTL = 5 * time.Minute

// ─── API-key records ──────────────────────────────────────────────
//
// Wire shape: `apikey:<sha256-hex>`
// Value: JSON record `{identifier, tier, scopes, expires_at?, revoked_at?}`.
// Writer: `/v1/account/keys` POST handler plus operator seeding scripts.
// Reader: `internal/auth/RedisAPIKeyValidator` on every authenticated
//         request when auth_mode=apikey.
//
// Plaintext keys are NEVER stored: the lookup hashes the caller-supplied bytes
// with SHA-256 (32-byte high-entropy keys are preimage-safe). A Redis dump leaks
// metadata but not the keys themselves.
//
// No TTL: expiry + revocation are encoded in the JSON record. An operator
// rotating keys deletes the record explicitly. No TTL does NOT protect a record
// from an allkeys-* eviction policy, and the plaintext cannot be re-issued.

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

// APIKeyTTL is the TTL for operator-issued apikey: records (self-service
// and register-mirror records carry auth.MirroredKeyIdleTTL instead).
// Zero — keys live until explicitly deleted; expiry/revocation are
// encoded in the JSON payload so the lookup can return the right error sentinel
// (ErrTokenExpired vs ErrUnauthorized). Zero is also what keeps them
// out of the instance's volatile-lru eviction pool: the plaintext is
// unrecoverable, so an evicted record is a lost credential.
const APIKeyTTL = time.Duration(0)

// APIKeyCacheKey is the typed Redis key for the `apikey-cache:<sha256-hex>`
// family: the auth_backend=postgres validator's read-through cache of
// api_keys rows, written with a finite TTL. Kept outside `apikey:` so
// no reader of canonical records (the redis-backend validator, whose
// refresh-on-use slides any TTL-bearing `apikey:` record to 90 days;
// the `apikey:*` record walk) can mistake a cache row for a credential.
type APIKeyCacheKey string

// String returns the wire-format key.
func (k APIKeyCacheKey) String() string { return string(k) }

// APIKeyCache returns the read-through cache key for keyHash, the
// hex-encoded SHA-256 of the plaintext key.
func APIKeyCache(keyHash string) APIKeyCacheKey {
	return APIKeyCacheKey("apikey-cache:" + keyHash)
}

// APIKeyCacheEvicted returns the short-lived tombstone an eviction writes
// for keyHash; while it lives the validator does not re-populate
// [APIKeyCache]. Kept under `apikey-cache:` so the same ACL rule admits it.
func APIKeyCacheEvicted(keyHash string) APIKeyCacheKey {
	return APIKeyCacheKey("apikey-cache:" + keyHash + ":evicted")
}

// ─── API-key lookup index ───
//
// Wire shape: `apikey-index:v1`, ONE Redis HASH with fields:
//
//	ready            → "1", written only by a COMPLETE index build
//	k:<key_id>       → <sha256-hex> of the record that KeyID names
//	o:<identifier>   → space-separated <sha256-hex> list of the records that owner holds
//
// Writer and reader: `internal/auth.RedisAPIKeyStore`, atomically with the record
// at issuance and by the one sanctioned keyspace walk. No TTL.
//
// Why ONE hash: under an allkeys-* policy every Redis key is independently
// evictable, and a per-owner set evicted while its records survive would make
// live credentials invisible to list, revoke and the tier clamp. Eviction of the
// one key takes the `ready` marker too, so readers fall back to the walk.
//
// NOT under `apikey:`: a HASH there would be matched by the `apikey:*` walk, whose
// GET would fail WRONGTYPE. It is a NEW pattern for the Redis ACL allow-list
// (`~apikey-index:*`); until applied, every access is NOPERM and the store walks.

// APIKeyIndexKey is the typed Redis key for the `apikey-index:*`
// family.
type APIKeyIndexKey string

// String returns the wire-format key.
func (k APIKeyIndexKey) String() string { return string(k) }

// APIKeyIndex returns the key of the single-hash lookup index over the
// `apikey:<sha256-hex>` records. The `v1` suffix versions the field
// layout: a layout change writes a new key and rebuilds instead of
// reinterpreting old fields.
func APIKeyIndex() APIKeyIndexKey { return APIKeyIndexKey("apikey-index:v1") }

// APIKeyIndexBuildLock returns the SET-NX lock that lets at most one
// process run the index build at a time. Same family as the index so
// one ACL pattern admits both.
func APIKeyIndexBuildLock() APIKeyIndexKey { return APIKeyIndexKey("apikey-index:build-lock") }

// APIKeyMintLock is the per-owner lock that serialises a capped mint's
// count and write across API instances (CreateCapped). TTL-bounded; in
// the index family so the shipped `~apikey-index:*` ACL admits it.
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

// HealthTTL is the expiry for health: keys. 60 s gives us one
// missed update before the gauge disappears.
const HealthTTL = 60 * time.Second

// ─── Oracle latest readings — read-through cache ─────────────────
//
// Wire shape: `oracle:latest:<asset-keys-joined>:<source-filter>`
// Value: JSON `{"computed_at": RFC 3339, "updates": [...]}`; a bare array
// (pre-computed_at) is read as a miss.
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

// ─── Assets list / Markets list: read-through cache ───
//
// Wire shape:
//
//	`assets:list:<cursor>:<limit>`
//	`markets:list:<cursor>:<limit>[:order=<order>[:source=<source>|:asset=<asset>|:pools=1:src=<sources>:base=<base>:quote=<quote>:asset=<asset>]]`
//
// Writer and reader: api (read-through; populated on cache miss). TTL: 60 s,
// TTL-only invalidation: both derive from a 14-day rolling window over the
// trades hypertable, and new listings surface on a minutes-to-hours timescale.
//
// `markets:list:` has several shapes because /v1/markets and /v1/pools layer
// filter dimensions on the base cursor+limit key. Each gets its own canonical
// builder below; concatenating onto [MarketsList]'s result at call sites is the
// ad-hoc-key-construction bug class this package exists to close, and against
// the typed [MarketsListKey] it is a compile error (ADR-0007).

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
