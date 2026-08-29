package timescale

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
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

// assetRegistryDedupe / issuerRegistryDedupe live on [Store] (not at
// package scope) so the cache is scoped to ONE database. A
// package-level map is keyed by asset_id alone, which is only unique
// within a database: two Stores over different DBs in one process
// (every integration-test container; a future multi-network binary)
// shared one cache, so the second DB's first trade for an asset was
// "deduped" and its classic_assets row never written — the
// TestAssetsReader `HasAsset(USDC) = false` flake (2026-08-28). The
// zero value is ready to use, so the `&Store{db: db}` constructors
// need no wiring. See the field docs on [Store].

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
	if s.shouldSkipAssetRegistryUpsert(assetID, time.Now()) {
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
	// slug (migration 0135, 2026-08-05): the fully-qualified asset_id,
	// verbatim. 0134 briefly shipped an abbreviated
	// lower(code)-issuer8 form; the operator flipped it to the full
	// form one day later because an 8-char issuer prefix is a ~2^32
	// vanity-grind (dust-attack address mimicry does this routinely)
	// and the abbreviation bought nothing but URL length. The full
	// form is self-certifying and unique by construction — it IS the
	// primary key's value — which also deletes the collision-retry
	// apparatus 0134's writer needed. ON CONFLICT the existing slug
	// is kept (a slug is a public URL; it never silently changes).
	const q = `
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
	if _, err := s.db.ExecContext(ctx, q,
		assetID, asset.Code, asset.Issuer,
		observedAt.UTC(), int(ledger),
	); err != nil {
		return fmt.Errorf("timescale: registerClassicAssetSeen %s: %w", assetID, err)
	}
	s.assetRegistryDedupe.Store(assetID, time.Now())
	return nil
}

// ResetAssetRegistryDedupeForTest clears this Store's
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
func (s *Store) ResetAssetRegistryDedupeForTest() {
	s.assetRegistryDedupe.Range(func(k, _ any) bool {
		s.assetRegistryDedupe.Delete(k)
		return true
	})
	s.issuerRegistryDedupe.Range(func(k, _ any) bool {
		s.issuerRegistryDedupe.Delete(k)
		return true
	})
}

// shouldSkipAssetRegistryUpsert returns true when `now` falls
// within `assetRegistryDedupeTTL` of the last recorded upsert
// for `assetID` on THIS store. Returns false on no-cache (first
// time) and on expired-cache (TTL elapsed). Touches nothing but
// the in-memory cache so the F-1243 TTL-gate semantics can be
// unit-tested without standing up a Postgres container.
func (s *Store) shouldSkipAssetRegistryUpsert(assetID string, now time.Time) bool {
	cached, ok := s.assetRegistryDedupe.Load(assetID)
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
	if _, seen := s.issuerRegistryDedupe.Load(gStrkey); seen {
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
	s.issuerRegistryDedupe.Store(gStrkey, struct{}{})
	return nil
}

// ClassicAssetBySlug resolves a PUBLIC slug to its classic
// (code, issuer) identity. Since migration 0135 the slug IS the
// fully-qualified asset_id (self-certifying — see the writer note in
// registerClassicAssetSeen), so this lookup mostly serves
// case-mangled URLs: a lowercased full form fails strkey parsing in
// the canonical parser and lands here instead. Exact match wins over
// the case-folded match. ok=false when no row carries the slug (not
// an error).
func (s *Store) ClassicAssetBySlug(ctx context.Context, slug string) (code, issuer string, ok bool, err error) {
	const q = `
		SELECT code, issuer_g_strkey
		  FROM classic_assets
		 WHERE slug = $1 OR lower(slug) = lower($1)
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
