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

// mevStampMinute is the scanned trade's own minute, the newest the ingest stamp can have been priced from.
const mevStampMinute = "date_trunc('minute', s.ts)"

// tradesForArbScanQuery values each leg at stored usd_volume, else its XLM leg at the minute's XLM/USD anchor ([xlmUSDAnchorGridCTE]), so
// NULL-usd SDEX cycles don't report "$0"; no anchor within xlmUSDAnchorMaxAge leaves it empty. The last column marks an
// anchor-valued leg as estimated. $3 is the native-XLM SAC.
//
// A stored usd_volume is set aside when the trade has an XLM leg against a non-USD asset and a recorded XLM/USD-proxy freeze
// ([xlmUSDFreezeOverlap]) overlaps the xlmUSDAnchorMaxAge before its minute: the ingest stamp read the newest XLM/USD minute in that
// window, and a stamp served from the resolver's cache or written before the freeze row landed may carry the frozen rate. The leg
// is then valued like an unstamped one, from the anchor (estimated) or not at all. The set-aside reaches the minutes a recorded
// freeze covers (five minutes before frozen_at up to recovery), not a trigger that trailed its minute by longer. An XLM/USD-proxy
// trade keeps its stamp: its USD value is its own proxy leg, not the XLM rate. A stamp on a non-XLM trade priced through the XLM
// bridge at ingest is not set aside here and may carry a triggering or not-yet-recorded minute.
//
//nolint:gosec // G202: fragments are constant SQL (helper output built from literals and $N placeholders); values bind via $N
var tradesForArbScanQuery = `
        WITH ` + xlmUSDAnchorGridCTE("xlm_usd_grid", "$1::timestamptz", "$3::text") + `,
        scan AS (
          SELECT source, ledger, tx_hash, op_index, ts,
                 base_asset, quote_asset, base_amount, quote_amount,
                 maker, taker, usd_volume
            FROM trades
           WHERE ts > $1::timestamptz
             AND ledger > 0
             AND taker IS NOT NULL AND taker <> ''
           ORDER BY ledger DESC, tx_hash DESC, op_index DESC
           LIMIT $2
        ),
        legs AS (
          SELECT s.*,
                 CASE
                   WHEN s.usd_volume IS NOT NULL
                    AND ((s.base_asset IN ('native', $3::text) AND s.quote_asset NOT IN (` + usdProxyQuotes + `))
                      OR (s.quote_asset IN ('native', $3::text) AND s.base_asset NOT IN (` + usdProxyQuotes + `)))
                    AND ` + xlmUSDFreezeOverlap(mevStampMinute+" - INTERVAL '"+xlmUSDAnchorMaxAge+"'", mevStampMinute, "$3::text") + `
                     THEN NULL
                   ELSE s.usd_volume
                 END AS usd_volume_trusted
            FROM scan s
        )
        SELECT t.source, t.ledger, t.tx_hash, t.op_index, t.ts,
               t.base_asset, t.quote_asset,
               t.base_amount, t.quote_amount,
               COALESCE(t.maker, ''), COALESCE(t.taker, ''),
               COALESCE((COALESCE(
                 t.usd_volume_trusted,
                 CASE
                   WHEN t.base_asset IN ('native', $3::text)
                     THEN (t.base_amount / 1e7::numeric) * xa.vwap
                   WHEN t.quote_asset IN ('native', $3::text)
                     THEN (t.quote_amount / 1e7::numeric) * xa.vwap
                   ELSE NULL
                 END
               ))::text, ''),
               t.usd_volume_trusted IS NULL
          FROM legs t
          ` + xlmUSDGridJoin("xa", "date_trunc('minute', t.ts)") + `
         ORDER BY t.ledger DESC, t.tx_hash DESC, t.op_index DESC
    `

// TradesForArbScan returns the NEWEST `limit` on-chain trades (ledger > 0, with a taker) after `since`, ascending by (ledger, tx_hash, op_index), plus each trade's USD value ("" when none; Estimated when it came from the XLM/USD anchor).
func (s *Store) TradesForArbScan(ctx context.Context, since time.Time, limit int) ([]canonical.Trade, []domain.MEVLegUSD, error) {
	if limit <= 0 {
		limit = 50_000
	}
	rows, err := s.db.QueryContext(ctx, tradesForArbScanQuery, since.UTC(), limit, canonical.NativeSACContractID())
	if err != nil {
		return nil, nil, fmt.Errorf("timescale: TradesForArbScan: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var (
		trades []canonical.Trade
		usd    []domain.MEVLegUSD
	)
	for rows.Next() {
		var (
			t                     canonical.Trade
			baseAsset, quoteAsset string
			usdVol                string
			estimated             bool
		)
		if err := rows.Scan(
			&t.Source, &t.Ledger, &t.TxHash, &t.OpIndex, &t.Timestamp,
			&baseAsset, &quoteAsset,
			&t.BaseAmount, &t.QuoteAmount,
			&t.Maker, &t.Taker, &usdVol, &estimated,
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
		usd = append(usd, domain.MEVLegUSD{Value: usdVol, Estimated: estimated && usdVol != ""})
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("timescale: TradesForArbScan rows: %w", err)
	}
	slices.Reverse(trades)
	slices.Reverse(usd)
	return trades, usd, nil
}

// oracleUpdatesForMEVScanQuery excludes unmapped `raw:%` oracle symbols (canonical.AssetOracleRaw): record-layer only, never MEV evidence.
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

// OracleUpdatesForMEVScan returns the newest `limit` on-chain, mapped-asset oracle updates after `since`, ascending by ledger; satisfies mev.OracleScanner.
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

// BlendFillsForMEVScan returns the newest `limit` Blend liquidation fills (event_kind='fill', auction_type 0 or 1; interest auctions excluded) after `since`, ascending by ledger; satisfies mev.AuctionScanner.
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

// insertMEVEventQuery is idempotent on dedup_key: a re-scan whose evidence CONTAINS the stored legs replaces them, a smaller one never
// does. detected_at and ledger keep the first detection; RETURNING xmax = 0 is true only for a fresh insert.
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

// InsertMEVEvent persists a detected MEV event and returns inserted=true only for a new one; profit_usd is written NULL since trade notional is not profit; satisfies mev.Sink.
func (s *Store) InsertMEVEvent(ctx context.Context, e domain.MEVStoredEvent) (bool, error) {
	// 0021's CHECK (array_length(tx_hashes, 1) > 0) passes '{}' (the length
	// is NULL), so an event with no evidence transaction must be refused here.
	if len(e.TxHashes) == 0 {
		return false, fmt.Errorf("timescale: InsertMEVEvent: %s event has no tx_hashes", e.Kind)
	}
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

// ListMEVEvents returns the most-recent MEV events, newest first. kind "" returns all kinds; a non-empty kind uses the per-kind index.
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
