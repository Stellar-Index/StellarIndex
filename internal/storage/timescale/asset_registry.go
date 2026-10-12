// This file is the ONLY writer of `classic_assets` and `issuers`
// (ADR-0031/0032: one writer per domain, coverage derived from the data).
//
// Two observation sources feed it:
//
//   - TRADES, from the indexer hot path ([Store.registerClassicAssetSeen]). Advances the trade
//     provenance columns and `observation_count`.
//   - TRUSTLINE HOLDINGS, from the ClickHouse lake ([Store.RegisterClassicAssetsHeld], called by
//     `stellarindex-ops asset-registry-backfill`). Advances the holding provenance columns only.
//
// The ops job owns no SQL against either table, so the second source is not a second writer with
// its own conflict clause and idea of `observation_count`.
//
// Both sources are idempotent and MONOTONE: LEAST on the first-seen pair, GREATEST on the
// last-seen pair, and only the trade source touches the counter, so re-running over covered
// ground converges instead of inflating.
package timescale

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// assetRegistryDedupeTTL caps upserts to one per asset per window; a permanent seen-set would freeze last_seen_* and observation_count.
const assetRegistryDedupeTTL = 60 * time.Second

// assetRegistryDedupe / issuerRegistryDedupe live on [Store], not at package scope, so the cache
// is scoped to ONE database: asset_id is only unique within a database, and a shared map would
// "dedupe" the second DB's first trade for an asset so its classic_assets row is never written.
// The zero value is ready to use. See the field docs on [Store].

// registerClassicAssetSeen ensures a classic_assets row exists, bumping last_seen_* and observation_count. Trade-side half;
// held-but-never-traded assets come from [Store.RegisterClassicAssetsHeld]. Callers log and swallow errors so a registry failure cannot
// sink the trade insert.
func (s *Store) registerClassicAssetSeen(
	ctx context.Context,
	asset canonical.Asset,
	ledger uint32,
	observedAt time.Time,
) error {
	return s.registerClassicAssetRange(ctx, asset, newRegistryObservation(ledger, observedAt))
}

// upsertClassicAssetSQL binds ($1..$3) identity, ($4,$5) first ts/ledger and
// ($6,$7) last ts/ledger.
const upsertClassicAssetSQL = `
		INSERT INTO classic_assets (
			asset_id, code, issuer_g_strkey, slug,
			first_seen_at, first_seen_ledger,
			last_seen_at,  last_seen_ledger,
			first_trade_at, first_trade_ledger,
			last_trade_at,  last_trade_ledger,
			observation_count
		) VALUES (
			$1, $2, $3, $1,
			$4, $5, $6, $7,
			$4, $5, $6, $7, 1
		)
		ON CONFLICT (asset_id) DO UPDATE SET
			first_seen_at      = LEAST(classic_assets.first_seen_at, EXCLUDED.first_seen_at),
			first_seen_ledger  = LEAST(classic_assets.first_seen_ledger, EXCLUDED.first_seen_ledger),
			last_seen_at       = GREATEST(classic_assets.last_seen_at, EXCLUDED.last_seen_at),
			last_seen_ledger   = GREATEST(classic_assets.last_seen_ledger, EXCLUDED.last_seen_ledger),
			first_trade_at     = LEAST(classic_assets.first_trade_at, EXCLUDED.first_trade_at),
			first_trade_ledger = LEAST(classic_assets.first_trade_ledger, EXCLUDED.first_trade_ledger),
			last_trade_at      = GREATEST(classic_assets.last_trade_at, EXCLUDED.last_trade_at),
			last_trade_ledger  = GREATEST(classic_assets.last_trade_ledger, EXCLUDED.last_trade_ledger),
			observation_count  = classic_assets.observation_count + 1
	`

