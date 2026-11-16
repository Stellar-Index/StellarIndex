package clickhouse

import (
	"context"
	"fmt"
	"math/big"

	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/scval"
)

// BackstopBalance is one backstop PoolBalance entry: shares issued, tokens held and shares queued for withdrawal.
type BackstopBalance struct {
	Shares *big.Int
	Tokens *big.Int
	Q4W    *big.Int
}

// BackstopTierState is one backstop tier's balance; Balance is nil when the lake holds no live entry (unavailable, never zero).
type BackstopTierState struct {
	Tier    string // "" for Blend's single, unnamed tier
	Balance *BackstopBalance
}

// LendingBackstopState is a lending pool's backstop as contract storage holds it.
type LendingBackstopState struct {
	Instance *PoolInstance // nil when the pool instance is missing, undecodable or archived; then nothing else is read
	Token    string        // Blend backstop instance "BToken"; "" when unread, and always for BLNT, whose tiers hold different tokens
	Tiers    []BackstopTierState
}

// blntBackstopTiers are BLNT's loss tiers in waterfall order. SecondLoss lives under the pre-tier PoolBalance(pool) key and the
// others under TierPoolBalance(PoolTierKey{pool, tier}) (backstop storage.rs get_pool_balance_for_tier), so no key holds a total.
var blntBackstopTiers = []string{"FirstLoss", "SecondLoss", "ThirdLoss"}

const (
	backstopKindBalance  = "PoolBalance"
	backstopKindInstance = "Instance"
)

// LendingBackstop reads a pool's current backstop balances from the backstop contract its instance names: Blend's one
// PoolBalance(pool) and backstop token, or BLNT's three tiers when tiered.
func (r *ExplorerReader) LendingBackstop(ctx context.Context, pool string, tiered bool) (LendingBackstopState, error) {
	poolID, err := contractIDFromStrkey(pool)
	if err != nil {
		return LendingBackstopState{}, fmt.Errorf("clickhouse: lending pool id %q: %w", pool, err)
	}
	instKeys, err := instanceKeyXDR(xdr.Hash(poolID))
	if err != nil {
		return LendingBackstopState{}, fmt.Errorf("clickhouse: lending pool instance key: %w", err)
	}
	vals, err := r.liveContractData(ctx, instKeys)
	if err != nil {
		return LendingBackstopState{}, err
	}
	var out LendingBackstopState
	for _, v := range vals {
		if pi, ok := poolInstanceFromStorage(v); ok {
			out.Instance = &pi
		}
	}
	if out.Instance == nil || out.Instance.Backstop == "" {
		return out, nil
	}
	backstopID, err := contractIDFromStrkey(out.Instance.Backstop)
	if err != nil {
		return LendingBackstopState{}, fmt.Errorf("clickhouse: backstop id %q: %w", out.Instance.Backstop, err)
	}
	keys, refByKey, err := backstopKeys(backstopID, poolID, tiered)
	if err != nil {
		return LendingBackstopState{}, err
	}
	if vals, err = r.liveContractData(ctx, keys); err != nil {
		return LendingBackstopState{}, err
	}
	out.Token, out.Tiers = assembleBackstop(vals, refByKey, tiered)
	return out, nil
}

// assembleBackstop decodes the read entries into tiers in waterfall order; an undecodable entry is unavailable, not zero.
func assembleBackstop(vals map[string]xdr.ScVal, refByKey map[string]keyRef, tiered bool) (string, []BackstopTierState) {
	tiers := []string{""}
	if tiered {
		tiers = blntBackstopTiers
	}
	byTier := make(map[string]*BackstopBalance, len(tiers))
	var token string
	for k, v := range vals {
		ref := refByKey[k]
		if ref.kind == backstopKindInstance {
			token = instanceAddress(v, "BToken")
			continue
		}
		if b, err := decodeBackstopBalance(v); err == nil {
			byTier[ref.asset] = &b
		}
	}
	out := make([]BackstopTierState, 0, len(tiers))
	for _, t := range tiers {
		out = append(out, BackstopTierState{Tier: t, Balance: byTier[t]})
	}
	return token, out
}

