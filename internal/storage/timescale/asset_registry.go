package timescale

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// assetRegistryDedupeTTL throttles per-asset upserts so the
// classic_assets row's `last_seen_*` + `observation_count` keep
// advancing while still bounding DB pressure. F-1243 (codex
// audit-2026-05-12): the prior `sync.Map` of asset_id → struct{}
// short-circuited every subsequent trade in the same process,
// leaving the row frozen at first observation. A coarse TTL
// caps the upsert rate to one per asset per window while
// guaranteeing the row advances under sustained trading.
//
// 60 seconds is generous enough to keep the indexer hot path
// out of the registry table (one upsert per asset per minute
// is trivial Postgres load even at 1000s of assets) and tight
// enough that the dashboard's "last seen" column always reflects
// activity within the last minute.
const assetRegistryDedupeTTL = 60 * time.Second

// assetRegistryDedupe is a process-lifetime cache of (asset_id →
// last successful upsert time). The next trade for the same
// asset within `assetRegistryDedupeTTL` skips the DB round-trip;
// trades outside the window upsert again so `last_seen_*` +
// `observation_count` advance. F-1243 (codex audit-2026-05-12).
//
// Process-lifetime is intentional: the indexer restarts often
// enough that we'll re-touch the row periodically. A persistent
// cross-process cache (Redis) would be over-engineering — the
// upsert is idempotent and the TTL bounds the worst-case load.
var assetRegistryDedupe sync.Map // key: asset_id (string) → time.Time (last upsert)

// issuerRegistryDedupe stays as a process-lifetime sentinel
// cache — the issuers table has no `last_seen` columns so
// there's nothing to advance after the first INSERT. The
// per-row DDL difference from classic_assets is documented in
// migrations 0023 (classic_assets) vs 0022 (issuers).
var issuerRegistryDedupe sync.Map

// registerClassicAssetSeen ensures a `classic_assets` row exists
// for the supplied classic asset, with last_seen_* bumped to the
// trade's ledger + timestamp and observation_count incremented.
//
// Hooked into InsertTrade after the trades-table INSERT succeeds.
// Per migration 0023's docblock, classic_assets is supposed to be
// "auto-populated by an observer (Phase 4) that hooks every trade
// + every ChangeTrust op + every payment-crossing-an-issuer op".
// This is the trade-side half — the only path the indexer reliably
// hits. ChangeTrust + payment hooks land later if needed.
//
// Returns nil error on any of: success, no-op (asset is non-
// classic), already-deduped within this process. Errors are
// logged-and-swallowed at the caller so a registry write failure
// can't sink the trade insert.
func (s *Store) registerClassicAssetSeen(
	ctx context.Context,
	asset canonical.Asset,
	ledger uint32,
	observedAt time.Time,
) error {
	if asset.Type != canonical.AssetClassic {
		return nil
	}
	assetID := asset.String()
	// F-1243 (codex audit-2026-05-12): TTL-based dedupe. The
	// prior `sync.Map` of bare sentinels froze the row at first
	// observation; now we only skip the upsert when the last
	// successful one was within `assetRegistryDedupeTTL`. Out-of-
	// window trades fire the upsert again so `last_seen_*` and
	// `observation_count` advance.
	if shouldSkipAssetRegistryUpsert(assetID, time.Now()) {
		return nil
	}

	// Issuer first — issuers row has no FK from classic_assets but
	// keeping the order consistent makes it easier to reason about
	// race-free reads from the API: every classic_assets row has a
	// matching issuers row by the time it's queryable.
	if err := s.registerIssuerSeen(ctx, asset.Issuer); err != nil {
		return err
	}

	// first_seen_* uses LEAST so chunked / parallel backfill that
	// processes ledgers out of order cannot leave a higher value
	// behind. Without this, replaying an older window after the
	// row already exists would leave first_seen_ledger pinned at
	// the original (later) ledger — wrong by definition. F-1239.
	//
	// slug (2026-08-04, migration 0134): NEW rows are stamped with the
	// disambiguated form migration 0023 documented but never
	// implemented — lower(code)-<first 8 of issuer> — instead of the
	// literal NULL this INSERT bound for its whole life. The NULL made
	// every reader's COALESCE(slug, code) serve the bare CODE as the
	// public slug, so 5+ unrelated issuers of "USDT" all published the
	// SAME slug and the explorer's slug-keyed cache crowned an
	// arbitrary winner as /assets/USDT (2026-08-04 identity incident).
	// ON CONFLICT the EXISTING slug is kept — the backfill migration
	// owns historical rows, and a slug is a public URL: it must never
	// silently change once issued.
	const q = `
		INSERT INTO classic_assets (
			asset_id, code, issuer_g_strkey, slug,
			first_seen_at, first_seen_ledger,
			last_seen_at,  last_seen_ledger,
			observation_count
		) VALUES (
			$1, $2, $3, lower($2) || '-' || lower(left($3, 8)),
			$4, $5, $4, $5, 1
		)
		ON CONFLICT (asset_id) DO UPDATE SET
			first_seen_at     = LEAST(classic_assets.first_seen_at, EXCLUDED.first_seen_at),
			first_seen_ledger = LEAST(classic_assets.first_seen_ledger, EXCLUDED.first_seen_ledger),
			last_seen_at      = GREATEST(classic_assets.last_seen_at, EXCLUDED.last_seen_at),
			last_seen_ledger  = GREATEST(classic_assets.last_seen_ledger, EXCLUDED.last_seen_ledger),
			observation_count = classic_assets.observation_count + 1
	`
	if _, err := s.db.ExecContext(ctx, q,
		assetID, asset.Code, asset.Issuer,
		observedAt.UTC(), int(ledger),
	); err != nil {
		// slug is UNIQUE, and the 8-char issuer prefix leaves an
		// astronomically small (but non-zero) same-code collision
		// window. A collision must not sink trade ingest: retry once
		// with the fully-qualified asset_id as the slug — unique by
		// construction (it IS the primary key's value), same tier-3
		// fallback the 0134 backfill uses.
		if isUniqueViolation(err) {
			const qFull = `
				INSERT INTO classic_assets (
					asset_id, code, issuer_g_strkey, slug,
					first_seen_at, first_seen_ledger,
					last_seen_at,  last_seen_ledger,
					observation_count
				) VALUES (
					$1, $2, $3, $1,
					$4, $5, $4, $5, 1
				)
				ON CONFLICT (asset_id) DO UPDATE SET
					first_seen_at     = LEAST(classic_assets.first_seen_at, EXCLUDED.first_seen_at),
					first_seen_ledger = LEAST(classic_assets.first_seen_ledger, EXCLUDED.first_seen_ledger),
					last_seen_at      = GREATEST(classic_assets.last_seen_at, EXCLUDED.last_seen_at),
					last_seen_ledger  = GREATEST(classic_assets.last_seen_ledger, EXCLUDED.last_seen_ledger),
					observation_count = classic_assets.observation_count + 1
			`
			if _, rerr := s.db.ExecContext(ctx, qFull,
				assetID, asset.Code, asset.Issuer,
				observedAt.UTC(), int(ledger),
			); rerr != nil {
				return fmt.Errorf("timescale: registerClassicAssetSeen %s (full-slug retry): %w", assetID, rerr)
			}
		} else {
			return fmt.Errorf("timescale: registerClassicAssetSeen %s: %w", assetID, err)
		}
	}
	assetRegistryDedupe.Store(assetID, time.Now())
	return nil
}

