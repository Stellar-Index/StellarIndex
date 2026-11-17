package clickhouse

import (
	"context"
	"errors"
	"fmt"
	"math/big"

	"github.com/stellar/go-stellar-sdk/strkey"
	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/scval"
)

// LendingAccountPositions is an account's current Positions entry in one lending pool, by reserve asset.
type LendingAccountPositions struct {
	// Readable is false when the entry or the pool's reserve list is archived or undecodable; the maps are then empty and
	// mean nothing. An absent entry is readable and empty: the contract reads it as no position.
	Readable bool
	Supply   map[string]*big.Int // b-tokens, collateral and non-collateral summed
	Borrow   map[string]*big.Int // d-tokens
	// Ledger is the entry's last-modified ledger, or its removal ledger; 0 when the lake never saw the key.
	Ledger uint32
}

// LendingAccountBackstop is an account's backstop shares for one pool per tier, and each tier entry's last-modified or
// removal ledger (absent when the lake never saw the key).
type LendingAccountBackstop struct {
	Held   map[string]*big.Int
	Ledger map[string]uint32
}

var errReserveIndex = errors.New("reserve index outside the pool's reserve list")

// LendingAccountPositions reads PoolDataKey::Positions(account) and the ResList that maps its reserve indexes to assets.
func (r *ExplorerReader) LendingAccountPositions(ctx context.Context, pool, account string) (LendingAccountPositions, error) {
	poolID, err := contractIDFromStrkey(pool)
	if err != nil {
		return LendingAccountPositions{}, fmt.Errorf("clickhouse: lending pool id %q: %w", pool, err)
	}
	user, err := addressScVal(account)
	if err != nil {
		return LendingAccountPositions{}, err
	}
	posKey, err := contractDataKeyXDR(poolID, "Positions", user)
	if err != nil {
		return LendingAccountPositions{}, fmt.Errorf("clickhouse: positions key: %w", err)
	}
	listKey, err := symbolKeyXDR(poolID, "ResList")
	if err != nil {
		return LendingAccountPositions{}, fmt.Errorf("clickhouse: reserve list key: %w", err)
	}
	ce, err := r.readContractData(ctx, []string{posKey, listKey})
	if err != nil {
		return LendingAccountPositions{}, err
	}
	out := assembleAccountPositions(ce.vals, ce.archived, posKey, listKey)
	out.Ledger = ce.ledger[posKey]
	return out, nil
}

func assembleAccountPositions(vals map[string]xdr.ScVal, archived map[string]bool, posKey, listKey string) LendingAccountPositions {
	out := LendingAccountPositions{Supply: map[string]*big.Int{}, Borrow: map[string]*big.Int{}}
	if archived[posKey] {
		return out
	}
	pos, ok := vals[posKey]
	if !ok {
		out.Readable = true
		return out
	}
	listVal, ok := vals[listKey]
	if !ok || archived[listKey] {
		return out
	}
	list, err := decodeReserveList(listVal)
	if err != nil {
		return out
	}
	if err := decodePositions(pos, list, &out); err != nil {
		return LendingAccountPositions{Supply: map[string]*big.Int{}, Borrow: map[string]*big.Int{}}
	}
	out.Readable = true
	return out
}

func decodeReserveList(v xdr.ScVal) ([]string, error) {
	vec, err := scval.AsVec(v)
	if err != nil {
		return nil, fmt.Errorf("ResList shape: %w", err)
	}
	out := make([]string, 0, len(vec))
	for _, a := range vec {
		s, err := scval.AsAddressStrkey(a)
		if err != nil {
			return nil, fmt.Errorf("ResList entry: %w", err)
		}
		out = append(out, s)
	}
	return out, nil
}

