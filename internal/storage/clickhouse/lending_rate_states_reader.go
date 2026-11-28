package clickhouse

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/big"
	"time"
)

// errLendingRateWindow is a rate-state read whose window does not open before it closes.
var errLendingRateWindow = errors.New("clickhouse: lending rate states: from must be before to")

// LendingRateState is one reserve's ResData row: the b_rate and d_rate a realised yield is read from.
type LendingRateState struct {
	Ledger    uint32
	CloseTime time.Time
	Removed   bool // the entry was removed; the rates are nil
	// BRate and DRate are at the row's protocol scale (1e12; Blend V1 1e9); LastTime is when they were last accrued.
	BRate, DRate *big.Int
	LastTime     time.Time
}

// LendingRateStates is a reserve's ResData around a window [From, To] read with a base slack: Base is the state in force at
// From when it closed within the slack before From, First the first state at or after From, End the state in force at To.
// Each is nil when no row falls in its range. Prior is true when a row exists before the slack: the reserve had a state
// then, which is too old to be a base. Undecodable names a row that exists but failed to decode (its side is then nil).
type LendingRateStates struct {
	Base, First, End *LendingRateState
	Prior            bool
	Undecodable      bool
}

// LendingRateWindow is every requested reserve's states; a reserve with no row before To is absent.
type LendingRateWindow struct {
	Reserves map[string]LendingRateStates
}

// lendingRateSideSelect is, per key, the last (order DESC) or first (ASC) row whose hour and close_time lie in [lo, hi],
// below the watermark's hour. Args: contract, keys, lo, hi, the watermark hour, lo, hi.
func lendingRateSideSelect(side int, order string) string {
	return fmt.Sprintf(`SELECT %d AS side, key_xdr, protocol, ledger_seq, intra_ledger_seq, close_time, entry_xdr FROM (
		SELECT key_xdr, protocol, hour, ledger_seq, intra_ledger_seq, close_time, entry_xdr
		FROM stellar.lending_state_hourly FINAL
		WHERE contract_id = ? AND key_xdr IN (?) AND hour >= toStartOfHour(toDateTime(?, 'UTC'))
			AND hour <= toDateTime(?, 'UTC') AND hour < toDateTime(?, 'UTC')
	) WHERE close_time >= toDateTime(?, 'UTC') AND close_time <= toDateTime(?, 'UTC')
	ORDER BY key_xdr, hour %s
	LIMIT 1 BY key_xdr`, side, order)
}

// Sides of lendingRateStatesQuery.
const (
	rateSideBase = iota
	rateSideFirst
	rateSideEnd
	rateSidePrior
)

// lendingRatePriorSelect is an existence probe: per key, the newest row at or before lo's hour (a row there closing before
// lo is too old, one closing after is the base, which settles the reason first). It is bounded: one primary-key prefix
// range per key, no entry column, LIMIT 1 BY key, and no FINAL, since a duplicate version cannot change whether a row
// exists. Args: contract, keys, lo, the watermark hour.
const lendingRatePriorSelect = `SELECT 3 AS side, key_xdr, protocol, ledger_seq, intra_ledger_seq, close_time, '' AS entry_xdr
	FROM stellar.lending_state_hourly
	WHERE contract_id = ? AND key_xdr IN (?) AND hour <= toStartOfHour(toDateTime(?, 'UTC')) AND hour < toDateTime(?, 'UTC')
	ORDER BY key_xdr, hour DESC
	LIMIT 1 BY key_xdr`

// lendingRateStatesQuery reads the four sides in one query. The three state sides are bounded below by From less the slack,
// which keeps them exact: a state older than that is never a base (it would average periods before the window), and an End
// older than that means nothing accrued in the window, which an absent End says too. The prior probe only tells "too old"
// from "none". Prewhere stays off so the close_time bound applies after FINAL collapses an hour.
var lendingRateStatesQuery = `SELECT side, key_xdr, protocol, ledger_seq, intra_ledger_seq, close_time, entry_xdr FROM (
		` + lendingRateSideSelect(rateSideBase, "DESC") + `
		UNION ALL
		` + lendingRateSideSelect(rateSideFirst, "ASC") + `
		UNION ALL
		` + lendingRateSideSelect(rateSideEnd, "DESC") + `
		UNION ALL
		` + lendingRatePriorSelect + `
	)
	SETTINGS optimize_move_to_prewhere_if_final = 0`