// backstopKeys builds the backstop entries to fetch; keyRef.asset carries the tier name.
func backstopKeys(backstopID, poolID xdr.ContractId, tiered bool) ([]string, map[string]keyRef, error) {
	refByKey := map[string]keyRef{}
	add := func(variant string, arg xdr.ScVal, tier string) error {
		k, err := contractDataKeyXDR(backstopID, variant, arg)
		if err != nil {
			return fmt.Errorf("clickhouse: backstop %s key: %w", variant, err)
		}
		refByKey[k] = keyRef{asset: tier, kind: backstopKindBalance}
		return nil
	}
	var err error
	if tiered {
		for _, tier := range blntBackstopTiers {
			if tier == "SecondLoss" {
				err = add("PoolBalance", contractAddressVal(poolID), tier)
			} else {
				err = add("TierPoolBalance", poolTierKeyVal(poolID, tier), tier)
			}
			if err != nil {
				return nil, nil, err
			}
		}
	} else {
		if err = add("PoolBalance", contractAddressVal(poolID), ""); err != nil {
			return nil, nil, err
		}
		instKeys, err := instanceKeyXDR(xdr.Hash(backstopID))
		if err != nil {
			return nil, nil, fmt.Errorf("clickhouse: backstop instance key: %w", err)
		}
		for _, k := range instKeys {
			refByKey[k] = keyRef{kind: backstopKindInstance}
		}
	}
	keys := make([]string, 0, len(refByKey))
	for k := range refByKey {
		keys = append(keys, k)
	}
	return keys, refByKey, nil
}

// poolTierKeyVal is the contracttype struct PoolTierKey{pool, tier}: a Map with symbol keys in sorted order, the unit-variant tier
// enum as Vec[Symbol].
func poolTierKeyVal(poolID xdr.ContractId, tier string) xdr.ScVal {
	poolSym, tierSym, tierName := xdr.ScSymbol("pool"), xdr.ScSymbol("tier"), xdr.ScSymbol(tier)
	tierVec := &xdr.ScVec{{Type: xdr.ScValTypeScvSymbol, Sym: &tierName}}
	m := &xdr.ScMap{
		{Key: xdr.ScVal{Type: xdr.ScValTypeScvSymbol, Sym: &poolSym}, Val: contractAddressVal(poolID)},
		{Key: xdr.ScVal{Type: xdr.ScValTypeScvSymbol, Sym: &tierSym}, Val: xdr.ScVal{Type: xdr.ScValTypeScvVec, Vec: &tierVec}},
	}
	return xdr.ScVal{Type: xdr.ScValTypeScvMap, Map: &m}
}

// decodeBackstopBalance decodes a PoolBalance struct {q4w, shares, tokens: i128} with the full 128 bits.
func decodeBackstopBalance(v xdr.ScVal) (BackstopBalance, error) {
	entries, err := scval.AsMap(v)
	if err != nil {
		return BackstopBalance{}, fmt.Errorf("PoolBalance shape: %w", err)
	}
	var out BackstopBalance
	for _, f := range []struct {
		name string
		dst  **big.Int
	}{{"shares", &out.Shares}, {"tokens", &out.Tokens}, {"q4w", &out.Q4W}} {
		sv, ok := scval.MapField(entries, f.name)
		if !ok {
			return BackstopBalance{}, fmt.Errorf("PoolBalance missing %q", f.name)
		}
		a, err := scval.AsAmountFromI128(sv)
		if err != nil {
			return BackstopBalance{}, fmt.Errorf("PoolBalance %s: %w", f.name, err)
		}
		*f.dst = a.BigInt()
	}
	return out, nil
}

// instanceAddress returns the address under a symbol key of an instance entry's storage, "" when absent.
func instanceAddress(v xdr.ScVal, key string) string {
	inst, ok := v.GetInstance()
	if !ok || inst.Storage == nil {
		return ""
	}
	for _, e := range *inst.Storage {
		if e.Key.Type == xdr.ScValTypeScvSymbol && e.Key.Sym != nil && string(*e.Key.Sym) == key {
			if a, err := scval.AsAddressStrkey(e.Val); err == nil {
				return a
			}
		}
	}
	return ""
}

// liveContractData reads current contract_data values by key and drops TTL-archived ones (the projection keeps a lapsed
// entry's last value); a key with no row is absent from the result.
func (r *ExplorerReader) liveContractData(ctx context.Context, keys []string) (map[string]xdr.ScVal, error) {
	rows, err := r.conn.Query(ctx, blendReserveStateQuery, keys)
	if err != nil {
		return nil, fmt.Errorf("clickhouse: contract data lookup: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := map[string]xdr.ScVal{}
	matched := make([]string, 0, len(keys))
	for rows.Next() {
		var keyXDR, b64 string
		if err := rows.Scan(&keyXDR, &b64); err != nil {
			return nil, fmt.Errorf("clickhouse: scan contract data: %w", err)
		}
		if v, ok := contractDataValue(b64); ok {
			out[keyXDR] = v
			matched = append(matched, keyXDR)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("clickhouse: contract data rows: %w", err)
	}
	if len(matched) == 0 {
		return out, nil
	}
	liveness, err := r.ttlVerdicts.resolve(ctx, matched)
	if err != nil {
		return nil, fmt.Errorf("clickhouse: contract data ttl liveness: %w", err)
	}
	for _, k := range matched {
		if liveness[k] == TTLArchived {
			delete(out, k)
		}
	}
	return out, nil
}
