package clickhouse

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"strings"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/stellar/go-stellar-sdk/xdr"
)

// Lending protocols as stellar.lending_state_hourly records them; the protocol fixes how a reader scales the rates.
const (
	LendingBlendV1 = "blend_v1"
	LendingBlendV2 = "blend_v2"
	LendingBLNT    = "blnt"
)

// LendingContract is one pool or backstop whose storage the lending derive captures.
type LendingContract struct {
	ID       string // C-strkey
	Protocol string
	Backstop bool
	Deployed uint32 // the deploy event's ledger, before which the contract has no storage; 0 = unknown
}

// LendingPrefix is one base64 key_xdr prefix the derive matches, and the contract it belongs to.
type LendingPrefix struct {
	Prefix   string
	Contract string
	Protocol string
	Key      string // storage key symbol, or "Instance"
}

// LendingPrefixes builds the key_xdr prefixes for a registry: per pool ResData, ResConfig and the instance (exact
// keys), plus ProtocolFees (exact) on BLNT pools; per backstop PoolBalance, plus TierPoolBalance on the BLNT backstop.
func LendingPrefixes(reg []LendingContract) ([]LendingPrefix, error) {
	var out []LendingPrefix
	for _, c := range reg {
		ps, err := lendingContractPrefixes(c)
		if err != nil {
			return nil, err
		}
		out = append(out, ps...)
	}
	return out, nil
}

// lendingKey is one captured storage key variant: its symbol and the type of its single argument.
type lendingKey struct {
	sym string
	arg xdr.ScValType
}

func lendingKeys(c LendingContract) []lendingKey {
	switch {
	case c.Backstop && c.Protocol == LendingBLNT:
		return []lendingKey{{"PoolBalance", xdr.ScValTypeScvAddress}, {"TierPoolBalance", xdr.ScValTypeScvMap}}
	case c.Backstop:
		return []lendingKey{{"PoolBalance", xdr.ScValTypeScvAddress}}
	}
	return []lendingKey{{"ResData", xdr.ScValTypeScvAddress}, {"ResConfig", xdr.ScValTypeScvAddress}}
}

func lendingContractPrefixes(c LendingContract) ([]LendingPrefix, error) {
	id, err := contractIDFromStrkey(c.ID)
	if err != nil {
		return nil, fmt.Errorf("clickhouse: lending contract %q: %w", c.ID, err)
	}
	var out []LendingPrefix
	for _, k := range lendingKeys(c) {
		p, err := lendingKeyPrefix(id, k.sym, k.arg)
		if err != nil {
			return nil, err
		}
		out = append(out, LendingPrefix{Prefix: p, Contract: c.ID, Protocol: c.Protocol, Key: k.sym})
	}
	if c.Backstop {
		return out, nil
	}
	inst, err := instanceKeyXDR(xdr.Hash(id))
	if err != nil {
		return nil, err
	}
	for _, k := range inst {
		out = append(out, LendingPrefix{Prefix: k, Contract: c.ID, Protocol: c.Protocol, Key: "Instance"})
	}
	if c.Protocol == LendingBLNT {
		// BLNT keeps protocol-fee credit outside ReserveData, in the unit-variant key PoolDataKey::ProtocolFees.
		fees, err := lendingUnitKey(id, "ProtocolFees")
		if err != nil {
			return nil, err
		}
		out = append(out, LendingPrefix{Prefix: fees, Contract: c.ID, Protocol: c.Protocol, Key: "ProtocolFees"})
	}
	return out, nil
}

