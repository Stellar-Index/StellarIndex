package clickhouse

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/stellar/go-stellar-sdk/xdr"
)

// ErrLendingHistoryNotReady means lending_state_hourly has no watermark: the derive has never run on this host.
var ErrLendingHistoryNotReady = errors.New("clickhouse: lending state history not derived yet")

// ErrLendingReserveNotFound means the pool has no ResData row for the asset at all: no such reserve, as far as the history knows.
var ErrLendingReserveNotFound = errors.New("clickhouse: no lending reserve state recorded")

// ErrLendingHistoryTooManyPoints means one series holds more than LendingHistoryQuery.MaxPoints points.
var ErrLendingHistoryTooManyPoints = errors.New("clickhouse: lending history exceeds the point cap")

// LendingHistoryQuery bounds a lending history read: (From, To] plus the state in force at From.
type LendingHistoryQuery struct {
	From, To  time.Time
	Daily     bool // one point per UTC day (the day's last hourly row) instead of per hour
	MaxPoints int  // per series, excluding the seed
}

// Aligned rounds From down to the start of its bucket (UTC hour or day), so the seed is the state in force when the first
// bucket opens: a bucket stores only its last change, never the state at a time inside it.
func (q LendingHistoryQuery) Aligned() LendingHistoryQuery {
	f := q.From.UTC()
	if q.Daily {
		q.From = time.Date(f.Year(), f.Month(), f.Day(), 0, 0, 0, 0, time.UTC)
	} else {
		q.From = f.Truncate(time.Hour)
	}
	return q
}

// LendingHistoryCoverage is how far lending_state_hourly is complete.
type LendingHistoryCoverage struct {
	ThroughLedger uint32
	ThroughTime   time.Time
}

// servedBefore is the start of the watermark's hour; only hours before it are read.
func (c LendingHistoryCoverage) servedBefore() int64 {
	return c.ThroughTime.UTC().Truncate(time.Hour).Unix()
}

// LendingReservePoint is a reserve's state at the close of a bucket in which it changed.
type LendingReservePoint struct {
	CloseTime    time.Time
	StateLedger  uint32
	ConfigLedger uint32 // ledger of the ResConfig in force; 0 when none is or it is unknown
	Removed      bool   // the ResData entry was removed; State is zero
	// ConfigUncertain: the ResConfig changed later in this state's hour, so the one in force is unknown; State has no config.
	ConfigUncertain bool
	// PoolConfigUncertain: the pool instance changed later in this state's hour; State's SupplyAPR is not valid.
	PoolConfigUncertain bool
	State               BlendReserveState
}

// LendingReserveHistory is one reserve's state series.
type LendingReserveHistory struct {
	Coverage LendingHistoryCoverage
	Protocol string // of the newest row read; "" when none
	// BeforeFirstState is true when no state is recorded at or before the aligned From.
	BeforeFirstState bool
	Points           []LendingReservePoint
	Undecodable      int // rows dropped because an entry in them failed to decode
}

// LendingBackstopPoint is one backstop tier's balance at the close of a bucket in which it changed.
type LendingBackstopPoint struct {
	CloseTime time.Time
	Ledger    uint32
	Removed   bool
	Balance   BackstopBalance // zero when Removed
}

// LendingBackstopTierHistory is one backstop tier's series; Tier is "" for Blend's single tier.
type LendingBackstopTierHistory struct {
	Tier             string
	BeforeFirstState bool
	Points           []LendingBackstopPoint
}

// LendingBackstopHistory is a pool's backstop series per tier; Backstop is "" when no pool instance names one.
type LendingBackstopHistory struct {
	Coverage    LendingHistoryCoverage
	Backstop    string
	Tiers       []LendingBackstopTierHistory
	Undecodable int
}

// stateRow is one lending_state_hourly row; entry "" is a removal.
type stateRow struct {
	key      string
	protocol string
	ledger   uint32
	intra    uint32
	close    time.Time
	entry    string
}

func compareVersion(a, b stateRow) int {
	return cmp.Or(cmp.Compare(a.ledger, b.ledger), cmp.Compare(a.intra, b.intra))
}

