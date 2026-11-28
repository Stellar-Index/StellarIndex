package clickhouse

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/cometlp"
)

// ErrCometHoursNotCovered means a requested hour is not complete in lending_state_hourly yet.
var ErrCometHoursNotCovered = errors.New("clickhouse: comet pool hours past the lending state watermark")

// ErrCometPoolNotWalked means lending_state_hourly was never walked for the pool on this host: run the scoped walk
// (deploy/clickhouse/lending_state.sql).
var ErrCometPoolNotWalked = errors.New("clickhouse: comet pool state not walked yet")

// cometHistoryMaxHours bounds one CometPoolStates read (92 days).
const cometHistoryMaxHours = 92 * 24

// CometPoolHour is a Comet pool's state at the close of one UTC hour: each key's last change at or before it, carried
// forward through hours with no change. State is nil before the pool's first state or after an entry's removal, and
// Missing then names the first absent key.
type CometPoolHour struct {
	Hour    time.Time // the hour's start
	Ledger  uint32    // the newest of the three entries' ledgers
	State   *cometlp.PoolState
	Missing string
}

// cometHistoryKeys is a pool's AllRecordData, TotalShares and persistent instance key, as the lending derive captures them.
func cometHistoryKeys(pool string) ([]string, error) {
	id, err := contractIDFromStrkey(pool)
	if err != nil {
		return nil, fmt.Errorf("clickhouse: comet pool %q: %w", pool, err)
	}
	rec, err := lendingUnitKey(id, cometlp.KeyAllRecordData)
	if err != nil {
		return nil, err
	}
	shares, err := lendingUnitKey(id, cometlp.KeyTotalShares)
	if err != nil {
		return nil, err
	}
	inst, err := instanceKeyXDR(xdr.Hash(id))
	if err != nil {
		return nil, err
	}
	return []string{rec, shares, inst[0]}, nil
}

// CometPoolStateAt is the pool's state at the close of hour's UTC hour.
func (r *ExplorerReader) CometPoolStateAt(ctx context.Context, pool string, hour time.Time) (CometPoolHour, error) {
	h := hour.UTC().Truncate(time.Hour)
	hs, err := r.CometPoolStates(ctx, pool, h, h.Add(time.Hour))
	if err != nil {
		return CometPoolHour{}, err
	}
	return hs[0], nil
}

// CometPoolStates is the pool's state at the close of every UTC hour in [from, to), both truncated to the hour. Only a
// curated pool is read, every hour must be complete under the lending_state watermark, and an entry that is not the
// audited layout fails the read.
func (r *ExplorerReader) CometPoolStates(ctx context.Context, pool string, from, to time.Time) ([]CometPoolHour, error) {
	if _, ok := cometlp.AuditedPoolWASM(pool); !ok {
		return nil, fmt.Errorf("clickhouse: %w: %s", cometlp.ErrNotGatedPool, pool)
	}
	from, to = from.UTC().Truncate(time.Hour), to.UTC().Truncate(time.Hour)
	n := int(to.Sub(from) / time.Hour)
	if n <= 0 || n > cometHistoryMaxHours {
		return nil, fmt.Errorf("clickhouse: comet pool states [%s, %s): %d hours, want 1..%d", from, to, n, cometHistoryMaxHours)
	}
	cov, err := r.lendingHistoryCoverage(ctx)
	if err != nil {
		return nil, err
	}
	before := cov.servedBefore()
	if to.Unix() > before {
		return nil, fmt.Errorf("%w: [%s, %s) ends after %s", ErrCometHoursNotCovered, from, to, time.Unix(before, 0).UTC())
	}
	keys, err := cometHistoryKeys(pool)
	if err != nil {
		return nil, err
	}
	if err := r.requireCometWalked(ctx, pool, keys[2], cov.ThroughLedger); err != nil {
		return nil, err
	}
	// Closes are whole seconds: the state at an hour's close is every change at or before its last second.
	seedAt, last := from.Unix()-1, to.Unix()-1
	seed, err := r.lendingStateRows(ctx, lendingHistorySeedQuery, pool, keys, seedAt, before, seedAt)
	if err != nil {
		return nil, err
	}
	rng, err := r.lendingStateRows(ctx, lendingHistoryRangeQuery, pool, keys, seedAt, last, before, seedAt, last, uint8(0), uint64(n)) //nolint:gosec // n is bounded
	if err != nil {
		return nil, err
	}
	return foldCometHours(pool, keys, from, n, seed, rng)
}

