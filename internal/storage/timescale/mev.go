package timescale

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/domain"
	"github.com/Stellar-Index/StellarIndex/internal/pgarray"
)

// TradesForArbScan returns the NEWEST `limit` ON-CHAIN trades (ledger >
// 0, with a taker) that closed after `since`, returned ascending by
// (ledger, tx_hash, op_index) — the order the MEV detector groups on —
// plus a parallel slice of each trade's USD volume ("" when NULL).
//
// The cap keeps the newest rows, not the oldest: the worker re-scans a
// trailing window every tick, so earlier ticks already covered the oldest
// rows, while an oldest-first LIMIT drops the same burst tail on every
// tick. len == limit means the window was truncated; the worker counts
// that and trims the possibly-partial oldest ledger.
func (s *Store) TradesForArbScan(ctx context.Context, since time.Time, limit int) ([]canonical.Trade, []string, error) {
	if limit <= 0 {
		limit = 50_000
	}
	// Per-leg USD value: prefer the stored usd_volume, else estimate
	// from the XLM leg × the current XLM/USD VWAP (same fallback the
	// markets queries use). Without this, SDEX arb legs — which usually
	// quote XLM/token and carry NULL usd_volume — summed to ~$0, so the
	// MEV feed showed "$0" notionals on real multi-leg cycles (audit
	// 2026-06-19). Token/token legs with no XLM side stay '' (no USD
	// basis). 'CAS3J7…' is the native-XLM SAC.
	const q = `
        WITH xlm_usd AS (
          SELECT vwap
            FROM prices_1m
           WHERE base_asset = 'native'
             AND quote_asset IN (
               'USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN',
               'fiat:USD'
             )
             AND vwap IS NOT NULL
             AND bucket >= NOW() - INTERVAL '24 hours'
           ORDER BY bucket DESC
           LIMIT 1
        )
        SELECT source, ledger, tx_hash, op_index, ts,
               base_asset, quote_asset,
               base_amount, quote_amount,
               COALESCE(maker, ''), COALESCE(taker, ''),
               COALESCE((COALESCE(
                 usd_volume,
                 CASE
                   WHEN base_asset IN ('native', 'CAS3J7GYLGXMF6TDJBBYYSE3HQ6BBSMLNUQ34T6TZMYMW2EVH34XOWMA')
                     THEN (base_amount / 1e7::numeric) * (SELECT vwap FROM xlm_usd)
                   WHEN quote_asset IN ('native', 'CAS3J7GYLGXMF6TDJBBYYSE3HQ6BBSMLNUQ34T6TZMYMW2EVH34XOWMA')
                     THEN (quote_amount / 1e7::numeric) * (SELECT vwap FROM xlm_usd)
                   ELSE NULL
                 END
               ))::text, '')
          FROM trades
         WHERE ts > $1
           AND ledger > 0
           AND taker IS NOT NULL AND taker <> ''
         ORDER BY ledger DESC, tx_hash DESC, op_index DESC
         LIMIT $2
    `
	rows, err := s.db.QueryContext(ctx, q, since.UTC(), limit)
	if err != nil {
		return nil, nil, fmt.Errorf("timescale: TradesForArbScan: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var (
		trades []canonical.Trade
		usd    []string
	)
	for rows.Next() {
		var (
			t                     canonical.Trade
			baseAsset, quoteAsset string
			usdVol                string
		)
		if err := rows.Scan(
			&t.Source, &t.Ledger, &t.TxHash, &t.OpIndex, &t.Timestamp,
			&baseAsset, &quoteAsset,
			&t.BaseAmount, &t.QuoteAmount,
			&t.Maker, &t.Taker, &usdVol,
		); err != nil {
			return nil, nil, fmt.Errorf("timescale: TradesForArbScan scan: %w", err)
		}
		base, err := canonical.ParseAsset(baseAsset)
		if err != nil {
			return nil, nil, fmt.Errorf("timescale: TradesForArbScan base %q: %w", baseAsset, err)
		}
		quote, err := canonical.ParseAsset(quoteAsset)
		if err != nil {
			return nil, nil, fmt.Errorf("timescale: TradesForArbScan quote %q: %w", quoteAsset, err)
		}
		pair, err := canonical.NewPair(base, quote)
		if err != nil {
			return nil, nil, fmt.Errorf("timescale: TradesForArbScan pair: %w", err)
		}
		t.Pair = pair
		trades = append(trades, t)
		usd = append(usd, usdVol)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("timescale: TradesForArbScan rows: %w", err)
	}
	slices.Reverse(trades)
	slices.Reverse(usd)
	return trades, usd, nil
}

// oracleUpdatesForMEVScanQuery is the OracleUpdatesForMEVScan SQL.
// `asset NOT LIKE 'raw:%'` excludes the unmapped rows the oracle
// capture-totality design records verbatim (canonical.AssetOracleRaw):
// they are record-layer only and must never become MEV evidence. The
// detectors also key on asset (cascade on the position's assets, the
// sandwich on the trade's), so this predicate is the SQL-side half of
// that rule.
const oracleUpdatesForMEVScanQuery = `
        SELECT source, COALESCE(contract_id, ''), ledger, tx_hash, op_index,
               asset, quote, ts
          FROM oracle_updates
         WHERE ts > $1
           AND ledger > 0
           AND asset NOT LIKE 'raw:%'
         ORDER BY ledger DESC, tx_hash DESC, op_index DESC
         LIMIT $2
    `

// OracleUpdatesForMEVScan returns the newest `limit` ON-CHAIN oracle
// updates (ledger > 0, mapped assets only — see
// oracleUpdatesForMEVScanQuery) published after `since`, returned
// ascending by ledger (newest kept, for the reason TradesForArbScan gives).
// Satisfies mev.OracleScanner — the input for the oracle_sandwich +
// liquidation_cascade detectors.
func (s *Store) OracleUpdatesForMEVScan(ctx context.Context, since time.Time, limit int) ([]domain.MEVOracleRef, error) {
	if limit <= 0 {
		limit = 50_000
	}
	rows, err := s.db.QueryContext(ctx, oracleUpdatesForMEVScanQuery, since.UTC(), limit)
	if err != nil {
		return nil, fmt.Errorf("timescale: OracleUpdatesForMEVScan: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []domain.MEVOracleRef
	for rows.Next() {
		var o domain.MEVOracleRef
		if err := rows.Scan(
			&o.Source, &o.ContractID, &o.Ledger, &o.TxHash, &o.OpIndex,
			&o.Asset, &o.Quote, &o.Timestamp,
		); err != nil {
			return nil, fmt.Errorf("timescale: OracleUpdatesForMEVScan scan: %w", err)
		}
		out = append(out, o)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("timescale: OracleUpdatesForMEVScan rows: %w", err)
	}
	slices.Reverse(out)
	return out, nil
}

// BlendFillsForMEVScan returns recent Blend liquidation-relevant
// auction fills (event_kind='fill', auction_type 0=UserLiquidation /
// 1=BadDebt — Interest auctions aren't liquidations) after `since`: the
// newest `limit`, returned ascending by ledger (newest kept, for the
// reason TradesForArbScan gives). Satisfies mev.AuctionScanner.
func (s *Store) BlendFillsForMEVScan(ctx context.Context, since time.Time, limit int) ([]domain.MEVAuctionFill, error) {
	if limit <= 0 {
		limit = 50_000
	}
	// assets: the distinct reserve assets in the fill's bid + lot — the
	// position's debt and collateral, which the cascade correlator keys
	// its oracle evidence on.
	const q = `
        SELECT pool, user_address, COALESCE(filler, ''), auction_type,
               ledger, tx_hash, op_index, ts,
               ARRAY(
                 SELECT DISTINCT e->>'asset'
                   FROM jsonb_array_elements(COALESCE(bid, '[]'::jsonb) || COALESCE(lot, '[]'::jsonb)) AS e
                  WHERE e->>'asset' IS NOT NULL
                  ORDER BY 1
               ) AS assets
          FROM blend_auctions
         WHERE ts > $1
           AND event_kind = 'fill'
           AND auction_type IN (0, 1)
         ORDER BY ledger DESC, tx_hash DESC, op_index DESC
         LIMIT $2
    `
	rows, err := s.db.QueryContext(ctx, q, since.UTC(), limit)
	if err != nil {
		return nil, fmt.Errorf("timescale: BlendFillsForMEVScan: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []domain.MEVAuctionFill
	for rows.Next() {
		var f domain.MEVAuctionFill
		if err := rows.Scan(
			&f.Pool, &f.User, &f.Filler, &f.AuctionType,
			&f.Ledger, &f.TxHash, &f.OpIndex, &f.Timestamp,
			pgarray.Strings(&f.Assets),
		); err != nil {
			return nil, fmt.Errorf("timescale: BlendFillsForMEVScan scan: %w", err)
		}
		out = append(out, f)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("timescale: BlendFillsForMEVScan rows: %w", err)
	}
	slices.Reverse(out)
	return out, nil
}

// insertMEVEventQuery is idempotent on dedup_key, but a re-scan whose
// evidence CONTAINS the stored row's legs replaces its evidence columns: a
// later overlapping scan that found more victims or more round-trip fills
// supersedes the first partial detection instead of being dropped. A scan
// that saw less (its window slid past earlier legs) never overwrites.
// detected_at and the ledger keep the first detection's identity. RETURNING
// xmax = 0 is true only for a fresh insert, so an update is not counted as
// a new event.
const insertMEVEventQuery = `
        INSERT INTO mev_events (
            detected_at, detected_at_ledger, kind,
            asset_id, quote_id, tx_hashes, accounts,
            detail, profit_usd, dedup_key
        ) VALUES (
            $1, $2, $3,
            $4, $5, $6, $7,
            $8, NULL, $9
        )
        ON CONFLICT (dedup_key) WHERE dedup_key IS NOT NULL DO UPDATE
           SET tx_hashes = EXCLUDED.tx_hashes,
               accounts  = EXCLUDED.accounts,
               detail    = EXCLUDED.detail
         WHERE (EXCLUDED.detail -> 'legs') @> (mev_events.detail -> 'legs')
           AND EXCLUDED.detail IS DISTINCT FROM mev_events.detail
        RETURNING (xmax = 0)
    `

// InsertMEVEvent persists a detected MEV event (see insertMEVEventQuery).
// Returns inserted=true only for a new event. profit_usd is written NULL:
// no detector estimates attacker profit, and trade notional is not profit.
// Satisfies mev.Sink.
func (s *Store) InsertMEVEvent(ctx context.Context, e domain.MEVStoredEvent) (bool, error) {
	var inserted bool
	err := s.db.QueryRowContext(ctx, insertMEVEventQuery,
		e.Timestamp.UTC(), int(e.DetectedAtLedger), e.Kind,
		nullString(e.AssetID), nullString(e.QuoteID), e.TxHashes, e.Accounts,
		string(e.DetailJSON), e.DedupKey,
	).Scan(&inserted)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil // conflict, stored evidence kept
	}
	if err != nil {
		return false, fmt.Errorf("timescale: InsertMEVEvent: %w", err)
	}
	return inserted, nil
}

// MEVEventRow is one mev_events row for the /v1/mev read path.
// Amounts/detail are pass-through (the API maps to the wire shape).
type MEVEventRow struct {
	EventID          string
	DetectedAt       time.Time
	DetectedAtLedger int64
	Kind             string
	AssetID          string
	QuoteID          string
	TxHashes         []string
	Accounts         []string
	Detail           string // raw jsonb text
	ProfitUSD        string // "" when NULL
}

// ListMEVEvents returns the most-recent MEV events, newest first.
// kind "" returns all kinds; a non-empty kind filters to it (uses the
// per-kind index). limit must be in [1, 500]; anything outside that
// range falls back to the 50-row default rather than being clamped to
// the ceiling — the same normalisation as ListIssuers /
// ListFreezeEvents / ListDivergenceLatest. /v1/mev already rejects an
// out-of-range ?limit= with 400 (parseExplorerLimit), so this is the
// defensive bound for direct callers.
func (s *Store) ListMEVEvents(ctx context.Context, kind string, limit int) ([]MEVEventRow, error) {
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	q := `
        SELECT event_id::text, detected_at, detected_at_ledger, kind,
               COALESCE(asset_id, ''), COALESCE(quote_id, ''),
               tx_hashes, accounts,
               detail::text, COALESCE(profit_usd::text, '')
          FROM mev_events`
	args := []any{}
	if kind != "" {
		q += ` WHERE kind = $1`
		args = append(args, kind)
		q += ` ORDER BY detected_at DESC LIMIT $2`
	} else {
		q += ` ORDER BY detected_at DESC LIMIT $1`
	}
	args = append(args, limit)

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("timescale: ListMEVEvents: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []MEVEventRow
	for rows.Next() {
		var (
			r        MEVEventRow
			accounts []string
			txHashes []string
		)
		if err := rows.Scan(
			&r.EventID, &r.DetectedAt, &r.DetectedAtLedger, &r.Kind,
			&r.AssetID, &r.QuoteID, pgarray.Strings(&txHashes), pgarray.Strings(&accounts),
			&r.Detail, &r.ProfitUSD,
		); err != nil {
			return nil, fmt.Errorf("timescale: ListMEVEvents scan: %w", err)
		}
		r.TxHashes = txHashes
		r.Accounts = accounts
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("timescale: ListMEVEvents rows: %w", err)
	}
	return out, nil
}