// ResetAssetRegistryDedupeForTest clears the process-lifetime
// dedupe cache used by [Store.registerClassicAssetSeen]. Used by
// the F-1243 (codex audit-2026-05-13) duplicate-replay integration
// proof to simulate a process restart between an original trade
// insert and a replay of the same trade — the test asserts that
// the registry row's `observation_count` does NOT advance on the
// replay because the [Store.InsertTrade] `RowsAffected == 0` guard
// short-circuits the registry hook even with a cold dedupe cache.
//
// Production code never calls this; it only exists so the
// integration test can isolate the RowsAffected guard from the
// in-process TTL cache that would otherwise mask a regression.
func ResetAssetRegistryDedupeForTest() {
	assetRegistryDedupe.Range(func(k, _ any) bool {
		assetRegistryDedupe.Delete(k)
		return true
	})
	issuerRegistryDedupe.Range(func(k, _ any) bool {
		issuerRegistryDedupe.Delete(k)
		return true
	})
}

// shouldSkipAssetRegistryUpsert returns true when `now` falls
// within `assetRegistryDedupeTTL` of the last recorded upsert
// for `assetID`. Returns false on no-cache (first time) and on
// expired-cache (TTL elapsed). Extracted as a pure function so
// the F-1243 TTL-gate semantics can be unit-tested without
// standing up a Postgres container.
func shouldSkipAssetRegistryUpsert(assetID string, now time.Time) bool {
	cached, ok := assetRegistryDedupe.Load(assetID)
	if !ok {
		return false
	}
	lastUpsert, ok := cached.(time.Time)
	if !ok {
		return false
	}
	return now.Sub(lastUpsert) < assetRegistryDedupeTTL
}

// registerIssuerSeen ensures a row exists in the `issuers` table
// for the supplied G-strkey. Idempotent + dedupe-cached.
//
// Only writes the g_strkey field — home_domain, auth flags, and
// SEP-1 payload come from a separate AccountEntry observer (per
// ADR-0021) which already exists for operator-configured watched
// accounts. Without that observer running, the curated
// known-issuer fallback at internal/api/v1/known_issuers.go fills
// home_domain + org_name at the wire boundary for the top
// anchors.
func (s *Store) registerIssuerSeen(ctx context.Context, gStrkey string) error {
	if gStrkey == "" {
		return nil
	}
	if _, seen := issuerRegistryDedupe.Load(gStrkey); seen {
		return nil
	}
	const q = `
		INSERT INTO issuers (g_strkey)
		VALUES ($1)
		ON CONFLICT (g_strkey) DO NOTHING
	`
	if _, err := s.db.ExecContext(ctx, q, gStrkey); err != nil {
		return fmt.Errorf("timescale: registerIssuerSeen %s: %w", gStrkey, err)
	}
	issuerRegistryDedupe.Store(gStrkey, struct{}{})
	return nil
}

// ClassicAssetBySlug resolves a PUBLIC slug (populated by migration
// 0134 + stamped on new rows above) to its classic (code, issuer)
// identity. Exact match wins over the case-folded match — tier-3
// fallback slugs are the verbatim asset_id and therefore mixed-case,
// while tier-1/2 slugs are lowercase; a URL may arrive in either
// casing. ok=false when no row carries the slug (not an error).
func (s *Store) ClassicAssetBySlug(ctx context.Context, slug string) (code, issuer string, ok bool, err error) {
	const q = `
		SELECT code, issuer_g_strkey
		  FROM classic_assets
		 WHERE slug = $1 OR slug = lower($1)
		 ORDER BY (slug = $1) DESC
		 LIMIT 1
	`
	err = s.db.QueryRowContext(ctx, q, slug).Scan(&code, &issuer)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", false, nil
	}
	if err != nil {
		return "", "", false, fmt.Errorf("timescale: ClassicAssetBySlug %q: %w", slug, err)
	}
	return code, issuer, true, nil
}
