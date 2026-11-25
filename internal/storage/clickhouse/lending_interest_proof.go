package clickhouse

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

// ErrLendingInterestSeedUnproved reports a seed lending_state_hourly gave that is not its key's last stored version
// before the window: rows are missing (a lending_state replay deleted them and has not yet rewritten them) or extra.
var ErrLendingInterestSeedUnproved = errors.New("clickhouse: lending interest seed is not the lake's last version before the window")

// lendingInterestCurrentQuery reads each key's latest change ledger from the lake's current-state projection: a point
// lookup on its (entry_type, key_xdr) sort key. It proves only that no later ledger changed the key: after a lake
// re-derive renumbers a ledger, its stale row there can outrank the new last one.
const lendingInterestCurrentQuery = `SELECT key_xdr, ledger_seq FROM stellar.ledger_entries_current FINAL
	WHERE entry_type = 'contract_data' AND key_xdr IN ?`

// lendingInterestLastVersionSelect is each key's last stored version in the versions read's apply order, a removal's
// image empty as lending_state stores it, and whether one change sits at two intra_ledger_seq (a renumbering leftover).
const lendingInterestLastVersionSelect = `SELECT key_xdr, last.1, last.2, last.3, renumbered FROM (
		SELECT key_xdr,
			argMax((ledger_seq, intra_ledger_seq, if(change_type = 'removed', '', entry_xdr)),
				(ledger_seq, intra_ledger_seq, tx_hash, op_index, change_index)) AS last,
			uniqExact(tx_hash, op_index, change_type, entry_xdr) !=
				uniqExact(tx_hash, op_index, change_type, entry_xdr, intra_ledger_seq) AS renumbered
		FROM stellar.ledger_entry_changes FINAL
		WHERE entry_type = 'contract_data' AND change_type != 'state' AND key_xdr IN ? AND `

// lendingInterestSeedLedgerQuery reads each key's versions at its own seed ledger only.
const lendingInterestSeedLedgerQuery = lendingInterestLastVersionSelect + `ledger_seq IN ?
			AND ledger_seq = transform(key_xdr, CAST(? AS Array(String)), CAST(? AS Array(UInt32)), toUInt32(0))
		GROUP BY key_xdr)`

// lendingInterestAfterSeedQuery finds one change of a bucket's keys after each key's own seed ledger, within [lo, hi].
// An existence read needs no FINAL (a duplicate row is the same change), so the key_xdr bloom index prunes it.
const lendingInterestAfterSeedQuery = `SELECT key_xdr, ledger_seq, intra_ledger_seq FROM stellar.ledger_entry_changes
	WHERE entry_type = 'contract_data' AND change_type != 'state' AND key_xdr IN ? AND ledger_seq BETWEEN ? AND ?
		AND ledger_seq > transform(key_xdr, CAST(? AS Array(String)), CAST(? AS Array(UInt32)), toUInt32(0))
	LIMIT 1`

// lendingInterestProofSeconds bounds the proof's lake reads inside the 10-minute timer cadence.
const lendingInterestProofSeconds = 300

const (
	// lendingInterestSeedBucketLedgers is how far below its own seed a key is read for sharing a bucket: a day.
	lendingInterestSeedBucketLedgers = 17_280
	// lendingInterestSeedBucketsMax caps the after-seed reads per proof; the oldest seeds share the last bucket.
	lendingInterestSeedBucketsMax = 16
)

// lendingInterestLakeVersion is a key's last stored lake version.
type lendingInterestLakeVersion struct {
	Ledger, Intra uint32
	Entry         string
	Renumbered    bool
}

