package timescale

import (
	"context"
	"fmt"
	"time"

	"github.com/lib/pq"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/domain"
)

// TradesForArbScan returns recent ON-CHAIN trades (ledger > 0, with a
// taker) that closed after `since`, ascending by (ledger, tx_hash,
// op_index) — the order the MEV detector groups on — plus a parallel
// slice of each trade's USD volume ("" when NULL). Capped at `limit`.
//
// The `since` lower bound prunes the trades hypertable to the recent
// chunk(s); combined with the limit this is a bounded scan, NOT an
// unbounded trade walk. The detector re-runs over overlapping windows
// and dedups on write, so a tight window each tick is sufficient.
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
         ORDER BY ledger ASC, tx_hash ASC, op_index ASC
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
	return trades, usd, nil
}

// OracleUpdatesForMEVScan returns recent ON-CHAIN oracle updates
// (ledger > 0) published after `since`, ascending by ledger, capped
// at `limit`. Satisfies mev.OracleScanner — the input for the
// oracle_sandwich + liquidation_cascade detectors.
func (s *Store) OracleUpdatesForMEVScan(ctx context.Context, since time.Time, limit int) ([]domain.MEVOracleRef, error) {
	if limit <= 0 {
		limit = 50_000
	}
	const q = `
        SELECT source, COALESCE(contract_id, ''), ledger, tx_hash, op_index,
               asset, quote, ts
          FROM oracle_updates
         WHERE ts > $1
           AND ledger > 0
         ORDER BY ledger ASC, tx_hash ASC, op_index ASC
         LIMIT $2
    `
	rows, err := s.db.QueryContext(ctx, q, since.UTC(), limit)
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
	return out, nil
}

// BlendFillsForMEVScan returns recent Blend liquidation-relevant
// auction fills (event_kind='fill', auction_type 0=UserLiquidation /
// 1=BadDebt — Interest auctions aren't liquidations) after `since`,
// ascending by ledger, capped at `limit`. Satisfies
// mev.AuctionScanner.
func (s *Store) BlendFillsForMEVScan(ctx context.Context, since time.Time, limit int) ([]domain.MEVAuctionFill, error) {
	if limit <= 0 {
		limit = 50_000
	}
	const q = `
        SELECT pool, user_address, COALESCE(filler, ''), auction_type,
               ledger, tx_hash, op_index, ts
          FROM blend_auctions
         WHERE ts > $1
           AND event_kind = 'fill'
           AND auction_type IN (0, 1)
         ORDER BY ledger ASC, tx_hash ASC, op_index ASC
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
		); err != nil {
			return nil, fmt.Errorf("timescale: BlendFillsForMEVScan scan: %w", err)
		}
		out = append(out, f)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("timescale: BlendFillsForMEVScan rows: %w", err)
	}
	return out, nil
}

// InsertMEVEvent persists a detected MEV event, idempotent on
// dedup_key (ON CONFLICT DO NOTHING). Returns inserted=false when the
// event already existed. Satisfies mev.Sink.
func (s *Store) InsertMEVEvent(ctx context.Context, e domain.MEVStoredEvent) (bool, error) {
	const q = `
        INSERT INTO mev_events (
            detected_at, detected_at_ledger, kind,
            asset_id, quote_id, tx_hashes, accounts,
            detail, profit_usd, dedup_key
        ) VALUES (
            $1, $2, $3,
            NULL, NULL, $4, $5,
            $6, NULL, $7
        )
        ON CONFLICT (dedup_key) WHERE dedup_key IS NOT NULL DO NOTHING
    `
	res, err := s.db.ExecContext(ctx, q,
		e.Timestamp.UTC(), int(e.DetectedAtLedger), e.Kind,
		pq.Array(e.TxHashes), pq.Array(e.Accounts),
		string(e.DetailJSON), e.DedupKey,
	)
	if err != nil {
		return false, fmt.Errorf("timescale: InsertMEVEvent: %w", err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
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
// per-kind index). limit is capped at 500.
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
			accounts pq.StringArray
			txHashes pq.StringArray
		)
		if err := rows.Scan(
			&r.EventID, &r.DetectedAt, &r.DetectedAtLedger, &r.Kind,
			&r.AssetID, &r.QuoteID, &txHashes, &accounts,
			&r.Detail, &r.ProfitUSD,
		); err != nil {
			return nil, fmt.Errorf("timescale: ListMEVEvents scan: %w", err)
		}
		r.TxHashes = []string(txHashes)
		r.Accounts = []string(accounts)
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("timescale: ListMEVEvents rows: %w", err)
	}
	return out, nil
}