// lendingUnitKey is the exact base64 key_xdr of a contract's persistent unit-variant key Vec[Symbol(sym)].
func lendingUnitKey(contract xdr.ContractId, sym string) (string, error) {
	s := xdr.ScSymbol(sym)
	vec := &xdr.ScVec{{Type: xdr.ScValTypeScvSymbol, Sym: &s}}
	k, err := xdr.MarshalBase64(xdr.LedgerKey{
		Type: xdr.LedgerEntryTypeContractData,
		ContractData: &xdr.LedgerKeyContractData{
			Contract:   xdr.ScAddress{Type: xdr.ScAddressTypeScAddressTypeContract, ContractId: &contract},
			Key:        xdr.ScVal{Type: xdr.ScValTypeScvVec, Vec: &vec},
			Durability: xdr.ContractDataDurabilityPersistent,
		},
	})
	if err != nil {
		return "", fmt.Errorf("clickhouse: marshal %s key: %w", sym, err)
	}
	return k, nil
}

// lendingKeyPrefix is the base64 prefix shared by every persistent key Vec[Symbol(sym), <arg of type arg>] of a
// contract: the fixed bytes through the argument's type tag, cut to a 3-byte boundary so the base64 is exact.
func lendingKeyPrefix(contract xdr.ContractId, sym string, arg xdr.ScValType) (string, error) {
	s := xdr.ScSymbol(sym)
	vec := &xdr.ScVec{{Type: xdr.ScValTypeScvSymbol, Sym: &s}, {Type: xdr.ScValTypeScvVoid}}
	lk := xdr.LedgerKey{
		Type: xdr.LedgerEntryTypeContractData,
		ContractData: &xdr.LedgerKeyContractData{
			Contract:   xdr.ScAddress{Type: xdr.ScAddressTypeScAddressTypeContract, ContractId: &contract},
			Key:        xdr.ScVal{Type: xdr.ScValTypeScvVec, Vec: &vec},
			Durability: xdr.ContractDataDurabilityPersistent,
		},
	}
	raw, err := lk.MarshalBinary()
	if err != nil {
		return "", fmt.Errorf("clickhouse: marshal %s key: %w", sym, err)
	}
	// raw ends with the void tag and the durability, 4 bytes each; the real argument starts with its own tag.
	fixed := binary.BigEndian.AppendUint32(raw[:len(raw)-8:len(raw)-8], uint32(arg)) //nolint:gosec // ScValType is a small enum
	return base64.StdEncoding.EncodeToString(fixed[:len(fixed)/3*3]), nil
}

// lendingContractHeadLen is the base64 length of a key's first 39 bytes (type tags and 31 of the 32 contract-id
// bytes): a hash-set prefilter in front of the per-prefix match.
const lendingContractHeadLen = 52

// lendingPrefixArgs is the derive's parameter arrays: prefixes with their contract and protocol, and the contract heads.
func lendingPrefixArgs(ps []LendingPrefix) (prefixes, contracts, protocols, heads []string) {
	seen := map[string]bool{}
	for _, p := range ps {
		prefixes = append(prefixes, p.Prefix)
		contracts = append(contracts, p.Contract)
		protocols = append(protocols, p.Protocol)
		if h := p.Prefix[:lendingContractHeadLen]; !seen[h] {
			seen[h] = true
			heads = append(heads, h)
		}
	}
	return prefixes, contracts, protocols, heads
}

// lendingMatchedChanges selects the window's post-change images ('state' rows are pre-images) of prefixed keys.
// FINAL: a re-derived range keeps its superseded rows until a merge, and those carry another walk's intra_ledger_seq.
const lendingMatchedChanges = `FROM stellar.ledger_entry_changes FINAL
	WHERE ledger_seq BETWEEN ? AND ? AND entry_type = 'contract_data' AND change_type != 'state'
	  AND has(?, substring(key_xdr, 1, 52)) AND arrayExists(p -> startsWith(key_xdr, p), ?)`

// lendingStateInsert writes each matched key's last change per hour, server-side. A window that splits an hour
// leaves two rows the table's version collapses.
const lendingStateInsert = `INSERT INTO stellar.lending_state_hourly
	(contract_id, protocol, key_xdr, hour, ledger_seq, intra_ledger_seq, close_time, entry_xdr)
	SELECT arrayElement(?, i), arrayElement(?, i), key_xdr, hour, last.1, last.2, last.3, last.4
	FROM (
		SELECT key_xdr, arrayFirstIndex(p -> startsWith(key_xdr, p), ?) AS i, toStartOfHour(close_time) AS hour,
		       argMax((ledger_seq, intra_ledger_seq, close_time, entry_xdr),
		              (ledger_seq, intra_ledger_seq, tx_hash, op_index, change_index)) AS last
		` + lendingMatchedChanges + `
		GROUP BY key_xdr, i, hour
	)`