// decodePositions decodes Positions {collateral, liabilities, supply: Map<u32, i128>} into out by asset.
func decodePositions(v xdr.ScVal, list []string, out *LendingAccountPositions) error {
	entries, err := scval.AsMap(v)
	if err != nil {
		return fmt.Errorf("positions entry shape: %w", err)
	}
	for _, f := range []struct {
		name string
		dst  map[string]*big.Int
	}{{"collateral", out.Supply}, {"supply", out.Supply}, {"liabilities", out.Borrow}} {
		mv, ok := scval.MapField(entries, f.name)
		if !ok {
			return fmt.Errorf("positions entry missing %q", f.name)
		}
		if err := addIndexedBalances(mv, list, f.dst); err != nil {
			return fmt.Errorf("positions entry %s: %w", f.name, err)
		}
	}
	return nil
}

func addIndexedBalances(v xdr.ScVal, list []string, dst map[string]*big.Int) error {
	m, err := scval.AsMap(v)
	if err != nil {
		return err
	}
	for _, e := range m {
		idx, err := scval.AsU32(e.Key)
		if err != nil {
			return err
		}
		if int(idx) >= len(list) {
			return errReserveIndex
		}
		amt, err := scval.AsAmountFromI128(e.Val)
		if err != nil {
			return err
		}
		asset := list[idx]
		if dst[asset] == nil {
			dst[asset] = new(big.Int)
		}
		dst[asset].Add(dst[asset], amt.BigInt())
	}
	return nil
}

// LendingAccountBackstop reads an account's backstop shares for one pool per tier ("" is Blend's single tier), queued
// shares included. A tier whose entry is archived or undecodable is absent; a missing entry is zero, as the contract reads it.
func (r *ExplorerReader) LendingAccountBackstop(ctx context.Context, backstop, pool, account string, tiers []string) (LendingAccountBackstop, error) {
	backstopID, err := contractIDFromStrkey(backstop)
	if err != nil {
		return LendingAccountBackstop{}, fmt.Errorf("clickhouse: backstop id %q: %w", backstop, err)
	}
	poolID, err := contractIDFromStrkey(pool)
	if err != nil {
		return LendingAccountBackstop{}, fmt.Errorf("clickhouse: lending pool id %q: %w", pool, err)
	}
	user, err := addressScVal(account)
	if err != nil {
		return LendingAccountBackstop{}, err
	}
	tierByKey := make(map[string]string, len(tiers))
	keys := make([]string, 0, len(tiers))
	for _, tier := range tiers {
		k, err := userBalanceKeyXDR(backstopID, poolID, user, tier)
		if err != nil {
			return LendingAccountBackstop{}, fmt.Errorf("clickhouse: backstop user key: %w", err)
		}
		tierByKey[k] = tier
		keys = append(keys, k)
	}
	ce, err := r.readContractData(ctx, keys)
	if err != nil {
		return LendingAccountBackstop{}, err
	}
	return assembleAccountBackstop(ce, tierByKey), nil
}

func assembleAccountBackstop(ce contractEntries, tierByKey map[string]string) LendingAccountBackstop {
	out := LendingAccountBackstop{Held: make(map[string]*big.Int, len(tierByKey)), Ledger: map[string]uint32{}}
	for k, tier := range tierByKey {
		if l, ok := ce.ledger[k]; ok {
			out.Ledger[tier] = l
		}
		if ce.archived[k] {
			continue
		}
		v, ok := ce.vals[k]
		if !ok {
			out.Held[tier] = new(big.Int)
			continue
		}
		if held, err := decodeUserBalance(v); err == nil {
			out.Held[tier] = held
		}
	}
	return out
}