// cometInstanceLedgerQuery is the newest ledger of a pool's instance rows (0 when it has none).
const cometInstanceLedgerQuery = `SELECT max(ledger_seq) FROM stellar.lending_state_hourly WHERE contract_id = ? AND key_xdr = ?`

// requireCometWalked refuses a pool created at or below the watermark whose instance has no row at or after its creation
// ledger: every pool writes its instance at creation, so the history was never walked for it (the rule
// LendingInterestUnderivedPools applies to lending pools), and an absent state would read as "did not exist".
func (r *ExplorerReader) requireCometWalked(ctx context.Context, pool, instanceKey string, wm uint32) error {
	deployed, _ := cometlp.PoolDeployed(pool)
	if wm < deployed {
		return nil
	}
	var last uint32
	if err := r.conn.QueryRow(ctx, cometInstanceLedgerQuery, pool, instanceKey).Scan(&last); err != nil {
		return fmt.Errorf("clickhouse: comet %s instance rows: %w", pool, err)
	}
	if last < deployed {
		return fmt.Errorf("%w: %s (created at %d) has no instance row at or after it", ErrCometPoolNotWalked, pool, deployed)
	}
	return nil
}

// foldCometHours carries each key's row forward hour by hour from the seed, applying the rows of each hour at its close.
func foldCometHours(pool string, keys []string, from time.Time, n int, seed, rng []stateRow) ([]CometPoolHour, error) {
	cur := map[string]stateRow{}
	for _, row := range seed {
		cur[row.key] = row
	}
	byHour := map[int64][]stateRow{}
	for _, row := range rng {
		h := row.close.UTC().Truncate(time.Hour).Unix()
		byHour[h] = append(byHour[h], row)
	}
	out := make([]CometPoolHour, 0, n)
	for i := range n {
		h := from.Add(time.Duration(i) * time.Hour)
		for _, row := range byHour[h.Unix()] {
			cur[row.key] = row
		}
		pt, err := cometPoolHour(pool, keys, h, cur)
		if err != nil {
			return nil, err
		}
		out = append(out, pt)
	}
	return out, nil
}

var cometKeyNames = [3]string{cometlp.KeyAllRecordData, cometlp.KeyTotalShares, "Instance"}

// cometPoolHour decodes the rows in force at h's close into the pool's state.
func cometPoolHour(pool string, keys []string, h time.Time, cur map[string]stateRow) (CometPoolHour, error) {
	pt := CometPoolHour{Hour: h}
	var vals [3]xdr.ScVal
	for i, k := range keys {
		row, ok := cur[k]
		switch {
		case !ok:
			pt.Missing = cometKeyNames[i] + " not recorded"
		case row.entry == "":
			pt.Missing = cometKeyNames[i] + " removed"
		}
		if pt.Missing != "" {
			return CometPoolHour{Hour: h, Missing: pt.Missing}, nil
		}
		v, err := cometEntryVal(pool, row.entry)
		if err != nil {
			return CometPoolHour{}, fmt.Errorf("clickhouse: comet %s %s at ledger %d: %w", pool, cometKeyNames[i], row.ledger, err)
		}
		vals[i], pt.Ledger = v, max(pt.Ledger, row.ledger)
	}
	st, err := cometlp.NewPoolState(pool, vals[0], vals[1], vals[2])
	if err != nil {
		return CometPoolHour{}, fmt.Errorf("clickhouse: comet %s at %s: %w", pool, h, err)
	}
	pt.State = &st
	return pt, nil
}

// cometEntryVal is a contract_data entry's value, refused unless the entry belongs to pool.
func cometEntryVal(pool, b64 string) (xdr.ScVal, error) {
	var entry xdr.LedgerEntry
	if err := xdr.SafeUnmarshalBase64(b64, &entry); err != nil {
		return xdr.ScVal{}, fmt.Errorf("%w: %w", cometlp.ErrMalformedStorage, err)
	}
	cd, ok := entry.Data.GetContractData()
	if !ok || cd.Contract.Type != xdr.ScAddressTypeScAddressTypeContract || cd.Contract.ContractId == nil {
		return xdr.ScVal{}, fmt.Errorf("%w: not a contract's contract_data entry", cometlp.ErrMalformedStorage)
	}
	id, err := contractIDFromStrkey(pool)
	if err != nil {
		return xdr.ScVal{}, err
	}
	if *cd.Contract.ContractId != id {
		return xdr.ScVal{}, fmt.Errorf("%w: entry belongs to another contract than %s", cometlp.ErrNotGatedPool, pool)
	}
	return cd.Val, nil
}