// proveLendingInterestSeeds proves each seed row is its key's last stored version before ledger before, by
// (ledger_seq, intra_ledger_seq) and image, against the lake alone: every key's last version at its seed ledger is
// the seed, and a key whose latest change ledger (ledger_entries_current) is not its seed's has no change after that
// ledger and below before. That read runs per bucket of keys with close seeds, so no old seed widens another's.
func proveLendingInterestSeeds(ctx context.Context, conn driver.Conn, got []lendingInterestSeedRow, before uint32) error {
	if len(got) == 0 {
		return nil
	}
	list := make([]any, 0, len(got))
	for _, r := range got {
		if r.Ledger >= before {
			return fmt.Errorf("%w: key %s: seed at ledger %d is not before ledger %d", ErrLendingInterestSeedUnproved, r.Key, r.Ledger, before)
		}
		list = append(list, r.Key)
	}
	current, err := lendingInterestKeyLedgers(ctx, conn, lendingInterestCurrentQuery, clickhouse.GroupSet{Value: list})
	if err != nil {
		return err
	}
	var scan []lendingInterestSeedRow
	for _, r := range got {
		if current[r.Key] != r.Ledger {
			scan = append(scan, r)
		}
	}
	ctx = clickhouse.Context(ctx, clickhouse.WithSettings(clickhouse.Settings{
		"max_execution_time": lendingInterestProofSeconds, "do_not_merge_across_partitions_select_final": 1,
	}))
	keys, ledgers := lendingInterestSeedArgs(got)
	set := make([]any, 0, len(ledgers))
	for _, l := range ledgers {
		set = append(set, l)
	}
	if err := matchLendingInterestSeeds(ctx, conn, got, before, lendingInterestSeedLedgerQuery,
		lendingInterestGroup(keys), clickhouse.GroupSet{Value: set}, keys, ledgers); err != nil {
		return err
	}
	for _, b := range lendingInterestSeedBuckets(scan) {
		if err := noLendingInterestChangeAfterSeed(ctx, conn, b, before); err != nil {
			return err
		}
	}
	return nil
}

// lendingInterestSeedBuckets groups seeds newest first into buckets whose seeds lie within
// lendingInterestSeedBucketLedgers of the bucket's newest; past lendingInterestSeedBucketsMax the rest share the last.
func lendingInterestSeedBuckets(rows []lendingInterestSeedRow) [][]lendingInterestSeedRow {
	sorted := slices.Clone(rows)
	slices.SortStableFunc(sorted, func(a, b lendingInterestSeedRow) int { return cmp.Compare(b.Ledger, a.Ledger) })
	var out [][]lendingInterestSeedRow
	for _, r := range sorted {
		n := len(out)
		if n > 0 && (n == lendingInterestSeedBucketsMax || out[n-1][0].Ledger-r.Ledger <= lendingInterestSeedBucketLedgers) {
			out[n-1] = append(out[n-1], r)
			continue
		}
		out = append(out, []lendingInterestSeedRow{r})
	}
	return out
}

// noLendingInterestChangeAfterSeed refuses a bucket's seed when the lake holds a change of its key after the seed
// ledger and below before. bucket is newest first, so its last seed is its lowest.
func noLendingInterestChangeAfterSeed(ctx context.Context, conn driver.Conn, bucket []lendingInterestSeedRow, before uint32) error {
	lo, hi := bucket[len(bucket)-1].Ledger+1, before-1
	if lo > hi {
		return nil
	}
	keys, ledgers := lendingInterestSeedArgs(bucket)
	rows, err := conn.Query(ctx, lendingInterestAfterSeedQuery, lendingInterestGroup(keys), lo, hi, keys, ledgers)
	if err != nil {
		return lendingInterestBucketError(keys, lo, hi, err)
	}
	defer func() { _ = rows.Close() }()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return lendingInterestBucketError(keys, lo, hi, err)
		}
		return nil
	}
	var k string
	var l, intra uint32
	if err := rows.Scan(&k, &l, &intra); err != nil {
		return lendingInterestBucketError(keys, lo, hi, err)
	}
	i := slices.Index(keys, k)
	if i < 0 {
		return lendingInterestBucketError(keys, lo, hi, fmt.Errorf("the read returned key %s, not one asked for", k))
	}
	seed := bucket[i]
	return fmt.Errorf("%w: key %s: lending_state_hourly's last row before ledger %d is (%d, %d), the lake holds a later "+
		"version at (%d, %d); finish the lending_state replay, then re-derive interest from its first ledger",
		ErrLendingInterestSeedUnproved, k, before, seed.Ledger, seed.Intra, l, intra)
}

