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

// /v1/assets listing snapshots — the boot seed.
//
// The prewarm cannot win the startup race, so each prewarm cycle writes its last-good page-set here
// and the next boot seeds [v1.CachedAssetsReader] before the listener starts; the first request
// then takes the stale-while-revalidate branch with an honest `flags.stale`. The key family lives
// here, not in internal/cachekeys (ADR-0007), because it has a sole reader and writer; promote it
// when a second binary needs it.

// assetsListingSnapshotKey is the typed Redis key for the
// `assets:listing-snapshot:<schema>:<order>:<limit>` family.
type assetsListingSnapshotKey string

// String returns the wire-format key.
func (k assetsListingSnapshotKey) String() string { return string(k) }

// assetsListingSnapshotRedisKey returns the snapshot key for one warmed listing variant. The schema fingerprint is in the key so a
// different [timescale.AssetRow] shape finds nothing instead of decoding added fields as zeros.
func assetsListingSnapshotRedisKey(limit int, order timescale.AssetsOrder) assetsListingSnapshotKey {
	return assetsListingSnapshotKey(fmt.Sprintf("assets:listing-snapshot:%s:%d:%d",
		assetRowSchemaFingerprint(), int(order), limit))
}

// assetsListingSnapshotTTL outlives [assetsListingSeedMaxAge] on purpose, so an over-age snapshot
// is refused with a log line instead of vanishing as an indistinguishable cache miss.
const assetsListingSnapshotTTL = 30 * time.Minute

// assetsListingSeedMaxAge is the oldest page-set the boot seed will publish; beyond it the
// request pays the cold query. The rows carry prices and volumes, so 10 minutes matches the
// handler's own enrichment horizon and refuses to republish valuations from a long outage.
const assetsListingSeedMaxAge = 10 * time.Minute

// assetsListingSnapshotBudget bounds the boot-seed read, which runs ahead of ListenAndServe and
// must not delay the listener; the fallback is only the cold path.
const assetsListingSnapshotBudget = 2 * time.Second

// assetsListingSnapshot is the persisted page-set. ObservedAt is when the rows were read upstream, never when written; a stale entry
// keeps it so only a real refresh resets its age.
type assetsListingSnapshot struct {
	ObservedAt time.Time            `json:"observed_at"`
	Rows       []timescale.AssetRow `json:"rows"`
}

// assetsListingSnapshots reads and writes [assetsListingSnapshot]s. A nil receiver or client is a
// no-op on both sides: Redis is optional, and its absence must degrade to the cold fill, never to
// an error or an empty listing.
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

// save persists one warmed listing variant. Empty row-sets and undated reads are skipped: an
// undatable entry cannot be labelled honestly at serve time, and an empty page-set must never
// displace a real cold fill.
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

// load returns the persisted page-set for one variant. ok=false covers every degraded case (no
// client, key absent, Redis unreachable, undecodable payload) and means "fall back to the cold
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

// seedAssetListingsFromSnapshots primes the listing cache from the previous process's snapshots and returns the seeded count. Call it
// before the listener starts (a racing seed loses); zero seeded from a non-empty request set is a real signal.
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

// assetRowSchemaFingerprint digests [timescale.AssetRow]'s exported shape: JSON would serve an added field as a zero value, so a changed
// shape must key a different entry. Computed once per build.
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
