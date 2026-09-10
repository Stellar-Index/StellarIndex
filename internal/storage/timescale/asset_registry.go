// This file is the ONLY writer of `classic_assets` and `issuers`.
//
// Two observation sources feed it, and keeping them behind one writer is
// the point (ADR-0031/0032: one writer per domain, coverage derived from
// the data rather than from a cursor a second writer forgot to move):
//
//   - TRADES, from the indexer hot path. [Store.registerClassicAssetSeen],
//     called by InsertTrade / BatchInsertTrades. Advances the trade
//     provenance columns and `observation_count`.
//   - TRUSTLINE HOLDINGS, from the ClickHouse lake.
//     [Store.RegisterClassicAssetsHeld], called by `stellarindex-ops
//     asset-registry-backfill`. Advances the holding provenance columns
//     and nothing else.
//
// The ops job owns NO SQL against either table: it is a feeder that reads
// the lake and hands observations to the writer below. That is what stops
// the second source from becoming a second writer with its own INSERT, its
// own conflict clause and its own idea of what `observation_count` means —
// the shape ADR-0032 was written about after the per-source tables drifted
// from `soroban_events`.
//
// Both sources are idempotent and MONOTONE: LEAST on the first-seen pair,
// GREATEST on the last-seen pair, and only the trade source touches the
// counter. Re-running either over ground it has already covered converges
// instead of inflating.
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
// This is the trade-side half. The holdings half is
// [Store.RegisterClassicAssetsHeld], fed from the lake by
// `stellarindex-ops asset-registry-backfill` — for six years this
// path was the ONLY one, so an asset that is held but never traded
// (a money-market fund, say) had no row here at all, and therefore
// no issuers row, no SEP-1 fetch and no RWA candidacy.
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
	// first_trade_* / last_trade_* (migration 0158) carry the same values
	// as the *_seen_* pair on THIS path and only on this path. They exist
	// because the *_seen_* pair now also moves for a trustline-only
	// observation, and a surface that means "last traded" must keep a
	// column that means exactly that. LEAST/GREATEST ignore NULL operands
	// in Postgres, so an asset the holdings path registered first gets its
	// trade columns filled correctly by its very first trade.
	const q = `
		INSERT INTO classic_assets (
			asset_id, code, issuer_g_strkey, slug,
			first_seen_at, first_seen_ledger,
			last_seen_at,  last_seen_ledger,
			first_trade_at, first_trade_ledger,
			last_trade_at,  last_trade_ledger,
			observation_count
		) VALUES (
			$1, $2, $3, $1,
			$4, $5, $4, $5,
			$4, $5, $4, $5, 1
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

// ─── holdings-fed registration ──────────────────────────────────────────

// classicAssetHoldingBatchMax caps how many observations one call packs
// into a single multi-row INSERT.
//
// The bound that matters is the Postgres wire protocol limit of 65,535
// bind parameters per statement, and each observation binds seven. 2,000
// rows is 14,000 parameters — a quarter of the ceiling, so the statement
// stays well clear even if a future column joins the row — while still
// amortising the round trip over enough rows that the 512k-asset walk is
// a few hundred statements rather than half a million.
const classicAssetHoldingBatchMax = 2000

// ClassicAssetHolding is one holdings-derived observation of a classic
// asset: evidence, taken from trustline ledger entries, that the asset
// EXISTS, independent of whether anyone has ever traded it.
//
// First/Last bracket the ledgers the evidence spans. They may be equal
// (one trustline, or many sharing a last-modified ledger). Neither is the
// asset genesis ledger — a trustline entry carries the ledger it was last
// modified in, so First is an upper bound on genesis that improves as
// later scans meet older trustlines. See migration 0158.
type ClassicAssetHolding struct {
	// Asset must be canonical.AssetClassic. Anything else is skipped:
	// the registry is keyed on (code, issuer) and native / pool-share
	// trustlines have no such identity.
	Asset canonical.Asset

	FirstLedger uint32
	FirstAt     time.Time
	LastLedger  uint32
	LastAt      time.Time
}

// RegisterClassicAssetsHeld records holdings evidence for a batch of
// classic assets, creating the `classic_assets` row (and its `issuers`
// row) when none exists.
//
// This is the second of the registry's two observation sources; see the
// file header for why both live behind one writer. What it does NOT do is
// as load-bearing as what it does:
//
//   - It never touches observation_count. That column counts TRADE
//     observations and nothing else, so the explorer column labelled
//     "Observations", the default listing rank and the scam-triage sweep
//     that reads the top of that rank all keep meaning what they meant.
//     A row this function creates starts at 0 and stays there until a
//     real trade arrives.
//   - It never touches first_trade_* / last_trade_*, so "last traded"
//     stays answerable and stays NULL for an asset that has never traded.
//   - It never touches slug. A slug is a public URL; ON CONFLICT keeps
//     whatever the row already has, and a new row takes the migration-0135
//     form, which is the asset_id verbatim.
//   - It does not populate the [Store] dedupe caches. Those exist to keep
//     the indexer hot path off this table for 60 seconds per asset, and a
//     bulk walk seeding them would suppress the very next trade's
//     observation_count increment for any asset it had just touched.
//
// Returns the number of asset rows and issuer rows the statement reports
// as affected. Postgres counts an ON CONFLICT DO UPDATE row as affected
// whether or not any value changed, so treat the asset figure as "rows
// considered", not "rows changed" — the caller reports it that way.
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

// registerClassicAssetsHeldBatch writes one bounded batch: the issuers
// first, then the assets.
//
// Issuers first, matching [Store.registerClassicAssetSeen]. There is no FK
// between the tables, so the order is not a constraint — it is what makes
// "every classic_assets row has an issuers row by the time it is
// queryable" true for a concurrent reader, which is what the RWA
// membership build and the SEP-1 refresh queue walk.
func (s *Store) registerClassicAssetsHeldBatch(ctx context.Context, obs []ClassicAssetHolding) (int64, int64, error) {
	rows := make([]ClassicAssetHolding, 0, len(obs))
	seenIssuer := make(map[string]struct{}, len(obs))
	issuerArgs := make([]any, 0, len(obs))
	for _, o := range obs {
		if o.Asset.Type != canonical.AssetClassic || o.Asset.Issuer == "" {
			continue
		}
		if o.FirstAt.IsZero() || o.LastAt.IsZero() {
			// A registry row cannot carry a NULL first_seen_at, and
			// fabricating one would put a lie in a timestamp column. An
			// observation with no close time is dropped, and the caller
			// counts the drop.
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

// normaliseHoldingBracket orders the bracket so first <= last on both the
// ledger and the timestamp.
//
// The lake supplies min/max from one aggregate so they arrive ordered, but
// classic_assets carries CHECK (last_seen_ledger >= first_seen_ledger) and
// a single inverted row would abort the whole multi-row statement — 2,000
// assets lost to one bad pair. Sorting here costs two comparisons and
// makes that class of failure impossible rather than merely unlikely.
func normaliseHoldingBracket(o ClassicAssetHolding) ClassicAssetHolding {
	if o.LastLedger < o.FirstLedger {
		o.FirstLedger, o.LastLedger = o.LastLedger, o.FirstLedger
	}
	if o.LastAt.Before(o.FirstAt) {
		o.FirstAt, o.LastAt = o.LastAt, o.FirstAt
	}
	return o
}

// insertIssuersBatch is the batch form of [Store.registerIssuerSeen]:
// g_strkey only, ON CONFLICT DO NOTHING, everything else left to the
// AccountEntry observer and the SEP-1 fetcher.
//
// Creating these rows is the whole point of the holdings path as far as
// the RWA surface is concerned. `issuers` is written ONLY from inside the
// registry writer, so an issuer whose assets are held but never traded had
// no row, was never offered to `issuer-enrich`, therefore never got a
// home_domain, therefore never entered IssuersNeedingSep1Refresh, therefore
// had no sep1_payload — and BoundSep1Currencies, which is the RWA
// membership build's only input, selects rows WHERE sep1_payload IS NOT
// NULL.
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

// upsertHeldAssetsBatch is the registry upsert for holdings evidence.
//
// The conflict clause widens first_seen_* / last_seen_* and the holding
// columns and touches nothing else. LEAST and GREATEST ignore NULL
// operands in Postgres, so a row that predates migration 0158 — every
// holding column NULL — gets them filled on its first pass rather than
// staying NULL forever.
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
		// slug reuses the row's asset_id parameter, per migration 0135:
		// the slug IS the fully-qualified asset_id, self-certifying and
		// unique by construction. observation_count is the literal 0 —
		// no trade has been observed for a row this statement creates.
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

// ClassicAssetRegistryStats is the registry coverage the backfill reports
// so an operator reads the population rather than inferring it from a
// row count that mixes both sources.
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

// ClassicAssetRegistryStats counts the registry by observation source.
// One sequential scan of a plain table; the backfill calls it twice (once
// before, once after) so its summary can show the delta it caused.
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
