package clickhouse

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math/big"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/stellar/go-stellar-sdk/strkey"
	"github.com/stellar/go-stellar-sdk/xdr"
)

// NetworkStateReader reads stellar.ledger_entries_current for comparison with
// the network's own state (verify-network-state).
type NetworkStateReader struct{ conn driver.Conn }

// NewNetworkStateReader dials ClickHouse on the heavy read class.
func NewNetworkStateReader(ctx context.Context, addr string) (*NetworkStateReader, error) {
	conn, err := openRead(ctx, addr)
	if err != nil {
		return nil, err
	}
	return &NetworkStateReader{conn: conn}, nil
}

// Close releases the connection.
func (r *NetworkStateReader) Close() error { return r.conn.Close() }

// LedgerKeyColumns returns the (entry_type, key_xdr) pair the writer stores
// for key, so a caller looks a network key up in exactly the lake's encoding.
func LedgerKeyColumns(key xdr.LedgerKey) (entryType, keyXDR string, err error) {
	keyXDR, err = xdr.MarshalBase64(key)
	if err != nil {
		return "", "", fmt.Errorf("clickhouse: marshal ledger key: %w", err)
	}
	return entryTypeName(key.Type), keyXDR, nil
}

// CurrentEntry is one key's resolved row in stellar.ledger_entries_current.
type CurrentEntry struct {
	ChangeType string
	LedgerSeq  uint32
	EntryXDR   string
}