// lendingAmbiguousOrderQuery counts the (key, hour) pairs whose last-changed ledger has rows without intra-ledger
// order (intra_ledger_seq 0) from more than one transaction: there tx_hash, not apply order, would pick the hour's row.
const lendingAmbiguousOrderQuery = `SELECT count(), groupArray(5)(concat(toString(last.1), ' ', substring(key_xdr, 1, 100)))
	FROM (
		SELECT key_xdr, argMax((ledger_seq, ambiguous), ledger_seq) AS last
		FROM (
			SELECT key_xdr, toStartOfHour(close_time) AS hour, ledger_seq,
			       countIf(intra_ledger_seq = 0) > 0 AND uniqExact(tx_hash) > 1 AS ambiguous
			` + lendingMatchedChanges + `
			GROUP BY key_xdr, hour, ledger_seq
		)
		GROUP BY key_xdr, hour
	)
	WHERE last.2 = 1`

// DeriveLendingWindow writes stellar.lending_state_hourly for [lo, hi] in one INSERT … SELECT and returns the
// tx-bearing ledgers that held entry changes when it started, the coverage SetLendingStateWatermark proves.
func DeriveLendingWindow(ctx context.Context, addr string, lo, hi uint32, ps []LendingPrefix) (uint64, error) {
	if len(ps) == 0 {
		return 0, fmt.Errorf("clickhouse: lending window [%d,%d]: no key prefixes", lo, hi)
	}
	conn, err := openAccountMovementsWrite(ctx, addr)
	if err != nil {
		return 0, err
	}
	defer func() { _ = conn.Close() }()
	// A row's identity starts with ledger_seq, the partition key, so FINAL never has to merge across partitions.
	ctx = clickhouse.Context(ctx, clickhouse.WithSettings(clickhouse.Settings{
		"max_execution_time": 3600, "do_not_merge_across_partitions_select_final": 1,
	}))
	// Counted before the read: rows only arrive, so the insert sees at least this coverage.
	txLedgers, err := lakeTxLedgers(ctx, conn, "lending", lo, hi)
	if err != nil {
		return 0, err
	}
	prefixes, contracts, protocols, heads := lendingPrefixArgs(ps)
	var ambiguous uint64
	var samples []string
	if err := conn.QueryRow(ctx, lendingAmbiguousOrderQuery, lo, hi, heads, prefixes).Scan(&ambiguous, &samples); err != nil {
		return 0, fmt.Errorf("clickhouse: lending window [%d,%d] same-ledger order check: %w", lo, hi, err)
	}
	if ambiguous > 0 {
		return 0, fmt.Errorf("clickhouse: lending window [%d,%d]: %d keys have several transactions without intra-ledger order "+
			"in their hour's last ledger (ledger key_xdr-prefix: %q); re-derive the lake range's intra_ledger_seq first", lo, hi, ambiguous, samples)
	}
	if err := conn.Exec(ctx, lendingStateInsert, contracts, protocols, prefixes, lo, hi, heads, prefixes); err != nil {
		return 0, fmt.Errorf("clickhouse: derive lending state [%d,%d]: %w", lo, hi, err)
	}
	return txLedgers, nil
}

const lendingStateWatermarkName = "lending_state"

// LendingStateWatermark returns the ledger lending_state_hourly is complete through (0 = never run).
func LendingStateWatermark(ctx context.Context, addr string) (uint32, error) {
	conn, err := openRead(ctx, addr)
	if err != nil {
		return 0, err
	}
	defer func() { _ = conn.Close() }()
	return entryWatermarkOn(ctx, conn, lendingStateWatermarkName)
}