// userBalanceKeyXDR is UserBalance(PoolUserKey{pool, user}), or for a BLNT tier other than SecondLoss
// TierUserBalance(PoolUserTierKey{pool, tier, user}) (backstop storage.rs get_user_balance_for_tier).
func userBalanceKeyXDR(backstopID, poolID xdr.ContractId, user xdr.ScVal, tier string) (string, error) {
	poolSym, userSym := xdr.ScSymbol("pool"), xdr.ScSymbol("user")
	if tier == "" || tier == "SecondLoss" {
		m := &xdr.ScMap{
			{Key: xdr.ScVal{Type: xdr.ScValTypeScvSymbol, Sym: &poolSym}, Val: contractAddressVal(poolID)},
			{Key: xdr.ScVal{Type: xdr.ScValTypeScvSymbol, Sym: &userSym}, Val: user},
		}
		return contractDataKeyXDR(backstopID, "UserBalance", xdr.ScVal{Type: xdr.ScValTypeScvMap, Map: &m})
	}
	tierSym, tierName := xdr.ScSymbol("tier"), xdr.ScSymbol(tier)
	tierVec := &xdr.ScVec{{Type: xdr.ScValTypeScvSymbol, Sym: &tierName}}
	m := &xdr.ScMap{
		{Key: xdr.ScVal{Type: xdr.ScValTypeScvSymbol, Sym: &poolSym}, Val: contractAddressVal(poolID)},
		{Key: xdr.ScVal{Type: xdr.ScValTypeScvSymbol, Sym: &tierSym}, Val: xdr.ScVal{Type: xdr.ScValTypeScvVec, Vec: &tierVec}},
		{Key: xdr.ScVal{Type: xdr.ScValTypeScvSymbol, Sym: &userSym}, Val: user},
	}
	return contractDataKeyXDR(backstopID, "TierUserBalance", xdr.ScVal{Type: xdr.ScValTypeScvMap, Map: &m})
}

// decodeUserBalance returns shares plus every queued Q4W amount of a UserBalance {q4w: Vec<Q4W{amount, exp}>, shares}.
func decodeUserBalance(v xdr.ScVal) (*big.Int, error) {
	entries, err := scval.AsMap(v)
	if err != nil {
		return nil, fmt.Errorf("UserBalance shape: %w", err)
	}
	sv, okS := scval.MapField(entries, "shares")
	qv, okQ := scval.MapField(entries, "q4w")
	if !okS || !okQ {
		return nil, errors.New("UserBalance missing a field")
	}
	shares, err := scval.AsAmountFromI128(sv)
	if err != nil {
		return nil, fmt.Errorf("UserBalance shares: %w", err)
	}
	held := shares.BigInt()
	queue, err := scval.AsVec(qv)
	if err != nil {
		return nil, fmt.Errorf("UserBalance q4w: %w", err)
	}
	for _, q := range queue {
		qe, err := scval.AsMap(q)
		if err != nil {
			return nil, fmt.Errorf("Q4W shape: %w", err)
		}
		av, ok := scval.MapField(qe, "amount")
		if !ok {
			return nil, errors.New("Q4W missing amount")
		}
		a, err := scval.AsAmountFromI128(av)
		if err != nil {
			return nil, fmt.Errorf("Q4W amount: %w", err)
		}
		held.Add(held, a.BigInt())
	}
	return held, nil
}

// addressScVal encodes a G- or C-strkey as an Address ScVal.
func addressScVal(s string) (xdr.ScVal, error) {
	if strkey.IsValidEd25519PublicKey(s) {
		id, err := xdr.AddressToAccountId(s)
		if err != nil {
			return xdr.ScVal{}, fmt.Errorf("clickhouse: account %q: %w", s, err)
		}
		addr := xdr.ScAddress{Type: xdr.ScAddressTypeScAddressTypeAccount, AccountId: &id}
		return xdr.ScVal{Type: xdr.ScValTypeScvAddress, Address: &addr}, nil
	}
	id, err := contractIDFromStrkey(s)
	if err != nil {
		return xdr.ScVal{}, fmt.Errorf("clickhouse: address %q: %w", s, err)
	}
	return contractAddressVal(id), nil
}

// symbolKeyXDR builds the base64 LedgerKey of a persistent entry keyed by a bare Symbol.
func symbolKeyXDR(contract xdr.ContractId, sym string) (string, error) {
	s := xdr.ScSymbol(sym)
	lk := xdr.LedgerKey{
		Type: xdr.LedgerEntryTypeContractData,
		ContractData: &xdr.LedgerKeyContractData{
			Contract:   xdr.ScAddress{Type: xdr.ScAddressTypeScAddressTypeContract, ContractId: &contract},
			Key:        xdr.ScVal{Type: xdr.ScValTypeScvSymbol, Sym: &s},
			Durability: xdr.ContractDataDurabilityPersistent,
		},
	}
	return xdr.MarshalBase64(lk)
}