// CurrentEntries returns the FINAL ledger_entries_current row for each key of
// one entry type. A key the table does not hold is absent from the map.
func (r *NetworkStateReader) CurrentEntries(ctx context.Context, entryType string, keys []string) (map[string]CurrentEntry, error) {
	out := make(map[string]CurrentEntry, len(keys))
	if len(keys) == 0 {
		return out, nil
	}
	const q = `SELECT key_xdr, change_type, ledger_seq, entry_xdr
		FROM stellar.ledger_entries_current FINAL
		WHERE entry_type = ? AND key_xdr IN (?)`
	rows, err := r.conn.Query(ctx, q, entryType, keys)
	if err != nil {
		return nil, fmt.Errorf("clickhouse: current entries (%s): %w", entryType, err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var key string
		var e CurrentEntry
		if err := rows.Scan(&key, &e.ChangeType, &e.LedgerSeq, &e.EntryXDR); err != nil {
			return nil, fmt.Errorf("clickhouse: scan current entry: %w", err)
		}
		out[key] = e
	}
	return out, rows.Err()
}

// LumenTally is native XLM summed over every domain that can hold it, as of
// one ledger, beside that ledger header's total_coins and fee_pool. All sums
// are exact integers in stroops.
type LumenTally struct {
	Ledger            uint32
	TotalCoins        int64
	FeePool           int64
	Accounts          *big.Int
	ClaimableBalances *big.Int
	LiquidityPools    *big.Int
	ContractBalances  *big.Int
	// Preimages counts keys whose current row is newer than Ledger and were
	// resolved from that row's first later change instead. A newer
	// contract_data key that cannot hold native XLM is not resolved: it adds
	// zero at every ledger.
	Preimages int
}

func newLumenTally(ledger uint32, totalCoins, feePool int64) *LumenTally {
	return &LumenTally{
		Ledger: ledger, TotalCoins: totalCoins, FeePool: feePool,
		Accounts: new(big.Int), ClaimableBalances: new(big.Int),
		LiquidityPools: new(big.Int), ContractBalances: new(big.Int),
	}
}

// Held is the native XLM held across all four domains.
func (t *LumenTally) Held() *big.Int {
	s := new(big.Int).Add(t.Accounts, t.ClaimableBalances)
	s.Add(s, t.LiquidityPools)
	return s.Add(s, t.ContractBalances)
}

// Residual is Held + FeePool − TotalCoins: zero when the lake's state conserves
// lumens, negative when it is missing holdings.
func (t *LumenTally) Residual() *big.Int {
	r := new(big.Int).Add(t.Held(), big.NewInt(t.FeePool))
	return r.Sub(r, big.NewInt(t.TotalCoins))
}

// lumenEntryTypes are the entry types that can hold native XLM.
var lumenEntryTypes = []string{"account", "claimable_balance", "liquidity_pool", "contract_data"}

// LumenConservation tallies native XLM held in ledger_entries_current as of the
// newest committed ledger (stellar.ledgers is written last in a flush). One
// FINAL read gives a single parts snapshot; keys whose resolved row is newer
// than that ledger are rolled back to their first later change's pre-image in
// ledger_entry_changes, so concurrent ingest cannot tear the sum.
// nativeSAC is the network's native Stellar Asset Contract id (C…).
func (r *NetworkStateReader) LumenConservation(ctx context.Context, nativeSAC string) (*LumenTally, error) {
	sac, prefix, err := nativeSACKey(nativeSAC)
	if err != nil {
		return nil, err
	}
	var (
		ledger              uint32
		totalCoins, feePool int64
	)
	if err := r.conn.QueryRow(ctx, `SELECT ledger_seq, total_coins, fee_pool FROM stellar.ledgers FINAL
		WHERE ledger_seq = (SELECT max(ledger_seq) FROM stellar.ledgers)`).
		Scan(&ledger, &totalCoins, &feePool); err != nil {
		return nil, fmt.Errorf("clickhouse: lumen conservation: tip header: %w", err)
	}
	t := newLumenTally(ledger, totalCoins, feePool)
	pending, err := r.foldCurrentLumens(ctx, t, sac, prefix)
	if err != nil {
		return nil, err
	}
	if err := r.foldPreimageLumens(ctx, t, sac, pending); err != nil {
		return nil, err
	}
	return t, nil
}

// foldCurrentLumens folds every resolved row at or below t.Ledger and returns
// the keys whose row is newer and that can hold native XLM.
func (r *NetworkStateReader) foldCurrentLumens(ctx context.Context, t *LumenTally, sac xdr.ContractId, prefix string) ([]string, error) {
	const q = `SELECT entry_type, if(ledger_seq > ?, key_xdr, '') AS key, change_type, ledger_seq, balance,
			if(entry_type = 'account', '', entry_xdr) AS entry
		FROM stellar.ledger_entries_current FINAL
		WHERE entry_type IN ('account', 'claimable_balance', 'liquidity_pool')
		   OR (entry_type = 'contract_data' AND startsWith(key_xdr, ?))`
	rows, err := r.conn.Query(ctx, q, t.Ledger, prefix)
	if err != nil {
		return nil, fmt.Errorf("clickhouse: lumen conservation: current state: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var pending []string
	for rows.Next() {
		var (
			entryType, key, changeType, entryXDR string
			seq                                  uint32
			balance                              int64
		)
		if err := rows.Scan(&entryType, &key, &changeType, &seq, &balance, &entryXDR); err != nil {
			return nil, fmt.Errorf("clickhouse: lumen conservation: scan: %w", err)
		}
		if seq > t.Ledger {
			holds, err := keyHoldsNativeLumens(entryType, key, sac)
			if err != nil {
				return nil, err
			}
			if holds {
				pending = append(pending, key)
			}
			continue
		}
		if changeType == "removed" {
			continue
		}
		if err := foldNativeHolding(t, entryType, balance, entryXDR, sac); err != nil {
			return nil, err
		}
	}
	return pending, rows.Err()
}

// keyHoldsNativeLumens reports whether an entry under this key can hold native
// XLM. Only contract_data is decidable from the key: an evicted temporary
// entry (an allowance) has a `removed` row and no pre-image, yet holds none.
func keyHoldsNativeLumens(entryType, keyXDR string, sac xdr.ContractId) (bool, error) {
	if entryType != "contract_data" {
		return true, nil
	}
	var k xdr.LedgerKey
	if err := xdr.SafeUnmarshalBase64(keyXDR, &k); err != nil {
		return false, fmt.Errorf("clickhouse: lumen conservation: decode contract_data key: %w", err)
	}
	cd, ok := k.GetContractData()
	if !ok {
		return false, fmt.Errorf("clickhouse: lumen conservation: contract_data row keyed as %s", k.Type)
	}
	return isNativeSACBalance(cd.Contract, cd.Key, sac), nil
}

// foldPreimageLumens resolves each pending key to its state as of t.Ledger
// from the first change after it: a `state` or `restored` row carries that
// state, a `created` row means the key did not exist yet. Snapshot seed rows
// are skipped: they hold an entry's post-state, never a pre-image.
func (r *NetworkStateReader) foldPreimageLumens(ctx context.Context, t *LumenTally, sac xdr.ContractId, pending []string) error {
	if len(pending) == 0 {
		return nil
	}
	const q = `SELECT key_xdr, entry_type, change_type, balance, entry_xdr, tx_hash, op_index
		FROM stellar.ledger_entry_changes
		WHERE ledger_seq > ? AND entry_type IN (?) AND key_xdr IN (?)
		ORDER BY ledger_seq, intra_ledger_seq`
	rows, err := r.conn.Query(ctx, q, t.Ledger, lumenEntryTypes, pending)
	if err != nil {
		return fmt.Errorf("clickhouse: lumen conservation: pre-images: %w", err)
	}
	defer func() { _ = rows.Close() }()
	open := make(map[string]bool, len(pending))
	for _, k := range pending {
		open[k] = true
	}
	for rows.Next() {
		var key, entryType, changeType, entryXDR, txHash string
		var balance int64
		var opIndex int32
		if err := rows.Scan(&key, &entryType, &changeType, &balance, &entryXDR, &txHash, &opIndex); err != nil {
			return fmt.Errorf("clickhouse: lumen conservation: scan pre-image: %w", err)
		}
		if !open[key] || isSnapshotSeedRow(txHash, opIndex, changeType) {
			continue
		}
		delete(open, key)
		switch changeType {
		case "created":
		case "state", "restored":
			if err := foldNativeHolding(t, entryType, balance, entryXDR, sac); err != nil {
				return err
			}
		default:
			return fmt.Errorf("clickhouse: lumen conservation: %s key %s first changes after ledger %d as %q with no pre-image", entryType, key, t.Ledger, changeType)
		}
		t.Preimages++
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(open) > 0 {
		return fmt.Errorf("clickhouse: lumen conservation: %d key(s) newer than ledger %d have no later change in ledger_entry_changes", len(open), t.Ledger)
	}
	return nil
}

// foldNativeHolding adds one live entry's native XLM to its domain's sum.
// Account rows carry it in the balance column; the rest are decoded.
func foldNativeHolding(t *LumenTally, entryType string, balance int64, entryXDR string, sac xdr.ContractId) error {
	if entryType == "account" {
		t.Accounts.Add(t.Accounts, big.NewInt(balance))
		return nil
	}
	var e xdr.LedgerEntry
	if err := xdr.SafeUnmarshalBase64(entryXDR, &e); err != nil {
		return fmt.Errorf("clickhouse: lumen conservation: decode %s entry: %w", entryType, err)
	}
	switch e.Data.Type {
	case xdr.LedgerEntryTypeClaimableBalance:
		cb := e.Data.MustClaimableBalance()
		if cb.Asset.Type == xdr.AssetTypeAssetTypeNative {
			t.ClaimableBalances.Add(t.ClaimableBalances, big.NewInt(int64(cb.Amount)))
		}
	case xdr.LedgerEntryTypeLiquidityPool:
		cp, ok := e.Data.MustLiquidityPool().Body.GetConstantProduct()
		if !ok {
			return errors.New("clickhouse: lumen conservation: liquidity pool with no constant-product body")
		}
		if cp.Params.AssetA.Type == xdr.AssetTypeAssetTypeNative {
			t.LiquidityPools.Add(t.LiquidityPools, big.NewInt(int64(cp.ReserveA)))
		}
		if cp.Params.AssetB.Type == xdr.AssetTypeAssetTypeNative {
			t.LiquidityPools.Add(t.LiquidityPools, big.NewInt(int64(cp.ReserveB)))
		}
	case xdr.LedgerEntryTypeContractData:
		return foldSACBalance(t, e.Data.MustContractData(), sac)
	}
	return nil
}

// foldSACBalance adds a native-SAC Balance(Address) entry's amount. The key
// prefix spans 31 of the contract id's 32 bytes, so the id is re-checked here.
func foldSACBalance(t *LumenTally, cd xdr.ContractDataEntry, sac xdr.ContractId) error {
	if !isNativeSACBalance(cd.Contract, cd.Key, sac) {
		return nil
	}
	amount, ok := balanceAmount(cd.Val)
	if !ok {
		return errors.New("clickhouse: lumen conservation: native SAC balance entry with no readable amount")
	}
	t.ContractBalances.Add(t.ContractBalances, amount)
	return nil
}

func isNativeSACBalance(contract xdr.ScAddress, key xdr.ScVal, sac xdr.ContractId) bool {
	return contract.Type == xdr.ScAddressTypeScAddressTypeContract && contract.ContractId != nil &&
		bytes.Equal(contract.ContractId[:], sac[:]) && balanceKeyHolder(key)
}

// nativeSACKey decodes the native SAC id and its contract-data key prefix.
func nativeSACKey(contractID string) (xdr.ContractId, string, error) {
	var id xdr.ContractId
	raw, err := strkey.Decode(strkey.VersionByteContract, contractID)
	if err != nil || len(raw) != len(id) {
		return id, "", fmt.Errorf("clickhouse: lumen conservation: bad native SAC id %q", contractID)
	}
	copy(id[:], raw)
	prefix, err := contractDataKeyPrefix(contractID)
	return id, prefix, err
}