// SetLendingStateWatermark records the derive complete through thru once the window is contiguous in the lake and
// the entry changes it read cover every tx-bearing ledger; a window at or below the watermark is a no-op.
func SetLendingStateWatermark(ctx context.Context, addr string, from, thru uint32, readTxLedgers uint64) error {
	return advanceEntryWatermark(ctx, addr, lendingStateWatermarkName, from, thru, func(ctx context.Context, conn driver.Conn) error {
		return entryHistoryWindowCovered(ctx, conn, lendingStateWatermarkName, from, thru, readTxLedgers)
	})
}

// LendingProofKey is one prefixed key of ledger_entries_current set against its latest history row at or below the
// watermark (under FINAL; zero fields when the table has none).
type LendingProofKey struct {
	Key     string
	Changed bool   // the current table's change is past the watermark, so its entry is not the state there
	Present bool   // a history row at or below the watermark exists
	Ledger  uint32 // that row's ledger
	Entry   string // that row's entry; returned for a Changed key only
	// For a key not Changed: the current entry equals the row's, and the current entry's hour holds a row past the
	// watermark.
	Same, AheadHit bool
	Ahead          []time.Time // hours holding a history row past the watermark; returned for a Changed key only
}

// lendingProofKeysHead/Tail wrap an OR of startsWith(key_xdr, ?), one per prefix: constant prefixes the
// (entry_type, key_xdr) primary key turns into ranges, where an array lookup would read every contract_data row.
const lendingProofKeysHead = `SELECT x.k, x.seq > ?, h.present, h.l, if(x.seq > ?, h.e, ''), x.e = h.e, has(h.ahead, x.hr),
		if(x.seq > ?, h.ahead, [])
	FROM (
		SELECT key_xdr AS k, entry_xdr AS e, ledger_seq AS seq, toStartOfHour(close_time) AS hr
		FROM stellar.ledger_entries_current FINAL
		WHERE entry_type = 'contract_data' AND (`

const lendingProofKeysTail = `)
	) AS x
	LEFT JOIN (
		SELECT key_xdr AS k, argMaxIf(entry_xdr, hour, ledger_seq <= ?) AS e, argMaxIf(ledger_seq, hour, ledger_seq <= ?) AS l,
		       countIf(ledger_seq <= ?) > 0 AS present, groupArrayIf(hour, ledger_seq > ?) AS ahead
		FROM stellar.lending_state_hourly FINAL GROUP BY key_xdr
	) AS h ON x.k = h.k`

// lendingProofKeysQuery reads the current table once, through the primary key, for the keys under ps.
func lendingProofKeysQuery(ps []LendingPrefix) string {
	return lendingProofKeysHead + strings.Repeat("startsWith(key_xdr, ?) OR ", len(ps)-1) + "startsWith(key_xdr, ?)" + lendingProofKeysTail
}

// LendingProofKeys lists every key under ps in ledger_entries_current with its latest history row at or below wm.
func LendingProofKeys(ctx context.Context, addr string, ps []LendingPrefix, wm uint32) ([]LendingProofKey, error) {
	if len(ps) == 0 {
		return nil, fmt.Errorf("clickhouse: lending proof at %d: no key prefixes", wm)
	}
	conn, err := openRead(ctx, addr)
	if err != nil {
		return nil, err
	}
	defer func() { _ = conn.Close() }()
	args := []any{wm, wm, wm}
	for _, p := range ps {
		args = append(args, p.Prefix)
	}
	args = append(args, wm, wm, wm, wm)
	rows, err := conn.Query(ctx, lendingProofKeysQuery(ps), args...)
	if err != nil {
		return nil, fmt.Errorf("clickhouse: lending proof keys at %d: %w", wm, err)
	}
	defer func() { _ = rows.Close() }()
	var out []LendingProofKey
	for rows.Next() {
		var k LendingProofKey
		var changed, present, same, ahead uint8
		if err := rows.Scan(&k.Key, &changed, &present, &k.Ledger, &k.Entry, &same, &ahead, &k.Ahead); err != nil {
			return nil, fmt.Errorf("clickhouse: lending proof keys at %d: %w", wm, err)
		}
		k.Changed, k.Present, k.Same, k.AheadHit = changed == 1, present == 1, same == 1, ahead == 1
		out = append(out, k)
	}
	return out, rows.Err()
}