// registerClassicAssetRange is [Store.registerClassicAssetSeen] for a lowest..highest range; a lower minimum bypasses the dedupe TTL so
// backward backfill still reaches first_seen_*.
func (s *Store) registerClassicAssetRange(
	ctx context.Context,
	asset canonical.Asset,
	o registryObservation,
) error {
	if asset.Type != canonical.AssetClassic {
		return nil
	}
	assetID := asset.String()
	// Skip the upsert only within `assetRegistryDedupeTTL` of the last successful one, so the row
	// never freezes at first observation.
	if s.shouldSkipAssetRegistryUpsert(assetID, time.Now()) && !s.lowersCachedMinLedger(assetID, o.minLedger) {
		return nil
	}

	// Issuer first: there is no FK, but this order guarantees every queryable classic_assets row
	// has a matching issuers row.
	if err := s.registerIssuerSeen(ctx, asset.Issuer); err != nil {
		return err
	}

	// first_seen_* uses LEAST so chunked / parallel backfill that processes ledgers out of order
	// cannot leave a higher value behind.
	//
	// slug: the fully-qualified asset_id verbatim, since an 8-char issuer prefix is a ~2^32
	// vanity-grind (address mimicry). It IS the primary key, so it is unique by construction.
	// ON CONFLICT keeps the existing slug (a public URL never silently changes).
	//
	// first_trade_* / last_trade_* carry the *_seen_* values on THIS path only, because the
	// *_seen_* pair also moves for a trustline-only observation and a "last traded" surface needs a
	// column that means exactly that. LEAST/GREATEST ignore NULL operands in Postgres, so an asset
	// the holdings path registered first still gets its trade columns filled by its first trade.
	if _, err := s.db.ExecContext(ctx, upsertClassicAssetSQL,
		assetID, asset.Code, asset.Issuer,
		o.minTs.UTC(), int(o.minLedger), o.maxTs.UTC(), int(o.maxLedger),
	); err != nil {
		return fmt.Errorf("timescale: registerClassicAssetSeen %s: %w", assetID, err)
	}
	s.assetRegistryDedupe.Store(assetID, time.Now())
	if _, cached := s.assetRegistryMinLedger.Load(assetID); !cached || s.lowersCachedMinLedger(assetID, o.minLedger) {
		s.assetRegistryMinLedger.Store(assetID, o.minLedger)
	}
	return nil
}

// lowersCachedMinLedger reports whether minLedger is below the lowest first ledger this Store has
// upserted for assetID; false when nothing is cached, leaving the TTL gate to decide.
func (s *Store) lowersCachedMinLedger(assetID string, minLedger uint32) bool {
	cached, ok := s.assetRegistryMinLedger.Load(assetID)
	if !ok {
		return false
	}
	prev, ok := cached.(uint32)
	return ok && minLedger < prev
}

// ResetAssetRegistryDedupeForTest clears the dedupe cache to simulate a restart between a trade insert and its replay, so the TTL cache cannot mask a regression in the RowsAffected == 0 guard.
func (s *Store) ResetAssetRegistryDedupeForTest() {
	s.assetRegistryDedupe.Range(func(k, _ any) bool {
		s.assetRegistryDedupe.Delete(k)
		return true
	})
	s.assetRegistryMinLedger.Range(func(k, _ any) bool {
		s.assetRegistryMinLedger.Delete(k)
		return true
	})
	s.issuerRegistryDedupe.Range(func(k, _ any) bool {
		s.issuerRegistryDedupe.Delete(k)
		return true
	})
}

// shouldSkipAssetRegistryUpsert reports whether `assetID` was upserted on THIS store within the TTL; in-memory only so it is
// unit-testable.
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

// registerIssuerSeen idempotently ensures an issuers row, writing only g_strkey; home_domain and SEP-1 data come from the AccountEntry
// observer (ADR-0021), with known_issuers.go as the wire-boundary fallback.
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

// ClassicAssetBySlug resolves a public slug (the fully-qualified asset_id) to its classic (code, issuer), serving case-mangled URLs; exact match beats case-folded and ok=false (not an error) when none matches.
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

// ─── holdings-fed registration ──────────────────────────────────────────

// classicAssetHoldingBatchMax caps observations per INSERT: seven binds each, so 2,000 rows stays far under Postgres's 65,535 parameter
// limit.
const classicAssetHoldingBatchMax = 2000

// ClassicAssetHolding is one holdings-derived observation of a classic asset (trustline evidence it exists); First/Last bracket the evidence ledgers and First is only an upper bound on genesis.
type ClassicAssetHolding struct {
	// Asset must be canonical.AssetClassic; native and pool-share trustlines are skipped since the
	// registry is keyed on (code, issuer).
	Asset canonical.Asset

	FirstLedger uint32
	FirstAt     time.Time
	LastLedger  uint32
	LastAt      time.Time
}