// lendingInterestBucketError names the keys and the ledger range a failed after-seed read covered.
func lendingInterestBucketError(keys []string, lo, hi uint32, err error) error {
	return fmt.Errorf("clickhouse: lending interest seed proof: no change after the seed of keys %s over ledgers [%d, %d] "+
		"(limit %ds): %w", strings.Join(keys, ","), lo, hi, lendingInterestProofSeconds, err)
}

func lendingInterestSeedArgs(rows []lendingInterestSeedRow) ([]string, []uint32) {
	keys, ledgers := make([]string, 0, len(rows)), make([]uint32, 0, len(rows))
	for _, r := range rows {
		keys, ledgers = append(keys, r.Key), append(ledgers, r.Ledger)
	}
	return keys, ledgers
}

func lendingInterestGroup(keys []string) clickhouse.GroupSet {
	v := make([]any, 0, len(keys))
	for _, k := range keys {
		v = append(v, k)
	}
	return clickhouse.GroupSet{Value: v}
}

// matchLendingInterestSeeds reads the lake's last version of each seed's key with q and refuses any seed it differs from.
func matchLendingInterestSeeds(ctx context.Context, conn driver.Conn, seeds []lendingInterestSeedRow, before uint32, q string,
	args ...any,
) error {
	lake, err := lendingInterestLakeVersions(ctx, conn, q, args...)
	if err != nil {
		return fmt.Errorf("clickhouse: lending interest seed proof before ledger %d (%d keys, limit %ds): %w",
			before, len(seeds), lendingInterestProofSeconds, err)
	}
	for _, r := range seeds {
		l, ok := lake[r.Key]
		if ok && l.Renumbered {
			return fmt.Errorf("%w: key %s: the lake holds one change at two intra_ledger_seq from ledger %d, a renumbering leftover; "+
				"run docs/operations/runbooks/entry-walk-renumbering.md's cleanup first", ErrLendingInterestSeedUnproved, r.Key, r.Ledger)
		}
		if !ok || l.Ledger != r.Ledger || l.Intra != r.Intra || l.Entry != r.Entry {
			return fmt.Errorf("%w: key %s: lending_state_hourly's last row before ledger %d is (%d, %d), the lake's last version "+
				"(%d, %d) (0: none), same image %t; finish the lending_state replay, then re-derive interest from its first ledger",
				ErrLendingInterestSeedUnproved, r.Key, before, r.Ledger, r.Intra, l.Ledger, l.Intra, ok && l.Entry == r.Entry)
		}
	}
	return nil
}

func lendingInterestLakeVersions(ctx context.Context, conn driver.Conn, q string, args ...any) (map[string]lendingInterestLakeVersion, error) {
	rows, err := conn.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := map[string]lendingInterestLakeVersion{}
	for rows.Next() {
		var k string
		var v lendingInterestLakeVersion
		var renumbered uint8
		if err := rows.Scan(&k, &v.Ledger, &v.Intra, &v.Entry, &renumbered); err != nil {
			return nil, err
		}
		v.Renumbered = renumbered != 0
		out[k] = v
	}
	return out, rows.Err()
}

