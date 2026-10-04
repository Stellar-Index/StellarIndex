package xdrjson

import (
	"encoding/base64"
	"strconv"

	"github.com/stellar/go-stellar-sdk/xdr"
)

// LedgerEntryFields decodes a classic ledger entry (account, trustline, offer,
// data, claimable balance, liquidity pool) to its state fields; ok=false for
// every other entry type. last_modified_ledger is left out on purpose: it moves
// on every write, so it would mark every field-level diff as changed.
func LedgerEntryFields(e xdr.LedgerEntry) (map[string]any, bool) {
	var f map[string]any
	switch e.Data.Type {
	case xdr.LedgerEntryTypeAccount:
		f = accountEntryFields(e.Data.MustAccount())
	case xdr.LedgerEntryTypeTrustline:
		f = trustlineEntryFields(e.Data.MustTrustLine())
	case xdr.LedgerEntryTypeOffer:
		f = offerEntryFields(e.Data.MustOffer())
	case xdr.LedgerEntryTypeData:
		d := e.Data.MustData()
		f = map[string]any{
			"account_id": d.AccountId.Address(),
			"name":       string(d.DataName),
			"value":      base64.StdEncoding.EncodeToString(d.DataValue),
		}
	case xdr.LedgerEntryTypeClaimableBalance:
		f = claimableBalanceEntryFields(e.Data.MustClaimableBalance())
	case xdr.LedgerEntryTypeLiquidityPool:
		f = liquidityPoolEntryFields(e.Data.MustLiquidityPool())
	default:
		return nil, false
	}
	if s := e.SponsoringID(); s != nil {
		f["sponsor"] = s.Address()
	}
	return f, true
}

func accountEntryFields(a xdr.AccountEntry) map[string]any {
	sponsors := a.SignerSponsoringIDs()
	signers := make([]map[string]any, 0, len(a.Signers))
	for i, s := range a.Signers {
		key, _ := s.Key.GetAddress()
		sg := map[string]any{"key": key, "weight": uint32(s.Weight)}
		if i < len(sponsors) && sponsors[i] != nil {
			sg["sponsor"] = sponsors[i].Address()
		}
		signers = append(signers, sg)
	}
	liab := a.Liabilities()
	f := map[string]any{
		"account_id":          a.AccountId.Address(),
		"balance":             amount(int64(a.Balance)),
		"seq_num":             strconv.FormatInt(int64(a.SeqNum), 10),
		"num_subentries":      uint32(a.NumSubEntries),
		"flags":               uint32(a.Flags),
		"home_domain":         string(a.HomeDomain),
		"master_weight":       a.MasterKeyWeight(),
		"threshold_low":       a.ThresholdLow(),
		"threshold_medium":    a.ThresholdMedium(),
		"threshold_high":      a.ThresholdHigh(),
		"signers":             signers,
		"buying_liabilities":  amount(int64(liab.Buying)),
		"selling_liabilities": amount(int64(liab.Selling)),
		"num_sponsored":       uint32(a.NumSponsored()),
		"num_sponsoring":      uint32(a.NumSponsoring()),
		"seq_ledger":          uint32(a.SeqLedger()),
		"seq_time":            strconv.FormatUint(uint64(a.SeqTime()), 10),
	}
	if a.InflationDest != nil {
		f["inflation_dest"] = a.InflationDest.Address()
	}
	return f
}

func trustlineEntryFields(t xdr.TrustLineEntry) map[string]any {
	liab := t.Liabilities()
	f := map[string]any{
		"account_id":          t.AccountId.Address(),
		"asset":               TrustLineAssetID(t.Asset),
		"balance":             amount(int64(t.Balance)),
		"limit":               amount(int64(t.Limit)),
		"flags":               uint32(t.Flags),
		"buying_liabilities":  amount(int64(liab.Buying)),
		"selling_liabilities": amount(int64(liab.Selling)),
	}
	if t.Ext.V1 != nil && t.Ext.V1.Ext.V2 != nil {
		f["liquidity_pool_use_count"] = int32(t.Ext.V1.Ext.V2.LiquidityPoolUseCount)
	}
	return f
}

func offerEntryFields(o xdr.OfferEntry) map[string]any {
	return map[string]any{
		"seller_id": o.SellerId.Address(),
		"offer_id":  strconv.FormatInt(int64(o.OfferId), 10),
		"selling":   assetID(o.Selling),
		"buying":    assetID(o.Buying),
		"amount":    amount(int64(o.Amount)),
		"price":     price(o.Price),
		"flags":     uint32(o.Flags),
	}
}

func claimableBalanceEntryFields(cb xdr.ClaimableBalanceEntry) map[string]any {
	id, _ := claimableBalanceIDHex(cb.BalanceId)
	f := map[string]any{
		"balance_id": id,
		"asset":      assetID(cb.Asset),
		"amount":     amount(int64(cb.Amount)),
		"claimants":  claimantsFields(cb.Claimants),
	}
	if cb.Ext.V1 != nil {
		f["flags"] = uint32(cb.Ext.V1.Flags)
	}
	return f
}

func liquidityPoolEntryFields(lp xdr.LiquidityPoolEntry) map[string]any {
	f := map[string]any{"pool_id": poolIDHex(lp.LiquidityPoolId)}
	if cp, ok := lp.Body.GetConstantProduct(); ok {
		f["asset_a"] = assetID(cp.Params.AssetA)
		f["asset_b"] = assetID(cp.Params.AssetB)
		f["fee_bp"] = int32(cp.Params.Fee)
		f["reserve_a"] = amount(int64(cp.ReserveA))
		f["reserve_b"] = amount(int64(cp.ReserveB))
		f["total_shares"] = amount(int64(cp.TotalPoolShares))
		f["trustline_count"] = strconv.FormatInt(int64(cp.PoolSharesTrustLineCount), 10)
	}
	return f
}
