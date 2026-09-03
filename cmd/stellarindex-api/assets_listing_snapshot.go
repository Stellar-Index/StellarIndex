package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"time"

	"github.com/redis/go-redis/v9"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// /v1/assets listing snapshots — the boot seed for #459.
//
// The prewarm cannot win the startup race. Re-measured on r1 at the
// 2026-09-03 03:50:22 restart (v0.58.0): "starting" 03:50:22.985 →
// "http listening" 03:50:22.997 (+12 ms) → first /v1/assets listing
// request at +3.5 s → first browser request at +4.0 s, answered
// 11,658 ms later. Warming faster does not help, because the cost IS
// the first fill of a cold slot and traffic arrives seconds before any
// fill can finish.
//
// So the last-good page-set outlives the process. Every prewarm cycle
// writes each warmed listing key here; the next boot reads them back
// and seeds [v1.CachedAssetsReader] BEFORE the listener starts, so the
// first request takes the cache's stale-while-revalidate branch —
// instant answer, honest `flags.stale`, one detached refresh behind it
// — instead of blocking on the ~11 s aggregate.
//
// KEY GRAMMAR. `internal/cachekeys` is the canonical home for Redis key
// families (ADR-0007) and has no builder for this shape (its
// `assets:list:<cursor>:<limit>` family belongs to a different reader,
// carries a different payload type, and has no `order` dimension). This
// family is therefore declared here, with the binary that is its sole
// reader and writer — the same narrowly-scoped, package-owned shape
// `internal/ratelimit` ("rl:") and `internal/usage` ("usage:") use, and
// which cachekeys' own package doc records as an accepted exception. It
// keeps ADR-0007's actual guarantees: a named key type that will not
// implicitly convert to another family's, a single builder (no
// concatenation at call sites), and a golden-string test. Promoting it
// to internal/cachekeys is the right move the moment a second binary
// needs to read it.

// assetsListingSnapshotKey is the typed Redis key for the
// `assets:listing-snapshot:<schema>:<order>:<limit>` family.
type assetsListingSnapshotKey string

// String returns the wire-format key.
func (k assetsListingSnapshotKey) String() string { return string(k) }

// assetsListingSnapshotRedisKey returns the snapshot key for one
// warmed listing variant.
//
// The schema fingerprint is part of the KEY, not the payload, so a
// binary whose [timescale.AssetRow] shape differs from the writer's
// simply finds nothing and falls back to the cold path. Encoding it in
// the payload instead would mean decoding a row-set built from
// different fields to discover that — and a field added since the
// snapshot was written would otherwise deserialise silently to its zero
// value and be served as data. Superseded keys cost nothing: they expire
// on [assetsListingSnapshotTTL].
func assetsListingSnapshotRedisKey(limit int, order timescale.AssetsOrder) assetsListingSnapshotKey {
	return assetsListingSnapshotKey(fmt.Sprintf("assets:listing-snapshot:%s:%d:%d",
		assetRowSchemaFingerprint(), int(order), limit))
}

// assetsListingSnapshotTTL bounds how long a snapshot survives in
// Redis. Comfortably longer than [assetsListingSeedMaxAge] on purpose:
// the age check is the authoritative freshness gate, and letting the
// key outlive it means an over-age snapshot is REFUSED with a log line
// rather than vanishing into an indistinguishable cache miss.
const assetsListingSnapshotTTL = 30 * time.Minute

// assetsListingSeedMaxAge is the oldest page-set the boot seed will
// publish. Beyond it the seed is refused and the request pays the cold
// query, which is today's behaviour.
//
// Why 10 minutes: these rows carry price_usd, volume_24h_usd and the
// change pills, so the bound has to be a freshness decision, not a
// convenience one. 10 min is the longest horizon THIS handler already
// accepts for its own enrichment reads (classicSupplyTTL,
// sep1ImagesTTL are both 10 min), so a seeded page-set is never older
// than data the same response already carries. It is also ~10x the
// prewarm cadence and 5x the listing TTL, so it covers every realistic
// restart — a deploy takes seconds, and the measured snapshot age at
// boot is bounded by one prewarm cycle plus downtime — while refusing
// to republish valuations from a genuinely long outage.
const assetsListingSeedMaxAge = 10 * time.Minute

// assetsListingSnapshotBudget bounds the whole boot-seed read. It runs
// on the startup path ahead of ListenAndServe, so it must be incapable
// of delaying the listener meaningfully: a local Redis answers a dozen
// GETs in single-digit milliseconds, and anything slower is not worth
// the wait when the fallback is merely today's behaviour.
const assetsListingSnapshotBudget = 2 * time.Second

// assetsListingSnapshot is the persisted page-set.
//
// ObservedAt is the time the rows were READ FROM UPSTREAM, never the
// time they were written here. That distinction is what keeps the seed
// honest across generations: when the prewarm reads a stale entry it
// re-persists the same rows with the same ObservedAt, so a snapshot's
// age can only be reset by a real refresh, and a page-set cannot be
// laundered fresh by being copied forward.
type assetsListingSnapshot struct {
	ObservedAt time.Time            `json:"observed_at"`
	Rows       []timescale.AssetRow `json:"rows"`
}

// assetsListingSnapshots reads and writes [assetsListingSnapshot]s. A
// nil receiver, or one with a nil client, is a no-op on both sides —
// Redis is optional in this binary and its absence must degrade to the
// pre-#459 cold-fill behaviour, never to an error or an empty listing.
type assetsListingSnapshots struct {
	rdb redis.UniversalClient
	log *slog.Logger
}