// LendingChange is a key's last post-change image in a ledger range.
type LendingChange struct {
	Key    string
	Ledger uint32
	Entry  string
	Hour   time.Time
}

// lendingLastChangesQuery folds each listed key's changes in one window to its last, in the derive's order and
// under its FINAL (see lendingMatchedChanges).
const lendingLastChangesQuery = `SELECT key_xdr, last.1, last.2, toStartOfHour(last.3) FROM (
		SELECT key_xdr, argMax((ledger_seq, entry_xdr, close_time), (ledger_seq, intra_ledger_seq, tx_hash, op_index, change_index)) AS last
		FROM stellar.ledger_entry_changes FINAL
		WHERE ledger_seq BETWEEN ? AND ? AND entry_type = 'contract_data' AND change_type != 'state' AND has(?, key_xdr)
		GROUP BY key_xdr
	)`

// LendingLastChanges returns each of keys' last change in [lo, hi]; a key with none there is absent. Keep [lo, hi]
// inside one lake partition: FINAL is then a merge of that range only.
func LendingLastChanges(ctx context.Context, addr string, keys []string, lo, hi uint32) ([]LendingChange, error) {
	conn, err := openRead(ctx, addr)
	if err != nil {
		return nil, err
	}
	defer func() { _ = conn.Close() }()
	ctx = clickhouse.Context(ctx, clickhouse.WithSettings(clickhouse.Settings{
		"max_execution_time": 3600, "do_not_merge_across_partitions_select_final": 1,
	}))
	rows, err := conn.Query(ctx, lendingLastChangesQuery, lo, hi, keys)
	if err != nil {
		return nil, fmt.Errorf("clickhouse: lending last changes [%d,%d]: %w", lo, hi, err)
	}
	defer func() { _ = rows.Close() }()
	var out []LendingChange
	for rows.Next() {
		var c LendingChange
		if err := rows.Scan(&c.Key, &c.Ledger, &c.Entry, &c.Hour); err != nil {
			return nil, fmt.Errorf("clickhouse: lending last changes [%d,%d]: %w", lo, hi, err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// lendingHourSpan bounds the ledgers one hour can span (closes are at least 5 s apart, so at most 720): the count
// proof reads this far past a window so an hour split across its end is attributed to the window holding its last change.
const lendingHourSpan = 2000

// LendingCountMismatch is one contract whose (key, hour) pairs in the lake differ from its history rows in a window.
type LendingCountMismatch struct {
	Contract    string
	Lake, Table uint64
}

// lendingProofCountQuery compares, per contract, the matched (key, hour) pairs whose last change falls in [lo, hi]
// with the history rows (under FINAL) whose ledger falls there. A pair in the watermark's hour whose row is past the
// watermark is left out of both sides: FINAL has replaced its row at the watermark, so the table cannot show it.
const lendingProofCountQuery = `SELECT c, sum(lake), sum(tbl) FROM (
		SELECT arrayElement(?, i) AS c, count() AS lake, toUInt64(0) AS tbl FROM (
			SELECT key_xdr, arrayFirstIndex(p -> startsWith(key_xdr, p), ?) AS i, toStartOfHour(close_time) AS hour,
			       max(ledger_seq) AS last
			` + lendingMatchedChanges + `
			GROUP BY key_xdr, i, hour
		) WHERE last BETWEEN ? AND ?
		  AND (key_xdr, hour) NOT IN (SELECT key_xdr, hour FROM stellar.lending_state_hourly FINAL WHERE ledger_seq > ?
		      AND hour = (SELECT toStartOfHour(max(close_time)) FROM stellar.ledgers WHERE ledger_seq = ?))
		GROUP BY c
		UNION ALL
		SELECT contract_id AS c, toUInt64(0) AS lake, count() AS tbl
		FROM stellar.lending_state_hourly FINAL WHERE ledger_seq BETWEEN ? AND ? GROUP BY c
	) GROUP BY c HAVING sum(lake) != sum(tbl) ORDER BY c`

// ProveLendingCounts checks that window [lo, hi] (at most the watermark wm) holds one history row per matched
// (key, hour) pair, per contract, and returns the contracts that differ.
func ProveLendingCounts(ctx context.Context, addr string, ps []LendingPrefix, lo, hi, wm uint32) ([]LendingCountMismatch, error) {
	conn, err := openRead(ctx, addr)
	if err != nil {
		return nil, err
	}
	defer func() { _ = conn.Close() }()
	prefixes, contracts, _, heads := lendingPrefixArgs(ps)
	readHi := min(uint64(hi)+lendingHourSpan, uint64(wm))
	rows, err := conn.Query(ctx, lendingProofCountQuery, contracts, prefixes, lo, readHi, heads, prefixes, lo, hi, wm, wm, lo, hi)
	if err != nil {
		return nil, fmt.Errorf("clickhouse: lending count proof [%d,%d]: %w", lo, hi, err)
	}
	defer func() { _ = rows.Close() }()
	var out []LendingCountMismatch
	for rows.Next() {
		var m LendingCountMismatch
		if err := rows.Scan(&m.Contract, &m.Lake, &m.Table); err != nil {
			return nil, fmt.Errorf("clickhouse: lending count proof [%d,%d]: %w", lo, hi, err)
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// LendingKeyCount is how many distinct keys of one kind a contract has in the history table and, live, in
// ledger_entries_current.
type LendingKeyCount struct {
	Contract, Key  string
	Table, Current uint64
}

// lendingKeyCountQuery counts distinct keys per (contract, key kind) in the history table and in the current table
// (live entries only).
const lendingKeyCountQuery = `SELECT arrayElement(?, i) AS c, arrayElement(?, i) AS kind,
		uniqExactIf(key_xdr, src = 'table'), uniqExactIf(key_xdr, src = 'current')
	FROM (
		SELECT key_xdr, arrayFirstIndex(p -> startsWith(key_xdr, p), ?) AS i, 'table' AS src
		FROM stellar.lending_state_hourly FINAL
		UNION ALL
		SELECT key_xdr, arrayFirstIndex(p -> startsWith(key_xdr, p), ?) AS i, 'current' AS src
		FROM stellar.ledger_entries_current FINAL
		WHERE entry_type = 'contract_data' AND entry_xdr != '' AND has(?, substring(key_xdr, 1, 52))
	)
	WHERE i > 0 GROUP BY c, kind ORDER BY c, kind`

// LendingKeyCounts lists, per contract and key kind, the distinct keys in the history and current tables.
func LendingKeyCounts(ctx context.Context, addr string, ps []LendingPrefix) ([]LendingKeyCount, error) {
	conn, err := openRead(ctx, addr)
	if err != nil {
		return nil, err
	}
	defer func() { _ = conn.Close() }()
	prefixes, contracts, _, heads := lendingPrefixArgs(ps)
	kinds := make([]string, len(ps))
	for i, p := range ps {
		kinds[i] = p.Key
	}
	rows, err := conn.Query(ctx, lendingKeyCountQuery, contracts, kinds, prefixes, prefixes, heads)
	if err != nil {
		return nil, fmt.Errorf("clickhouse: lending key counts: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []LendingKeyCount
	for rows.Next() {
		var k LendingKeyCount
		if err := rows.Scan(&k.Contract, &k.Key, &k.Table, &k.Current); err != nil {
			return nil, fmt.Errorf("clickhouse: lending key counts: %w", err)
		}
		out = append(out, k)
	}
	return out, rows.Err()
}
