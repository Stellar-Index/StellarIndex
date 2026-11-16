package clickhouse

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"fmt"

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
}

// LendingPrefix is one base64 key_xdr prefix the derive matches, and the contract it belongs to.
type LendingPrefix struct {
	Prefix   string
	Contract string
	Protocol string
	Key      string // storage key symbol, or "Instance"
}

// LendingPrefixes builds the key_xdr prefixes for a registry: per pool ResData, ResConfig and the instance (exact
// keys), per backstop PoolBalance, plus TierPoolBalance on the BLNT backstop.
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
	return out, nil
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
const lendingMatchedChanges = `FROM stellar.ledger_entry_changes
	WHERE ledger_seq BETWEEN ? AND ? AND entry_type = 'contract_data' AND change_type != 'state'
	  AND has(?, substring(key_xdr, 1, 52)) AND arrayExists(p -> startsWith(key_xdr, p), ?)`

// lendingStateInsert writes each matched key's last change per hour, server-side. Duplicate lake rows are
// byte-identical, so argMax needs no FINAL; a window that splits an hour leaves two rows the table's version collapses.
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
	ctx = clickhouse.Context(ctx, clickhouse.WithSettings(clickhouse.Settings{"max_execution_time": 3600}))
	// Counted before the read: rows only arrive, so the insert sees at least this coverage.
	var txLedgers uint64
	if err := conn.QueryRow(ctx, `SELECT uniqExact(ledger_seq) FROM stellar.ledger_entry_changes
		WHERE ledger_seq BETWEEN ? AND ? AND tx_hash != ''`, lo, hi).Scan(&txLedgers); err != nil {
		return 0, fmt.Errorf("clickhouse: lending window [%d,%d] entry-change ledgers: %w", lo, hi, err)
	}
	prefixes, contracts, protocols, heads := lendingPrefixArgs(ps)
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

// LendingProof is the completeness check's result: prefixed keys compared, and those whose latest history row is
// missing or differs from the lake's state at the watermark.
type LendingProof struct {
	Keys, Mismatched uint64
}

// lendingProofQuery compares, per prefixed key, the state at the watermark with its latest history row. A key the
// current table shows at or below the watermark is taken from there; one changed since is re-folded from the
// entry changes up to the watermark. Duplicate history rows are collapsed by FINAL, duplicate changes are identical.
const lendingProofQuery = `WITH cur AS (
		SELECT key_xdr, entry_xdr, ledger_seq FROM stellar.ledger_entries_current FINAL
		WHERE entry_type = 'contract_data' AND has(?, substring(key_xdr, 1, 52)) AND arrayExists(p -> startsWith(key_xdr, p), ?)
	)
	SELECT count(), countIf(h.present = 0 OR h.e != x.e)
	FROM (
		SELECT key_xdr AS k, entry_xdr AS e FROM cur WHERE ledger_seq <= ?
		UNION ALL
		SELECT key_xdr AS k, argMax(entry_xdr, (ledger_seq, intra_ledger_seq, tx_hash, op_index, change_index)) AS e
		FROM stellar.ledger_entry_changes
		WHERE ledger_seq <= ? AND entry_type = 'contract_data' AND change_type != 'state'
		  AND key_xdr IN (SELECT key_xdr FROM cur WHERE ledger_seq > ?)
		GROUP BY key_xdr
	) AS x
	LEFT JOIN (
		SELECT key_xdr AS k, argMax(entry_xdr, hour) AS e, 1 AS present
		FROM stellar.lending_state_hourly FINAL GROUP BY key_xdr
	) AS h ON x.k = h.k`

// ProveLendingState runs the completeness check at watermark wm.
func ProveLendingState(ctx context.Context, addr string, ps []LendingPrefix, wm uint32) (LendingProof, error) {
	conn, err := openRead(ctx, addr)
	if err != nil {
		return LendingProof{}, err
	}
	defer func() { _ = conn.Close() }()
	prefixes, _, _, heads := lendingPrefixArgs(ps)
	var p LendingProof
	if err := conn.QueryRow(ctx, lendingProofQuery, heads, prefixes, wm, wm, wm).Scan(&p.Keys, &p.Mismatched); err != nil {
		return LendingProof{}, fmt.Errorf("clickhouse: lending proof at %d: %w", wm, err)
	}
	return p, nil
}