// newAssetsListingSnapshots returns a store, or nil when Redis is not
// configured.
func newAssetsListingSnapshots(rdb redis.UniversalClient, log *slog.Logger) *assetsListingSnapshots {
	if rdb == nil {
		return nil
	}
	return &assetsListingSnapshots{rdb: rdb, log: log}
}

// save persists one warmed listing variant. Empty row-sets and
// undated reads are skipped: an entry we cannot date cannot be
// labelled honestly at serve time, and an empty page-set must never be
// able to displace a real cold fill.
func (s *assetsListingSnapshots) save(
	ctx context.Context, opts timescale.ListAssetsOptions, rows []timescale.AssetRow, observedAt time.Time,
) {
	if s == nil || s.rdb == nil || len(rows) == 0 || observedAt.IsZero() {
		return
	}
	key := assetsListingSnapshotRedisKey(opts.Limit, opts.Order)
	buf, err := json.Marshal(assetsListingSnapshot{ObservedAt: observedAt, Rows: rows})
	if err != nil {
		s.log.Warn("assets listing snapshot encode failed", "key", key.String(), "err", err)
		return
	}
	if err := s.rdb.Set(ctx, key.String(), buf, assetsListingSnapshotTTL).Err(); err != nil {
		s.log.Warn("assets listing snapshot write failed", "key", key.String(), "err", err)
	}
}

// load returns the persisted page-set for one variant. ok=false covers
// every degraded case — no client, key absent, Redis unreachable,
// undecodable payload — and each of them means "fall back to the cold
// fill", never an error to the caller.
func (s *assetsListingSnapshots) load(
	ctx context.Context, opts timescale.ListAssetsOptions,
) (assetsListingSnapshot, bool) {
	if s == nil || s.rdb == nil {
		return assetsListingSnapshot{}, false
	}
	key := assetsListingSnapshotRedisKey(opts.Limit, opts.Order)
	raw, err := s.rdb.Get(ctx, key.String()).Bytes()
	if err != nil {
		if !errors.Is(err, redis.Nil) {
			s.log.Warn("assets listing snapshot read failed", "key", key.String(), "err", err)
		}
		return assetsListingSnapshot{}, false
	}
	var snap assetsListingSnapshot
	if err := json.Unmarshal(raw, &snap); err != nil {
		s.log.Warn("assets listing snapshot decode failed", "key", key.String(), "err", err)
		return assetsListingSnapshot{}, false
	}
	return snap, true
}

// seedAssetListingsFromSnapshots primes the in-process listing cache
// from the previous process's snapshots, and reports how many of the
// requested variants were seeded.
//
// Call this BEFORE the HTTP listener starts: on r1 the listener is up
// 12 ms after "starting" and human traffic arrives at +3.5 s, so a seed
// that races the listener is a seed that loses. It is bounded by
// [assetsListingSnapshotBudget] precisely so it can be synchronous
// there.
//
// It seeds exactly [assetListingPrewarmOptions] — the same list the
// prewarm warms and the same one save() is driven from — so the seed
// cannot address a variant the prewarm does not maintain, and the
// handler's key derivation is shared with the seed
// (v1.CachedAssetsReader.SeedListing → listAssetsCacheKey).
//
// Returns (seeded, requested) so the caller can log a self-accounting
// line: zero seeded out of a non-empty request set is a real signal
// (cold Redis, first deploy, schema change, or a genuinely broken
// path), not silence.
func seedAssetListingsFromSnapshots(
	ctx context.Context, logger *slog.Logger, reader *v1.CachedAssetsReader, snaps *assetsListingSnapshots,
) (seeded, requested int) {
	opts := assetListingPrewarmOptions()
	if reader == nil || snaps == nil {
		return 0, len(opts)
	}
	now := time.Now()
	var skippedOld int
	for _, o := range opts {
		snap, ok := snaps.load(ctx, o)
		if !ok {
			continue
		}
		if age := now.Sub(snap.ObservedAt); age > assetsListingSeedMaxAge || age < 0 {
			skippedOld++
			continue
		}
		if reader.SeedListing(o, snap.Rows, snap.ObservedAt) {
			seeded++
		}
	}
	if skippedOld > 0 {
		logger.Info("assets listing boot seed: snapshots refused as over-age",
			"count", skippedOld, "max_age", assetsListingSeedMaxAge.String())
	}
	return seeded, len(opts)
}

// assetRowSchemaFingerprint is a stable digest of [timescale.AssetRow]'s
// exported shape (field names + types, in declaration order).
//
// It exists because the snapshot is JSON and JSON is forgiving: a field
// ADDED to AssetRow since a snapshot was written would deserialise to
// its zero value and be served as though it were data — a null
// market_cap or a zeroed source_count presented as observation. Keying
// on the shape makes that unrepresentable: a binary with a different
// AssetRow reads a different key, finds nothing, and pays the cold fill.
//
// Computed once; the result is a build-invariant of the binary.
var assetRowSchemaFingerprint = func() func() string {
	t := reflect.TypeOf(timescale.AssetRow{})
	h := sha256.New()
	for i := range t.NumField() {
		f := t.Field(i)
		_, _ = fmt.Fprintf(h, "%s:%s\n", f.Name, f.Type.String())
	}
	fp := hex.EncodeToString(h.Sum(nil))[:12]
	return func() string { return fp }
}()
