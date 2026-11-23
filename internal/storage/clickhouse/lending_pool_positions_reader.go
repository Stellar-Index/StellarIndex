package clickhouse

import (
	"context"
	"encoding/base64"
	"fmt"
	"math/big"
	"sort"

	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/scval"
)

// LendingPoolPositionsLimit caps the rows one pool-wide Positions read returns; a read that reaches it is Truncated.
const LendingPoolPositionsLimit = 50_000

// LendingPoolPosition is one account's current Positions entry in a pool, by reserve asset.
type LendingPoolPosition struct {
	Account string
	Supply  map[string]*big.Int // b-tokens, collateral and non-collateral summed
	Borrow  map[string]*big.Int // d-tokens
	Ledger  uint32              // the entry's last-modified ledger
}

// LendingPoolPositions is every current Positions entry of one pool the lake holds, in account order.
type LendingPoolPositions struct {
	// Readable is false when the pool's ResList is absent, archived or undecodable; Accounts is then empty and means nothing.
	Readable bool
	Accounts []LendingPoolPosition
	// Unreadable counts present entries left out of Accounts as archived or undecodable: positions the chain still holds.
	Unreadable int
	// Truncated is set when the read reached LendingPoolPositionsLimit, so entries past it are missing.
	Truncated bool
}

// poolPositionsQuery reads every current PoolDataKey::Positions(*) entry of one pool as a primary-key range: the table is
// ORDER BY (entry_type, key_xdr) and every such key starts with the same encoded bytes (positionsKeyPrefix).
const poolPositionsQuery = `SELECT key_xdr, entry_xdr, ledger_seq
	FROM stellar.ledger_entries_current FINAL
	WHERE entry_type = 'contract_data' AND startsWith(key_xdr, ?)
	LIMIT ?
	SETTINGS max_threads = 4, max_memory_usage = 8000000000, optimize_move_to_prewhere_if_final = 0`

// LendingPoolPositions reads every account's current Positions entry in one pool, and the ResList that maps their reserve
// indexes to assets.
func (r *ExplorerReader) LendingPoolPositions(ctx context.Context, pool string) (LendingPoolPositions, error) {
	poolID, err := contractIDFromStrkey(pool)
	if err != nil {
		return LendingPoolPositions{}, fmt.Errorf("clickhouse: lending pool id %q: %w", pool, err)
	}
	prefix, err := positionsKeyPrefix(poolID)
	if err != nil {
		return LendingPoolPositions{}, err
	}
	listKey, err := symbolKeyXDR(poolID, "ResList")
	if err != nil {
		return LendingPoolPositions{}, fmt.Errorf("clickhouse: reserve list key: %w", err)
	}
	list, err := r.readContractData(ctx, []string{listKey})
	if err != nil {
		return LendingPoolPositions{}, err
	}
	ce, n, err := r.queryContractData(ctx, poolPositionsQuery, prefix, LendingPoolPositionsLimit+1)
	if err != nil {
		return LendingPoolPositions{}, err
	}
	listVal, ok := list.vals[listKey]
	out := assemblePoolPositions(ce, listVal, ok && !list.archived[listKey])
	out.Truncated = n > LendingPoolPositionsLimit
	return out, nil
}

// positionsKeyPrefix is the base64 prefix every Positions(user) LedgerKey of the pool shares, cut at a whole base64 quantum
// so it is a prefix of each key's encoding; keys it also matches are dropped by positionsKeyAccount.
func positionsKeyPrefix(poolID xdr.ContractId) (string, error) {
	user := contractAddressVal(xdr.ContractId{})
	full, err := contractDataKeyXDR(poolID, "Positions", user)
	if err != nil {
		return "", fmt.Errorf("clickhouse: positions key: %w", err)
	}
	raw, err := base64.StdEncoding.DecodeString(full)
	if err != nil {
		return "", fmt.Errorf("clickhouse: positions key: %w", err)
	}
	userRaw, err := user.MarshalBinary()
	if err != nil {
		return "", fmt.Errorf("clickhouse: positions key user: %w", err)
	}
	n := len(raw) - len(userRaw) - 4 // the trailing 4 bytes are the durability
	n -= n % 3
	return base64.StdEncoding.EncodeToString(raw[:n]), nil
}

// positionsKeyAccount is the account a Positions(Address) LedgerKey names; false for any other key.
func positionsKeyAccount(keyB64 string) (string, bool) {
	var lk xdr.LedgerKey
	if xdr.SafeUnmarshalBase64(keyB64, &lk) != nil || lk.ContractData == nil {
		return "", false
	}
	vec, err := scval.AsVec(lk.ContractData.Key)
	if err != nil || len(vec) != 2 {
		return "", false
	}
	if sym, ok := vec[0].GetSym(); !ok || string(sym) != "Positions" {
		return "", false
	}
	account, err := scval.AsAddressStrkey(vec[1])
	return account, err == nil
}

func assemblePoolPositions(ce contractEntries, listVal xdr.ScVal, listOK bool) LendingPoolPositions {
	out := LendingPoolPositions{Accounts: []LendingPoolPosition{}}
	if !listOK {
		return out
	}
	list, err := decodeReserveList(listVal)
	if err != nil {
		return out
	}
	out.Readable = true
	for k := range ce.archived {
		if _, ok := positionsKeyAccount(k); ok {
			out.Unreadable++
		}
	}
	for k, v := range ce.vals {
		account, ok := positionsKeyAccount(k)
		if !ok || ce.archived[k] {
			continue
		}
		p := LendingAccountPositions{Supply: map[string]*big.Int{}, Borrow: map[string]*big.Int{}}
		if decodePositions(v, list, &p) != nil {
			out.Unreadable++
			continue
		}
		out.Accounts = append(out.Accounts, LendingPoolPosition{Account: account, Supply: p.Supply, Borrow: p.Borrow, Ledger: ce.ledger[k]})
	}
	sort.Slice(out.Accounts, func(i, j int) bool { return out.Accounts[i].Account < out.Accounts[j].Account })
	return out
}
