package timescale

import (
	"context"
	"fmt"
	"time"

	"github.com/lib/pq"

	"github.com/StellarIndex/stellar-index/internal/aggregate/mev"
	"github.com/StellarIndex/stellar-index/internal/canonical"
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
	const q = `
        SELECT source, ledger, tx_hash, op_index, ts,
               base_asset, quote_asset,
               base_amount, quote_amount,
               COALESCE(maker, ''), COALESCE(taker, ''),
               COALESCE(usd_volume::text, '')
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

// InsertMEVEvent persists a detected MEV event, idempotent on
// dedup_key (ON CONFLICT DO NOTHING). Returns inserted=false when the
// event already existed. Satisfies mev.Sink.
func (s *Store) InsertMEVEvent(ctx context.Context, e mev.StoredEvent) (bool, error) {
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
