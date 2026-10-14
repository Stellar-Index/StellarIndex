// Package entryhistory turns ledger-entry-change source rows into account and asset history rows;
// the ch-entry-history derive and the entry-changes read path share one projector.
package entryhistory

import (
	"encoding/json"
	"fmt"
	"math/big"
	"reflect"
	"slices"
	"sort"

	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/xdrjson"
)

// Projector turns source rows into history rows and needs source sort-key order: core emits each update's
// and removal's 'state' pre-image just before it in the same (tx, op_index) group, so only the latest per key is kept.
type Projector struct {
	ledger uint32
	tx     string
	op     int32
	pre    map[string]xdr.LedgerEntry
	// PreImageMissing counts removals the lake holds no pre-image for: their sponsorship end is unknowable.
	PreImageMissing uint64
}

func NewProjector() *Projector {
	return &Projector{pre: map[string]xdr.LedgerEntry{}}
}

// Projection is everything one source row derives.
type Projection struct {
	Accounts     []clickhouse.AccountEntryChange
	Assets       []clickhouse.AssetEntryChange
	Sponsorships []clickhouse.SponsorshipEvent
}

// Project derives the account and asset rows of one source row; ProjectAll adds the sponsorship events.
func (p *Projector) Project(r clickhouse.EntryHistorySourceRow) ([]clickhouse.AccountEntryChange, []clickhouse.AssetEntryChange, error) {
	pr, err := p.ProjectAll(r)
	return pr.Accounts, pr.Assets, err
}

// ProjectAll derives every history row of one source row.
func (p *Projector) ProjectAll(r clickhouse.EntryHistorySourceRow) (Projection, error) {
	if r.Ledger != p.ledger || r.TxHash != p.tx || r.OpIndex != p.op {
		p.ledger, p.tx, p.op = r.Ledger, r.TxHash, r.OpIndex
		clear(p.pre)
	}
	var post *xdr.LedgerEntry
	if r.EntryXDR != "" {
		var e xdr.LedgerEntry
		if err := xdr.SafeUnmarshalBase64(r.EntryXDR, &e); err != nil {
			return Projection{}, fmt.Errorf("decode entry: %w", err)
		}
		post = &e
	}
	if r.ChangeType == "state" {
		// A pre-image is never itself a change. It stays in the map (not
		// consumed) so an unmerged duplicate of the change sees it too.
		if post == nil {
			return Projection{}, fmt.Errorf("state row without an entry")
		}
		p.pre[r.KeyXDR] = *post
		return Projection{}, nil
	}
	if post == nil && r.ChangeType != "removed" {
		return Projection{}, fmt.Errorf("%s row without an entry", r.ChangeType)
	}
	var prev *xdr.LedgerEntry
	if e, ok := p.pre[r.KeyXDR]; ok {
		prev = &e
	}

	postFields, err := entryFields(post)
	if err != nil {
		return Projection{}, err
	}
	prevFields, err := entryFields(prev)
	if err != nil {
		return Projection{}, err
	}
	shown := postFields
	if post == nil {
		shown = prevFields // a removal records the entry's last state
	}
	if shown == nil {
		shown = map[string]any{}
	}
	fieldsJSON, err := json.Marshal(shown)
	if err != nil {
		return Projection{}, fmt.Errorf("marshal fields: %w", err)
	}
	ref := clickhouse.EntryChangeRef{
		Ledger: r.Ledger, CloseTime: r.CloseTime, TxHash: r.TxHash, OpIndex: r.OpIndex,
		ChangeIndex: r.ChangeIndex, IntraLedgerSeq: r.IntraLedgerSeq,
		EntryType: r.EntryType, ChangeType: r.ChangeType,
		Changed: changedFields(r.ChangeType, prevFields, postFields),
		Fields:  string(fieldsJSON),
	}

	var key *xdr.LedgerKey
	if post == nil && prev == nil {
		p.PreImageMissing++
		var k xdr.LedgerKey
		if err := xdr.SafeUnmarshalBase64(r.KeyXDR, &k); err != nil {
			return Projection{}, fmt.Errorf("decode key: %w", err)
		}
		key = &k
	}
	return Projection{
		Accounts:     accountRows(ref, prev, post, key),
		Assets:       assetRows(ref, prev, post, key),
		Sponsorships: sponsorshipEvents(ref, prev, post),
	}, nil
}