// lendingHistoryRangeQuery reads (from, to] of some keys, one row per bucket. Filters on non-key columns sit outside
// the FINAL subquery and prewhere is off so they apply after the collapse: an older version must not resurface.
// Only hours before the watermark's hour are read: a derive in flight may already hold a newer version of that hour, which
// FINAL would keep over the served one; every earlier hour holds only ledgers at or below the watermark.
const lendingHistoryRangeQuery = `SELECT key_xdr, protocol, ledger_seq, intra_ledger_seq, close_time, entry_xdr FROM (
		SELECT key_xdr, protocol, hour, ledger_seq, intra_ledger_seq, close_time, entry_xdr FROM (
			SELECT key_xdr, protocol, hour, ledger_seq, intra_ledger_seq, close_time, entry_xdr
			FROM stellar.lending_state_hourly FINAL
			WHERE contract_id = ? AND key_xdr IN (?) AND hour >= toStartOfHour(toDateTime(?, 'UTC')) AND hour <= toDateTime(?, 'UTC')
				AND hour < toDateTime(?, 'UTC')
		) WHERE close_time > toDateTime(?, 'UTC') AND close_time <= toDateTime(?, 'UTC')
		ORDER BY key_xdr, hour DESC
		LIMIT 1 BY key_xdr, if(? = 1, toStartOfDay(hour), hour)
	)
	ORDER BY key_xdr, ledger_seq, intra_ledger_seq
	LIMIT ? BY key_xdr
	SETTINGS optimize_move_to_prewhere_if_final = 0`

// lendingHistorySeedQuery is each key's last row at or before from: the state in force when the window opens. The same
// watermark-hour bound as lendingHistoryRangeQuery applies.
const lendingHistorySeedQuery = `SELECT key_xdr, protocol, ledger_seq, intra_ledger_seq, close_time, entry_xdr FROM (
		SELECT key_xdr, protocol, hour, ledger_seq, intra_ledger_seq, close_time, entry_xdr
		FROM stellar.lending_state_hourly FINAL
		WHERE contract_id = ? AND key_xdr IN (?) AND hour <= toDateTime(?, 'UTC') AND hour < toDateTime(?, 'UTC')
	) WHERE close_time <= toDateTime(?, 'UTC')
	ORDER BY key_xdr, hour DESC
	LIMIT 1 BY key_xdr
	SETTINGS optimize_move_to_prewhere_if_final = 0`

// lendingHistoryKeyRowsQuery counts a key's rows of any version in the served hours.
const lendingHistoryKeyRowsQuery = `SELECT count() FROM stellar.lending_state_hourly
	WHERE contract_id = ? AND key_xdr = ? AND hour < toDateTime(?, 'UTC')`

// lendingHistoryCoverage reads the derive watermark; ErrLendingHistoryNotReady when it never ran or the lake has no close time
// for it, since the served hours are bounded by that time.
func (r *ExplorerReader) lendingHistoryCoverage(ctx context.Context) (LendingHistoryCoverage, error) {
	wm, err := entryWatermarkOn(ctx, r.conn, lendingStateWatermarkName)
	if err != nil {
		return LendingHistoryCoverage{}, err
	}
	if wm == 0 {
		return LendingHistoryCoverage{}, ErrLendingHistoryNotReady
	}
	at, found, err := r.CloseTimeForLedger(ctx, wm)
	if err != nil {
		return LendingHistoryCoverage{}, err
	}
	if !found {
		return LendingHistoryCoverage{}, ErrLendingHistoryNotReady
	}
	return LendingHistoryCoverage{ThroughLedger: wm, ThroughTime: at}, nil
}