// lendingInterestKeyLedgers runs a (key_xdr, ledger) query into a map.
func lendingInterestKeyLedgers(ctx context.Context, conn driver.Conn, q string, args ...any) (map[string]uint32, error) {
	rows, err := conn.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("clickhouse: lending interest seed proof: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := map[string]uint32{}
	for rows.Next() {
		var k string
		var l uint32
		if err := rows.Scan(&k, &l); err != nil {
			return nil, fmt.Errorf("clickhouse: lending interest seed proof: %w", err)
		}
		out[k] = l
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("clickhouse: lending interest seed proof: %w", err)
	}
	return out, nil
}

// LendingInterestUnderived is a pool deployed by a window's end of which lending_state_hourly holds no instance row.
// Lake reports whether the lake holds the instance's change at the deploy ledger.
type LendingInterestUnderived struct {
	Pool     string
	Deployed uint32
	Lake     bool
}

const lendingInterestInstanceRowsQuery = `SELECT contract_id, max(ledger_seq) FROM stellar.lending_state_hourly FINAL
	WHERE contract_id IN ? AND key_xdr IN ? AND hour <= ?
	GROUP BY contract_id`

const lendingInterestLakeInstanceQuery = `SELECT 1 FROM stellar.ledger_entry_changes
	WHERE ledger_seq = ? AND entry_type = 'contract_data' AND change_type != 'state' AND key_xdr = ? LIMIT 1`

// LendingInterestUnderivedPools lists the pools deployed by ledger hi whose instance lending_state_hourly holds no
// row of at or after the deploy ledger through hi's hour. Every pool writes its instance at deploy, so the state pass
// never derived such a pool: a pool holding an instance row but no reserve is empty, one holding nothing is unread.
func LendingInterestUnderivedPools(ctx context.Context, addr string, pools []LendingContract, hi uint32) ([]LendingInterestUnderived, error) {
	ps, err := LendingPrefixes(pools)
	if err != nil {
		return nil, err
	}
	inst := map[string]string{}
	var ids, keys []any
	for _, p := range ps {
		if p.Key == "Instance" {
			inst[p.Contract] = p.Prefix
			ids, keys = append(ids, p.Contract), append(keys, p.Prefix)
		}
	}
	if len(ids) == 0 {
		return nil, errors.New("clickhouse: lending interest underived pools: no pools")
	}
	conn, err := openRead(ctx, addr)
	if err != nil {
		return nil, err
	}
	defer func() { _ = conn.Close() }()
	hour, ok, err := ledgerHour(ctx, conn, hi)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("clickhouse: lending interest underived pools through %d: the ledger is not in stellar.ledgers", hi)
	}
	have, err := lendingInterestKeyLedgers(ctx, conn, lendingInterestInstanceRowsQuery, clickhouse.GroupSet{Value: ids},
		clickhouse.GroupSet{Value: keys}, hour)
	if err != nil {
		return nil, err
	}
	var out []LendingInterestUnderived
	for _, c := range pools {
		if c.Deployed > hi || (have[c.ID] != 0 && have[c.ID] >= c.Deployed) {
			continue
		}
		u := LendingInterestUnderived{Pool: c.ID, Deployed: c.Deployed}
		rows, err := conn.Query(ctx, lendingInterestLakeInstanceQuery, c.Deployed, inst[c.ID])
		if err != nil {
			return nil, fmt.Errorf("clickhouse: lending interest: pool %s instance at %d: %w", c.ID, c.Deployed, err)
		}
		u.Lake = rows.Next()
		err = rows.Err()
		_ = rows.Close()
		if err != nil {
			return nil, fmt.Errorf("clickhouse: lending interest: pool %s instance at %d: %w", c.ID, c.Deployed, err)
		}
		out = append(out, u)
	}
	return out, nil
}

// A lending_state replay rewrites state the interest rows from its first ledger on were derived from. It arms a redo
// (generation g, from ledger L); the interest pass then refuses everything but a re-derive from at or below L, or
// from the ledger after the redo's progress, until one reaches the watermark. Every row is max()-read, so each
// generation has its own from and progress rows and the latest armed and done generations are counters.
const (
	lendingInterestRedoName       = "lending_interest_redo"
	lendingInterestRedoDoneName   = "lending_interest_redo_done"
	lendingInterestRedoFromPrefix = "lending_interest_redo_from:"
	lendingInterestRedoThruPrefix = "lending_interest_redo_thru:"
)

// LendingInterestRedo is an armed redo: interest rows from From on must be re-derived; Thru is how far one has
// rewritten them contiguously (0: not begun).
type LendingInterestRedo struct {
	Gen, From, Thru uint32
}

// Next is the highest -from a re-derive may start at and keep the redo contiguous.
func (r LendingInterestRedo) Next() uint32 { return max(r.From, r.Thru+1) }

