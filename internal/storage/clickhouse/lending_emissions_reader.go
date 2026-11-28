package clickhouse

import (
	"cmp"
	"context"
	"fmt"
	"slices"

	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/scval"
)

// EmissionEPSDecimals is the fixed-point scale of every eps: emitted-token base units per second times 10^7 (pool
// emissions/manager.rs and backstop emissions in Blend V2 and BLNT both compute eps = tokens * SCALAR_7 / seconds).
const EmissionEPSDecimals = 7

// Emission stream sides: a reserve's bToken earns supply emissions, its dToken borrow emissions.
const (
	EmissionSideSupply = "supply"
	EmissionSideBorrow = "borrow"
)

// EmissionData is one emission stream's ReserveEmissionData / BackstopEmissionData.
type EmissionData struct {
	EPS        uint64 // at EmissionEPSDecimals
	Expiration uint64 // unix seconds
	LastTime   uint64 // unix seconds
}

// ReserveEmission is one reserve token's EmisData(token_id) entry.
type ReserveEmission struct {
	TokenID  uint32
	Asset    string // "" when the id is past the reserve list
	Side     string
	Data     EmissionData
	Archived bool // TTL-archived: the value is the last one written, restorable but not live
}

// BackstopEmission is a pool's BEmisData entry in its backstop, per BLNT tier ("" for Blend's single stream).
type BackstopEmission struct {
	Tier     string
	Data     EmissionData
	Archived bool
}

// LendingEmissionsState is a pool's emission configuration as contract storage holds it.
type LendingEmissionsState struct {
	Instance      *PoolInstance // nil when the pool instance is missing, undecodable or archived; then nothing else is read
	EmissionToken string        // the pool instance's BLNDTkn (Blend) or BLNTTkn (BLNT); "" when absent
	ReserveList   []string      // ResList; nil when unread
	Reserves      []ReserveEmission
	Backstop      []BackstopEmission // nil when the pool names no backstop
	// UndecodableTokenIDs are reserve token ids whose EmisData entry exists but failed to decode: their stream is unknown,
	// not absent.
	UndecodableTokenIDs []uint32
}

const (
	emisKindInstance = "Instance"
	emisKindResList  = "ResList"
	emisKindReserve  = "EmisData"
	emisKindBackstop = "BEmisData"
)

// emisRef maps a built key back to what it is; id is the reserve token id, tier the backstop tier.
type emisRef struct {
	kind string
	id   uint32
	tier string
}

// LendingEmissions reads a pool's reserve emission streams and its backstop emission stream(s) from current contract
// storage. tiered selects BLNT's layout: BLNTTkn and one BEmisData(PoolTierKey) per tier.
func (r *ExplorerReader) LendingEmissions(ctx context.Context, pool string, tiered bool) (LendingEmissionsState, error) {
	poolID, err := contractIDFromStrkey(pool)
	if err != nil {
		return LendingEmissionsState{}, fmt.Errorf("clickhouse: lending pool id %q: %w", pool, err)
	}
	refs, err := poolHeadKeys(poolID)
	if err != nil {
		return LendingEmissionsState{}, err
	}
	vals, archived, err := r.contractData(ctx, mapKeys(refs))
	if err != nil {
		return LendingEmissionsState{}, err
	}
	out := assembleEmissionHead(vals, archived, refs, tiered)
	if out.Instance == nil {
		return out, nil
	}
	refs, err = emissionStreamKeys(poolID, out, tiered)
	if err != nil {
		return LendingEmissionsState{}, err
	}
	vals, archived, err = r.contractData(ctx, mapKeys(refs))
	if err != nil {
		return LendingEmissionsState{}, err
	}
	assembleEmissionStreams(&out, vals, archived, refs)
	return out, nil
}

// poolHeadKeys are the pool instance keys and the persistent Symbol("ResList") key.
func poolHeadKeys(poolID xdr.ContractId) (map[string]emisRef, error) {
	inst, err := instanceKeyXDR(xdr.Hash(poolID))
	if err != nil {
		return nil, fmt.Errorf("clickhouse: lending pool instance key: %w", err)
	}
	refs := map[string]emisRef{}
	for _, k := range inst {
		refs[k] = emisRef{kind: emisKindInstance}
	}
	k, err := contractDataScKeyXDR(poolID, symbolScVal(emisKindResList))
	if err != nil {
		return nil, fmt.Errorf("clickhouse: ResList key: %w", err)
	}
	refs[k] = emisRef{kind: emisKindResList}
	return refs, nil
}

func assembleEmissionHead(vals map[string]xdr.ScVal, archived map[string]bool, refs map[string]emisRef, tiered bool) LendingEmissionsState {
	var out LendingEmissionsState
	tokenKey := "BLNDTkn"
	if tiered {
		tokenKey = "BLNTTkn"
	}
	for k, v := range vals {
		switch refs[k].kind {
		case emisKindInstance:
			if pi, ok := poolInstanceFromStorage(v); ok && !archived[k] {
				out.Instance = &pi
				out.EmissionToken = instanceAddress(v, tokenKey)
			}
		case emisKindResList:
			// Append-only (storage.rs push_res_list), so an archived entry still names every reserve's index.
			if list, err := decodeAddressList(v); err == nil {
				out.ReserveList = list
			}
		}
	}
	return out
}

func decodeAddressList(v xdr.ScVal) ([]string, error) {
	vec, err := scval.AsVec(v)
	if err != nil {
		return nil, fmt.Errorf("ResList shape: %w", err)
	}
	out := make([]string, 0, len(vec))
	for _, e := range vec {
		a, err := scval.AsAddressStrkey(e)
		if err != nil {
			return nil, fmt.Errorf("ResList entry: %w", err)
		}
		out = append(out, a)
	}
	return out, nil
}