func (r *ExplorerReader) lendingStateRows(ctx context.Context, query string, args ...any) ([]stateRow, error) {
	rows, err := r.conn.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("clickhouse: lending state history: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []stateRow
	for rows.Next() {
		var s stateRow
		if err := rows.Scan(&s.key, &s.protocol, &s.ledger, &s.intra, &s.close, &s.entry); err != nil {
			return nil, fmt.Errorf("clickhouse: scan lending state history: %w", err)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("clickhouse: lending state history rows: %w", err)
	}
	return out, nil
}

// readSeries reads the seed and the bucketed range of keys under one contract, capped at the watermark's hour.
func (r *ExplorerReader) readSeries(ctx context.Context, contract string, keys []string, q LendingHistoryQuery, cov LendingHistoryCoverage) (seed, rng []stateRow, err error) {
	q = q.Aligned()
	from, to, before := q.From.Unix(), q.To.Unix(), cov.servedBefore()
	seed, err = r.lendingStateRows(ctx, lendingHistorySeedQuery, contract, keys, from, before, from)
	if err != nil {
		return nil, nil, err
	}
	var daily uint8
	if q.Daily {
		daily = 1
	}
	rng, err = r.lendingStateRows(ctx, lendingHistoryRangeQuery, contract, keys, from, to, before, from, to, daily, q.MaxPoints+1)
	if err != nil {
		return nil, nil, err
	}
	perKey := map[string]int{}
	for _, row := range rng {
		if perKey[row.key]++; perKey[row.key] > q.MaxPoints {
			return nil, nil, ErrLendingHistoryTooManyPoints
		}
	}
	return seed, rng, nil
}

// readInForce reads every row of keys up to To: the reserve config and instance versions a point pairs with.
func (r *ExplorerReader) readInForce(ctx context.Context, contract string, keys []string, to time.Time, cov LendingHistoryCoverage) ([]stateRow, error) {
	rows, err := r.lendingStateRows(ctx, lendingHistoryRangeQuery, contract, keys, int64(0), to.Unix(), cov.servedBefore(), int64(0), to.Unix(), uint8(0), uint64(1)<<32)
	if err != nil {
		return nil, err
	}
	slices.SortStableFunc(rows, compareVersion)
	return rows, nil
}

// LendingReserveHistory reads one pool reserve's ResData series, decoded with the live endpoint's model and paired with the
// ResConfig and pool instance in force at each point.
func (r *ExplorerReader) LendingReserveHistory(ctx context.Context, pool, asset string, q LendingHistoryQuery) (LendingReserveHistory, error) {
	poolID, err := contractIDFromStrkey(pool)
	if err != nil {
		return LendingReserveHistory{}, fmt.Errorf("clickhouse: lending pool id %q: %w", pool, err)
	}
	assetID, err := contractIDFromStrkey(asset)
	if err != nil {
		return LendingReserveHistory{}, fmt.Errorf("clickhouse: lending asset id %q: %w", asset, err)
	}
	dataKey, err := poolDataKeyXDR(poolID, "ResData", assetID)
	if err != nil {
		return LendingReserveHistory{}, fmt.Errorf("clickhouse: ResData key: %w", err)
	}
	cfgKey, err := poolDataKeyXDR(poolID, "ResConfig", assetID)
	if err != nil {
		return LendingReserveHistory{}, fmt.Errorf("clickhouse: ResConfig key: %w", err)
	}
	instKeys, err := instanceKeyXDR(xdr.Hash(poolID))
	if err != nil {
		return LendingReserveHistory{}, fmt.Errorf("clickhouse: lending pool instance key: %w", err)
	}
	cov, err := r.lendingHistoryCoverage(ctx)
	if err != nil {
		return LendingReserveHistory{}, err
	}
	seed, rng, err := r.readSeries(ctx, pool, []string{dataKey}, q, cov)
	if err != nil {
		return LendingReserveHistory{}, err
	}
	if len(seed) == 0 && len(rng) == 0 {
		if err := r.requireKeyRows(ctx, pool, dataKey, cov); err != nil {
			return LendingReserveHistory{}, err
		}
	}
	inForce, err := r.readInForce(ctx, pool, append([]string{cfgKey}, instKeys...), q.To, cov)
	if err != nil {
		return LendingReserveHistory{}, err
	}
	cfgRows, instRows := splitByKey(inForce, cfgKey)
	out := assembleReserveHistory(pool, asset, append(seed, rng...), cfgRows, instRows)
	out.Coverage, out.BeforeFirstState = cov, len(seed) == 0
	return out, nil
}

// requireKeyRows returns ErrLendingReserveNotFound when key has no row in the served hours.
func (r *ExplorerReader) requireKeyRows(ctx context.Context, contract, key string, cov LendingHistoryCoverage) error {
	var n uint64
	if err := r.conn.QueryRow(ctx, lendingHistoryKeyRowsQuery, contract, key, cov.servedBefore()).Scan(&n); err != nil {
		return fmt.Errorf("clickhouse: lending reserve rows: %w", err)
	}
	if n == 0 {
		return ErrLendingReserveNotFound
	}
	return nil
}

// splitByKey separates the in-force rows into the ResConfig rows and the instance rows, each in version order.
func splitByKey(rows []stateRow, cfgKey string) (cfg, inst []stateRow) {
	for _, row := range rows {
		if row.key == cfgKey {
			cfg = append(cfg, row)
		} else {
			inst = append(inst, row)
		}
	}
	return cfg, inst
}

// inForce returns the newest row whose version is at or below at's; a tie is the same transaction, whose end state holds both.
// uncertain is true when a later row falls in at's hour: only an hour's last row is kept, so the one in force at at is unknown.
func inForce(rows []stateRow, at stateRow) (row *stateRow, uncertain bool) {
	i, _ := slices.BinarySearchFunc(rows, at, func(e, t stateRow) int {
		if compareVersion(e, t) <= 0 {
			return -1
		}
		return 1
	})
	uncertain = i < len(rows) && rows[i].close.Truncate(time.Hour).Equal(at.close.Truncate(time.Hour))
	if i == 0 {
		return nil, uncertain
	}
	return &rows[i-1], uncertain
}

// reserveHistoryIn is the ResConfig and instance versions a reserve's points pair with, in version order.
type reserveHistoryIn struct {
	cfg, inst []stateRow
}

// assembleReserveHistory decodes data (seed first, then the range, in version order) into points.
func assembleReserveHistory(pool, asset string, data, cfgRows, instRows []stateRow) LendingReserveHistory {
	in := reserveHistoryIn{cfg: cfgRows, inst: instRows}
	var out LendingReserveHistory
	for _, row := range data {
		out.Protocol = row.protocol
		p, ok := in.point(pool, asset, row)
		if !ok {
			out.Undecodable++
			continue
		}
		out.Points = append(out.Points, p)
	}
	return out
}

// LendingBackstopHistory reads a pool's backstop balance series per tier from the backstop its newest instance row names.
func (r *ExplorerReader) LendingBackstopHistory(ctx context.Context, pool string, tiered bool, q LendingHistoryQuery) (LendingBackstopHistory, error) {
	poolID, err := contractIDFromStrkey(pool)
	if err != nil {
		return LendingBackstopHistory{}, fmt.Errorf("clickhouse: lending pool id %q: %w", pool, err)
	}
	instKeys, err := instanceKeyXDR(xdr.Hash(poolID))
	if err != nil {
		return LendingBackstopHistory{}, fmt.Errorf("clickhouse: lending pool instance key: %w", err)
	}
	cov, err := r.lendingHistoryCoverage(ctx)
	if err != nil {
		return LendingBackstopHistory{}, err
	}
	insts, err := r.lendingStateRows(ctx, lendingHistorySeedQuery, pool, instKeys, q.To.Unix(), cov.servedBefore(), q.To.Unix())
	if err != nil {
		return LendingBackstopHistory{}, err
	}
	out := LendingBackstopHistory{Coverage: cov}
	for _, row := range insts {
		if v, ok := contractDataValue(row.entry); ok {
			if pi, ok := poolInstanceFromStorage(v); ok && pi.Backstop != "" {
				out.Backstop = pi.Backstop
			}
		}
	}
	if out.Backstop == "" {
		return out, nil
	}
	backstopID, err := contractIDFromStrkey(out.Backstop)
	if err != nil {
		return LendingBackstopHistory{}, fmt.Errorf("clickhouse: backstop id %q: %w", out.Backstop, err)
	}
	_, refByKey, err := backstopKeys(backstopID, poolID, tiered)
	if err != nil {
		return LendingBackstopHistory{}, err
	}
	tierKey := map[string]string{}
	for k, ref := range refByKey {
		if ref.kind == backstopKindBalance {
			tierKey[ref.asset] = k
		}
	}
	keys := make([]string, 0, len(tierKey))
	for _, k := range tierKey {
		keys = append(keys, k)
	}
	seed, rng, err := r.readSeries(ctx, out.Backstop, keys, q, cov)
	if err != nil {
		return LendingBackstopHistory{}, err
	}
	tiers := []string{""}
	if tiered {
		tiers = blntBackstopTiers
	}
	out.Tiers, out.Undecodable = assembleBackstopHistory(tiers, tierKey, seed, rng)
	return out, nil
}

func assembleBackstopHistory(tiers []string, tierKey map[string]string, seed, rng []stateRow) ([]LendingBackstopTierHistory, int) {
	out := make([]LendingBackstopTierHistory, 0, len(tiers))
	undecodable := 0
	for _, tier := range tiers {
		key := tierKey[tier]
		th := LendingBackstopTierHistory{Tier: tier, BeforeFirstState: !slices.ContainsFunc(seed, func(s stateRow) bool { return s.key == key })}
		for _, row := range slices.Concat(seed, rng) {
			if row.key != key {
				continue
			}
			p, ok := backstopPoint(row)
			if !ok {
				undecodable++
				continue
			}
			th.Points = append(th.Points, p)
		}
		out = append(out, th)
	}
	return out, undecodable
}

func backstopPoint(row stateRow) (LendingBackstopPoint, bool) {
	p := LendingBackstopPoint{CloseTime: row.close.UTC(), Ledger: row.ledger, Removed: row.entry == ""}
	if p.Removed {
		return p, true
	}
	v, ok := contractDataValue(row.entry)
	if !ok {
		return p, false
	}
	b, err := decodeBackstopBalance(v)
	if err != nil {
		return p, false
	}
	p.Balance = b
	return p, true
}