// RegisterClassicAssetsHeld records holdings evidence for classic assets, creating missing `classic_assets` and `issuers` rows; it never touches observation_count, which counts trade observations only.
func (s *Store) RegisterClassicAssetsHeld(ctx context.Context, obs []ClassicAssetHolding) (assets, issuers int64, err error) {
	for start := 0; start < len(obs); start += classicAssetHoldingBatchMax {
		end := min(start+classicAssetHoldingBatchMax, len(obs))
		a, i, berr := s.registerClassicAssetsHeldBatch(ctx, obs[start:end])
		assets += a
		issuers += i
		if berr != nil {
			return assets, issuers, berr
		}
	}
	return assets, issuers, nil
}

// registerClassicAssetsHeldBatch writes issuers first, then assets, so every classic_assets row has an issuers row once queryable (the
// RWA membership build and SEP-1 queue run concurrently; there is no FK).
func (s *Store) registerClassicAssetsHeldBatch(ctx context.Context, obs []ClassicAssetHolding) (int64, int64, error) {
	rows := make([]ClassicAssetHolding, 0, len(obs))
	seenIssuer := make(map[string]struct{}, len(obs))
	issuerArgs := make([]any, 0, len(obs))
	for _, o := range obs {
		if o.Asset.Type != canonical.AssetClassic || o.Asset.Issuer == "" {
			continue
		}
		if o.FirstAt.IsZero() || o.LastAt.IsZero() {
			// first_seen_at cannot be NULL and must not be fabricated, so an observation with no
			// close time is dropped and the caller counts the drop.
			continue
		}
		rows = append(rows, normaliseHoldingBracket(o))
		if _, dup := seenIssuer[o.Asset.Issuer]; !dup {
			seenIssuer[o.Asset.Issuer] = struct{}{}
			issuerArgs = append(issuerArgs, o.Asset.Issuer)
		}
	}
	if len(rows) == 0 {
		return 0, 0, nil
	}

	issuerCount, err := s.insertIssuersBatch(ctx, issuerArgs)
	if err != nil {
		return 0, 0, err
	}
	assetCount, err := s.upsertHeldAssetsBatch(ctx, rows)
	if err != nil {
		return 0, issuerCount, err
	}
	return assetCount, issuerCount, nil
}

// normaliseHoldingBracket orders first <= last on ledger and timestamp: classic_assets CHECKs last_seen_ledger >= first_seen_ledger, and
// one inverted row would abort the whole multi-row statement.
func normaliseHoldingBracket(o ClassicAssetHolding) ClassicAssetHolding {
	if o.LastLedger < o.FirstLedger {
		o.FirstLedger, o.LastLedger = o.LastLedger, o.FirstLedger
	}
	if o.LastAt.Before(o.FirstAt) {
		o.FirstAt, o.LastAt = o.LastAt, o.FirstAt
	}
	return o
}