// emissionStreamKeys builds EmisData(id) for both tokens of every listed reserve and the backstop's BEmisData key(s).
func emissionStreamKeys(poolID xdr.ContractId, st LendingEmissionsState, tiered bool) (map[string]emisRef, error) {
	refs := map[string]emisRef{}
	for id := range uint32(2 * len(st.ReserveList)) { //nolint:gosec // ResList holds at most MAX_RESERVES entries
		k, err := contractDataKeyXDR(poolID, emisKindReserve, u32ScVal(id))
		if err != nil {
			return nil, fmt.Errorf("clickhouse: EmisData(%d) key: %w", id, err)
		}
		refs[k] = emisRef{kind: emisKindReserve, id: id}
	}
	if st.Instance.Backstop == "" {
		return refs, nil
	}
	backstopID, err := contractIDFromStrkey(st.Instance.Backstop)
	if err != nil {
		return nil, fmt.Errorf("clickhouse: backstop id %q: %w", st.Instance.Backstop, err)
	}
	args := map[string]xdr.ScVal{"": contractAddressVal(poolID)}
	if tiered {
		args = map[string]xdr.ScVal{}
		for _, t := range blntBackstopTiers {
			args[t] = poolTierKeyVal(poolID, t)
		}
	}
	for tier, arg := range args {
		k, err := contractDataKeyXDR(backstopID, emisKindBackstop, arg)
		if err != nil {
			return nil, fmt.Errorf("clickhouse: BEmisData key: %w", err)
		}
		refs[k] = emisRef{kind: emisKindBackstop, tier: tier}
	}
	return refs, nil
}

// assembleEmissionStreams decodes the stream entries in token-id, then tier, order. An undecodable reserve entry is named
// in UndecodableTokenIDs; an undecodable backstop entry is skipped.
func assembleEmissionStreams(out *LendingEmissionsState, vals map[string]xdr.ScVal, archived map[string]bool, refs map[string]emisRef) {
	out.Reserves = []ReserveEmission{}
	backstop := map[string]BackstopEmission{}
	for k, v := range vals {
		ref := refs[k]
		d, err := decodeEmissionData(v)
		if err != nil {
			if ref.kind == emisKindReserve {
				out.UndecodableTokenIDs = append(out.UndecodableTokenIDs, ref.id)
			}
			continue
		}
		switch ref.kind {
		case emisKindReserve:
			asset, side, _ := ReserveTokenSide(ref.id, out.ReserveList)
			out.Reserves = append(out.Reserves, ReserveEmission{TokenID: ref.id, Asset: asset, Side: side, Data: d, Archived: archived[k]})
		case emisKindBackstop:
			backstop[ref.tier] = BackstopEmission{Tier: ref.tier, Data: d, Archived: archived[k]}
		}
	}
	slices.SortFunc(out.Reserves, func(a, b ReserveEmission) int { return cmp.Compare(a.TokenID, b.TokenID) })
	slices.Sort(out.UndecodableTokenIDs)
	if out.Instance.Backstop == "" {
		return
	}
	out.Backstop = []BackstopEmission{}
	for _, t := range append([]string{""}, blntBackstopTiers...) {
		if b, ok := backstop[t]; ok {
			out.Backstop = append(out.Backstop, b)
		}
	}
}

// ReserveTokenSide maps an emission token id to its reserve and side: id = index*2 for the dToken (borrow) and
// index*2+1 for the bToken (supply) (pool user.rs). ok is false when the index is past the reserve list.
func ReserveTokenSide(id uint32, reserves []string) (asset, side string, ok bool) {
	side = EmissionSideBorrow
	if id%2 == 1 {
		side = EmissionSideSupply
	}
	idx := int(id / 2)
	if idx >= len(reserves) {
		return "", side, false
	}
	return reserves[idx], side, true
}

// decodeEmissionData reads the expiration, eps and last_time fields shared by Blend V2's ReserveEmissionData and
// BackstopEmissionData and BLNT's StoredReserveEmissionData and BackstopEmissionData; the other fields differ by version.
func decodeEmissionData(v xdr.ScVal) (EmissionData, error) {
	entries, err := scval.AsMap(v)
	if err != nil {
		return EmissionData{}, fmt.Errorf("emission data shape: %w", err)
	}
	var out EmissionData
	for _, f := range []struct {
		name string
		dst  *uint64
	}{{"eps", &out.EPS}, {"expiration", &out.Expiration}, {"last_time", &out.LastTime}} {
		fv, ok := scval.MapField(entries, f.name)
		if !ok {
			return EmissionData{}, fmt.Errorf("emission data missing %q", f.name)
		}
		n, err := scval.AsU64(fv)
		if err != nil {
			return EmissionData{}, fmt.Errorf("emission data %s: %w", f.name, err)
		}
		*f.dst = n
	}
	return out, nil
}

// contractDataScKeyXDR is the base64 LedgerKey of a contract's persistent entry under an arbitrary ScVal key.
func contractDataScKeyXDR(contract xdr.ContractId, key xdr.ScVal) (string, error) {
	return xdr.MarshalBase64(xdr.LedgerKey{
		Type: xdr.LedgerEntryTypeContractData,
		ContractData: &xdr.LedgerKeyContractData{
			Contract:   xdr.ScAddress{Type: xdr.ScAddressTypeScAddressTypeContract, ContractId: &contract},
			Key:        key,
			Durability: xdr.ContractDataDurabilityPersistent,
		},
	})
}

func mapKeys(refs map[string]emisRef) []string {
	out := make([]string, 0, len(refs))
	for k := range refs {
		out = append(out, k)
	}
	return out
}