// LendingInterestPendingRedo returns the armed redo no re-derive has completed, if any.
func LendingInterestPendingRedo(ctx context.Context, addr string) (LendingInterestRedo, bool, error) {
	conn, err := openRead(ctx, addr)
	if err != nil {
		return LendingInterestRedo{}, false, err
	}
	defer func() { _ = conn.Close() }()
	return pendingLendingInterestRedo(ctx, conn)
}

func pendingLendingInterestRedo(ctx context.Context, conn driver.Conn) (LendingInterestRedo, bool, error) {
	rows, err := lendingInterestKeyLedgers(ctx, conn, `SELECT name, max(thru_ledger) FROM stellar.entry_history_watermark
		WHERE startsWith(name, 'lending_interest_redo') GROUP BY name`)
	if err != nil {
		return LendingInterestRedo{}, false, err
	}
	gen := rows[lendingInterestRedoName]
	if gen == 0 || gen <= rows[lendingInterestRedoDoneName] {
		return LendingInterestRedo{}, false, nil
	}
	g := strconv.FormatUint(uint64(gen), 10)
	from, ok := rows[lendingInterestRedoFromPrefix+g]
	if !ok {
		return LendingInterestRedo{}, false, fmt.Errorf("clickhouse: lending interest redo %d has no from row", gen)
	}
	return LendingInterestRedo{Gen: gen, From: from, Thru: rows[lendingInterestRedoThruPrefix+g]}, true, nil
}

// ArmLendingInterestRedo records, before a lending_state replay from ledger from writes, that interest rows from
// there on must be re-derived; a pending redo widens to the lower ledger and restarts, never below the interest floor.
// A no-op while no interest row reaches from.
func ArmLendingInterestRedo(ctx context.Context, addr string, from uint32) error {
	conn, err := openRead(ctx, addr)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	wm, err := entryWatermarkRow(ctx, conn, lendingInterestWatermarkName)
	if err != nil || wm < from {
		return err
	}
	last, err := entryWatermarkRow(ctx, conn, lendingInterestRedoName)
	if err != nil {
		return err
	}
	p, pending, err := pendingLendingInterestRedo(ctx, conn)
	if err != nil {
		return err
	}
	if pending {
		from = min(from, p.From)
	}
	floor, err := entryWatermarkRow(ctx, conn, lendingInterestFloorName)
	if err != nil {
		return err
	}
	// No interest row lies below the floor, and a re-derive may not start there.
	from = max(from, floor)
	gen := last + 1
	// The from row first, so the generation never stands without it.
	for _, row := range []struct {
		name string
		v    uint32
	}{{lendingInterestRedoFromPrefix + strconv.FormatUint(uint64(gen), 10), from}, {lendingInterestRedoName, gen}} {
		if err := conn.Exec(ctx, `INSERT INTO stellar.entry_history_watermark (name, thru_ledger) VALUES (?, ?)`, row.name, row.v); err != nil {
			return fmt.Errorf("clickhouse: arm lending interest redo from %d: %w", from, err)
		}
	}
	return nil
}

// AdvanceLendingInterestRedo records a re-derive of redo gen rewritten through thru, and the redo done once that
// reaches the interest watermark.
func AdvanceLendingInterestRedo(ctx context.Context, addr string, gen, thru uint32) error {
	conn, err := openRead(ctx, addr)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	g := strconv.FormatUint(uint64(gen), 10)
	if err := conn.Exec(ctx, `INSERT INTO stellar.entry_history_watermark (name, thru_ledger) VALUES (?, ?)`,
		lendingInterestRedoThruPrefix+g, thru); err != nil {
		return fmt.Errorf("clickhouse: lending interest redo %s through %d: %w", g, thru, err)
	}
	wm, err := entryWatermarkRow(ctx, conn, lendingInterestWatermarkName)
	if err != nil || thru < wm {
		return err
	}
	if err := conn.Exec(ctx, `INSERT INTO stellar.entry_history_watermark (name, thru_ledger) VALUES (?, ?)`,
		lendingInterestRedoDoneName, gen); err != nil {
		return fmt.Errorf("clickhouse: lending interest redo %s done: %w", g, err)
	}
	return nil
}