// insertIssuersBatch is the batch form of [Store.registerIssuerSeen] (g_strkey only, ON CONFLICT DO NOTHING). Needed because
// held-but-never-traded issuers would otherwise never get sep1_payload, which BoundSep1Currencies (the RWA membership input) requires.
func (s *Store) insertIssuersBatch(ctx context.Context, gStrkeys []any) (int64, error) {
	if len(gStrkeys) == 0 {
		return 0, nil
	}
	var b strings.Builder
	b.WriteString("INSERT INTO issuers (g_strkey) VALUES ")
	for i := range gStrkeys {
		if i > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "($%d)", i+1)
	}
	b.WriteString(" ON CONFLICT (g_strkey) DO NOTHING")
	res, err := s.db.ExecContext(ctx, b.String(), gStrkeys...)
	if err != nil {
		return 0, fmt.Errorf("timescale: insertIssuersBatch (%d): %w", len(gStrkeys), err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// upsertHeldAssetsBatch widens first_seen_*/last_seen_* and the holding columns only; LEAST/GREATEST ignore NULL, so NULL holding
// columns fill in.
func (s *Store) upsertHeldAssetsBatch(ctx context.Context, rows []ClassicAssetHolding) (int64, error) {
	var b strings.Builder
	b.WriteString(`INSERT INTO classic_assets (
			asset_id, code, issuer_g_strkey, slug,
			first_seen_at, first_seen_ledger,
			last_seen_at,  last_seen_ledger,
			first_holding_at, first_holding_ledger,
			last_holding_at,  last_holding_ledger,
			observation_count
		) VALUES `)
	args := make([]any, 0, len(rows)*7)
	for i, r := range rows {
		if i > 0 {
			b.WriteString(", ")
		}
		p := i * 7
		// slug reuses the asset_id parameter (self-certifying, unique). observation_count is the
		// literal 0: no trade has been observed for a row this statement creates.
		fmt.Fprintf(&b, "($%d, $%d, $%d, $%d, $%d, $%d, $%d, $%d, $%d, $%d, $%d, $%d, 0)",
			p+1, p+2, p+3, p+1, p+4, p+5, p+6, p+7, p+4, p+5, p+6, p+7)
		args = append(args,
			r.Asset.String(), r.Asset.Code, r.Asset.Issuer,
			r.FirstAt.UTC(), int(r.FirstLedger),
			r.LastAt.UTC(), int(r.LastLedger),
		)
	}
	b.WriteString(`
		ON CONFLICT (asset_id) DO UPDATE SET
			first_seen_at        = LEAST(classic_assets.first_seen_at, EXCLUDED.first_seen_at),
			first_seen_ledger    = LEAST(classic_assets.first_seen_ledger, EXCLUDED.first_seen_ledger),
			last_seen_at         = GREATEST(classic_assets.last_seen_at, EXCLUDED.last_seen_at),
			last_seen_ledger     = GREATEST(classic_assets.last_seen_ledger, EXCLUDED.last_seen_ledger),
			first_holding_at     = LEAST(classic_assets.first_holding_at, EXCLUDED.first_holding_at),
			first_holding_ledger = LEAST(classic_assets.first_holding_ledger, EXCLUDED.first_holding_ledger),
			last_holding_at      = GREATEST(classic_assets.last_holding_at, EXCLUDED.last_holding_at),
			last_holding_ledger  = GREATEST(classic_assets.last_holding_ledger, EXCLUDED.last_holding_ledger)`)
	res, err := s.db.ExecContext(ctx, b.String(), args...)
	if err != nil {
		return 0, fmt.Errorf("timescale: upsertHeldAssetsBatch (%d rows): %w", len(rows), err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// ClassicAssetRegistryStats is the registry coverage the backfill reports, split by source so a row count that mixes both is not misread.
type ClassicAssetRegistryStats struct {
	// Total is every row in classic_assets.
	Total int64
	// WithTrade have been seen trading at least once.
	WithTrade int64
	// WithHolding carry trustline evidence from a holdings scan.
	WithHolding int64
	// HoldingOnly are held but never traded — the population that had no
	// row at all before the holdings path existed. BENJI lives here.
	HoldingOnly int64
	// TradeOnly have traded but carry no holdings evidence yet. A large
	// figure right after a completed backfill means the walk missed a
	// slice of the lake, not that the assets have no holders.
	TradeOnly int64
}

// ClassicAssetRegistryStats counts the registry by observation source with one sequential scan of a plain table, called by the backfill before and after to show its delta.
func (s *Store) ClassicAssetRegistryStats(ctx context.Context) (ClassicAssetRegistryStats, error) {
	const q = `
		SELECT count(*)::bigint,
		       count(*) FILTER (WHERE last_trade_at   IS NOT NULL)::bigint,
		       count(*) FILTER (WHERE last_holding_at IS NOT NULL)::bigint,
		       count(*) FILTER (WHERE last_trade_at   IS NULL AND last_holding_at IS NOT NULL)::bigint,
		       count(*) FILTER (WHERE last_holding_at IS NULL AND last_trade_at   IS NOT NULL)::bigint
		  FROM classic_assets
	`
	var st ClassicAssetRegistryStats
	if err := s.db.QueryRowContext(ctx, q).Scan(
		&st.Total, &st.WithTrade, &st.WithHolding, &st.HoldingOnly, &st.TradeOnly,
	); err != nil {
		return ClassicAssetRegistryStats{}, fmt.Errorf("timescale: ClassicAssetRegistryStats: %w", err)
	}
	return st, nil
}