// LendingRateBaseSlack is how long before a window opens a state may have last accrued and still be its base: an hour (the
// stored grain) or a 24th of the window, whichever is longer, so a quiet reserve's base is found without averaging more than
// about 4% of the window from before it.
func LendingRateBaseSlack(window time.Duration) time.Duration {
	return max(time.Hour, window/24)
}

// LendingHistoryCoverage is how far lending_state_hourly is derived; ErrLendingHistoryNotReady when it never ran.
func (r *ExplorerReader) LendingHistoryCoverage(ctx context.Context) (LendingHistoryCoverage, error) {
	return r.lendingHistoryCoverage(ctx)
}

// LendingRateStates reads the ResData of each asset of pool around [from, to], a base up to slack before from, through the
// hours cov has derived. Each key is read by its primary-key prefix (contract_id, key_xdr), one row per key and side.
func (r *ExplorerReader) LendingRateStates(ctx context.Context, pool string, assets []string, from, to time.Time, slack time.Duration, cov LendingHistoryCoverage) (LendingRateWindow, error) {
	if !from.Before(to) {
		return LendingRateWindow{}, errLendingRateWindow
	}
	keyAsset, keys, err := rateStateKeys(pool, assets)
	if err != nil {
		return LendingRateWindow{}, err
	}
	out := LendingRateWindow{Reserves: map[string]LendingRateStates{}}
	if len(keys) == 0 {
		return out, nil
	}
	lo, f, t, before := from.Add(-slack).Unix(), from.Unix(), to.Unix(), cov.servedBefore()
	rows, err := r.conn.Query(ctx, lendingRateStatesQuery,
		pool, keys, lo, f, before, lo, f,
		pool, keys, f, t, before, f, t,
		pool, keys, lo, t, before, lo, t,
		pool, keys, lo, before)
	if err != nil {
		return LendingRateWindow{}, fmt.Errorf("clickhouse: lending rate states: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var side uint8
		var s stateRow
		if err := rows.Scan(&side, &s.key, &s.protocol, &s.ledger, &s.intra, &s.close, &s.entry); err != nil {
			return LendingRateWindow{}, fmt.Errorf("clickhouse: scan lending rate states: %w", err)
		}
		asset := keyAsset[s.key]
		st := out.Reserves[asset]
		if side == rateSidePrior {
			st.Prior = true
			out.Reserves[asset] = st
			continue
		}
		rs, ok := decodeRateState(s)
		switch {
		case !ok:
			st.Undecodable = true
		case side == rateSideBase:
			st.Base = rs
		case side == rateSideFirst:
			st.First = rs
		default:
			st.End = rs
		}
		out.Reserves[asset] = st
	}
	if err := rows.Err(); err != nil {
		return LendingRateWindow{}, fmt.Errorf("clickhouse: lending rate states rows: %w", err)
	}
	return out, nil
}

// rateStateKeys is the ResData key of each asset of pool, and the asset each key names.
func rateStateKeys(pool string, assets []string) (map[string]string, []string, error) {
	poolID, err := contractIDFromStrkey(pool)
	if err != nil {
		return nil, nil, fmt.Errorf("clickhouse: lending pool id %q: %w", pool, err)
	}
	keyAsset := make(map[string]string, len(assets))
	keys := make([]string, 0, len(assets))
	for _, a := range assets {
		id, err := contractIDFromStrkey(a)
		if err != nil {
			return nil, nil, fmt.Errorf("clickhouse: lending asset id %q: %w", a, err)
		}
		k, err := poolDataKeyXDR(poolID, "ResData", id)
		if err != nil {
			return nil, nil, fmt.Errorf("clickhouse: ResData key: %w", err)
		}
		keyAsset[k] = a
		keys = append(keys, k)
	}
	return keyAsset, keys, nil
}

// decodeRateState decodes one ResData row with its protocol's reserve model; ok=false when it cannot be.
func decodeRateState(row stateRow) (*LendingRateState, bool) {
	st := &LendingRateState{Ledger: row.ledger, CloseTime: row.close.UTC(), Removed: row.entry == ""}
	if st.Removed {
		return st, true
	}
	model, ok := historyModel(row.protocol)
	if !ok {
		return nil, false
	}
	val, ok := contractDataValue(row.entry)
	if !ok {
		return nil, false
	}
	rd, err := model.decode(val)
	if err != nil {
		return nil, false
	}
	if rd.LastTime > math.MaxInt64 {
		return nil, false
	}
	st.BRate, st.DRate, st.LastTime = rd.BRate, rd.DRate, time.Unix(int64(rd.LastTime), 0).UTC()
	return st, true
}