func entryFields(e *xdr.LedgerEntry) (map[string]any, error) {
	if e == nil {
		return nil, nil //nolint:nilnil // no image is not an error
	}
	f, ok := xdrjson.LedgerEntryFields(*e)
	if !ok {
		return nil, fmt.Errorf("unsupported entry type %s", e.Data.Type)
	}
	return f, nil
}

// changedFields names the fields an update or restore altered; with no pre-image it reports every field, so a
// field-filtered history read never misses it.
func changedFields(changeType string, prev, post map[string]any) []string {
	if changeType != "updated" && changeType != "restored" {
		return nil
	}
	if prev == nil {
		if changeType == "restored" {
			return nil
		}
		return fieldNames(post)
	}
	var out []string
	for k, v := range post {
		if pv, ok := prev[k]; !ok || !reflect.DeepEqual(pv, v) {
			out = append(out, k)
		}
	}
	for k := range prev {
		if _, ok := post[k]; !ok {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

func fieldNames(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// entryParty is one account an entry concerns, in one role, with the asset and amount it holds through the entry, if any.
type entryParty struct {
	account, role, asset string
	balance              int64
}

func entryParties(e *xdr.LedgerEntry) []entryParty {
	var out []entryParty
	switch e.Data.Type {
	case xdr.LedgerEntryTypeAccount:
		a := e.Data.MustAccount()
		out = append(out, entryParty{account: a.AccountId.Address(), role: "owner", asset: "native", balance: int64(a.Balance)})
		for _, s := range a.SignerSponsoringIDs() {
			if s != nil {
				out = append(out, entryParty{account: s.Address(), role: "sponsor"})
			}
		}
	case xdr.LedgerEntryTypeTrustline:
		t := e.Data.MustTrustLine()
		out = append(out, entryParty{account: t.AccountId.Address(), role: "owner", asset: xdrjson.TrustLineAssetID(t.Asset), balance: int64(t.Balance)})
	case xdr.LedgerEntryTypeOffer:
		out = append(out, entryParty{account: e.Data.MustOffer().SellerId.Address(), role: "owner"})
	case xdr.LedgerEntryTypeData:
		out = append(out, entryParty{account: e.Data.MustData().AccountId.Address(), role: "owner"})
	case xdr.LedgerEntryTypeClaimableBalance:
		cb := e.Data.MustClaimableBalance()
		for _, c := range cb.Claimants {
			if v0, ok := c.GetV0(); ok {
				out = append(out, entryParty{account: v0.Destination.Address(), role: "claimant", asset: xdrjson.AssetID(cb.Asset), balance: int64(cb.Amount)})
			}
		}
	}
	if s := e.SponsoringID(); s != nil {
		out = append(out, entryParty{account: s.Address(), role: "sponsor"})
	}
	return out
}

// keyParties is the owner a removal's key names when no pre-image was seen.
func keyParties(k *xdr.LedgerKey) []entryParty {
	switch k.Type {
	case xdr.LedgerEntryTypeAccount:
		return []entryParty{{account: k.MustAccount().AccountId.Address(), role: "owner", asset: "native"}}
	case xdr.LedgerEntryTypeTrustline:
		t := k.MustTrustLine()
		return []entryParty{{account: t.AccountId.Address(), role: "owner", asset: xdrjson.TrustLineAssetID(t.Asset)}}
	case xdr.LedgerEntryTypeOffer:
		return []entryParty{{account: k.MustOffer().SellerId.Address(), role: "owner"}}
	case xdr.LedgerEntryTypeData:
		return []entryParty{{account: k.MustData().AccountId.Address(), role: "owner"}}
	}
	return nil
}

// accountRows fans a change out to every account it concerns before or after (a sponsor whose sponsorship was revoked
// still sees it); a party only the pre-image names holds nothing through the entry any more (balance 0).
func accountRows(ref clickhouse.EntryChangeRef, prev, post *xdr.LedgerEntry, key *xdr.LedgerKey) []clickhouse.AccountEntryChange {
	var parties []entryParty
	if post != nil {
		parties = entryParties(post)
	}
	var gone []entryParty
	switch {
	case prev != nil:
		gone = entryParties(prev)
	case key != nil:
		gone = keyParties(key)
	}
	for _, g := range gone {
		if !slices.ContainsFunc(parties, func(p entryParty) bool { return p.account == g.account && p.role == g.role }) {
			g.balance = 0
			parties = append(parties, g)
		}
	}
	out := make([]clickhouse.AccountEntryChange, 0, len(parties))
	for _, p := range parties {
		if slices.ContainsFunc(out, func(o clickhouse.AccountEntryChange) bool { return o.Account == p.account && o.Role == p.role }) {
			continue // one account sponsoring both the entry and a signer
		}
		out = append(out, clickhouse.AccountEntryChange{
			EntryChangeRef: ref, Account: p.account, Role: p.role, Asset: p.asset, Balance: big.NewInt(p.balance),
		})
	}
	return out
}

// entryAsset is one asset an entry concerns, in one role, with the amount of
// it the entry holds and the account holding, selling or sponsoring it.
type entryAsset struct {
	asset, role, account string
	balance              int64
}

func entryAssets(e *xdr.LedgerEntry) []entryAsset {
	switch e.Data.Type {
	case xdr.LedgerEntryTypeTrustline:
		t := e.Data.MustTrustLine()
		return []entryAsset{{asset: xdrjson.TrustLineAssetID(t.Asset), role: "holder", account: t.AccountId.Address(), balance: int64(t.Balance)}}
	case xdr.LedgerEntryTypeOffer:
		o := e.Data.MustOffer()
		seller := o.SellerId.Address()
		return []entryAsset{
			{asset: xdrjson.AssetID(o.Selling), role: "selling", account: seller, balance: int64(o.Amount)},
			{asset: xdrjson.AssetID(o.Buying), role: "buying", account: seller},
		}
	case xdr.LedgerEntryTypeClaimableBalance:
		cb := e.Data.MustClaimableBalance()
		var sponsor string
		if s := e.SponsoringID(); s != nil {
			sponsor = s.Address()
		}
		return []entryAsset{{asset: xdrjson.AssetID(cb.Asset), role: "claimable", account: sponsor, balance: int64(cb.Amount)}}
	case xdr.LedgerEntryTypeLiquidityPool:
		lp := e.Data.MustLiquidityPool()
		out := []entryAsset{{asset: poolAssetID(lp.LiquidityPoolId), role: "pool"}}
		if cp, ok := lp.Body.GetConstantProduct(); ok {
			out[0].balance = int64(cp.TotalPoolShares)
			out = append(out,
				entryAsset{asset: xdrjson.AssetID(cp.Params.AssetA), role: "reserve_a", balance: int64(cp.ReserveA)},
				entryAsset{asset: xdrjson.AssetID(cp.Params.AssetB), role: "reserve_b", balance: int64(cp.ReserveB)})
		}
		return out
	}
	return nil
}

func keyAssets(k *xdr.LedgerKey) []entryAsset {
	switch k.Type {
	case xdr.LedgerEntryTypeTrustline:
		t := k.MustTrustLine()
		return []entryAsset{{asset: xdrjson.TrustLineAssetID(t.Asset), role: "holder", account: t.AccountId.Address()}}
	case xdr.LedgerEntryTypeLiquidityPool:
		return []entryAsset{{asset: poolAssetID(k.MustLiquidityPool().LiquidityPoolId), role: "pool"}}
	}
	return nil
}

// poolAssetID spells a pool the way a pool-share trustline's asset is spelled.
func poolAssetID(id xdr.PoolId) string {
	return xdrjson.TrustLineAssetID(xdr.TrustLineAsset{Type: xdr.AssetTypeAssetTypePoolShare, LiquidityPoolId: &id})
}

// assetRows fans a change out to every asset it concerns. Account entries are absent: native's per-holder history is
// every account-entry change, already served account-keyed.
func assetRows(ref clickhouse.EntryChangeRef, prev, post *xdr.LedgerEntry, key *xdr.LedgerKey) []clickhouse.AssetEntryChange {
	var assets []entryAsset
	if post != nil {
		assets = entryAssets(post)
	}
	var gone []entryAsset
	switch {
	case prev != nil:
		gone = entryAssets(prev)
	case key != nil:
		gone = keyAssets(key)
	}
	for _, g := range gone {
		if !slices.ContainsFunc(assets, func(a entryAsset) bool { return a.asset == g.asset && a.role == g.role }) {
			g.balance = 0
			assets = append(assets, g)
		}
	}
	out := make([]clickhouse.AssetEntryChange, 0, len(assets))
	for _, a := range assets {
		out = append(out, clickhouse.AssetEntryChange{
			EntryChangeRef: ref, Asset: a.asset, Role: a.role, Account: a.account, Balance: big.NewInt(a.balance),
		})
	}
	return out
}
