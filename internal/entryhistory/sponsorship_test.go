package entryhistory

import (
	"testing"

	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/xdrjson"
)

const (
	spAlice = "GAAZI4TCR3TY5OJHCTJC2A4QSY6CJWJH5IAJTGKIN2ER7LBNVKOCCWN7"
	spBob   = "GBRPYHIL2CI3FNQ4BXLFMNDLFJUNPU2HY3ZMFSHONUCEOASW7QC7OX2H"
	spCarol = "GCEZWKCA5VLDNRLN3RPRJMRZOX3Z6G5CHCGSNFHEYVXM3XOJMDS674JZ"
)

func spSponsored(e xdr.LedgerEntry, sponsor string) *xdr.LedgerEntry {
	if sponsor != "" {
		e.Ext = xdr.LedgerEntryExt{V: 1, V1: &xdr.LedgerEntryExtensionV1{SponsoringId: xdr.MustAddressPtr(sponsor)}}
	}
	return &e
}

func spTrustline(poolShare bool) xdr.LedgerEntry {
	asset := xdr.MustNewCreditAsset("USDC", spCarol).ToTrustLineAsset()
	if poolShare {
		asset = xdr.TrustLineAsset{Type: xdr.AssetTypeAssetTypePoolShare, LiquidityPoolId: &xdr.PoolId{1}}
	}
	return xdr.LedgerEntry{Data: xdr.LedgerEntryData{Type: xdr.LedgerEntryTypeTrustline, TrustLine: &xdr.TrustLineEntry{
		AccountId: xdr.MustAddress(spBob), Asset: asset, Limit: 1,
	}}}
}

// spAccount is bob with the given (signer key, sponsor) pairs; "" means an unsponsored signer.
func spAccount(balance int64, signers ...[2]string) xdr.LedgerEntry {
	acc := &xdr.AccountEntry{AccountId: xdr.MustAddress(spBob), Balance: xdr.Int64(balance), Thresholds: xdr.Thresholds{1, 0, 0, 0}}
	var ids []xdr.SponsorshipDescriptor
	for _, s := range signers {
		acc.Signers = append(acc.Signers, xdr.Signer{Key: xdr.MustSigner(s[0]), Weight: 1})
		var id xdr.SponsorshipDescriptor
		if s[1] != "" {
			id = xdr.MustAddressPtr(s[1])
		}
		ids = append(ids, id)
	}
	if len(signers) > 0 {
		acc.Ext = xdr.AccountEntryExt{V: 1, V1: &xdr.AccountEntryExtensionV1{Ext: xdr.AccountEntryExtensionV1Ext{
			V: 2, V2: &xdr.AccountEntryExtensionV2{SignerSponsoringIDs: ids},
		}}}
	}
	return xdr.LedgerEntry{Data: xdr.LedgerEntryData{Type: xdr.LedgerEntryTypeAccount, Account: acc}}
}

func spClaimable(claimants int) xdr.LedgerEntry {
	cb := &xdr.ClaimableBalanceEntry{
		BalanceId: xdr.ClaimableBalanceId{Type: xdr.ClaimableBalanceIdTypeClaimableBalanceIdTypeV0, V0: &xdr.Hash{7}},
		Asset:     xdr.MustNewNativeAsset(), Amount: 1,
	}
	for range claimants {
		cb.Claimants = append(cb.Claimants, xdr.Claimant{Type: xdr.ClaimantTypeClaimantTypeV0, V0: &xdr.ClaimantV0{
			Destination: xdr.MustAddress(spAlice), Predicate: xdr.ClaimPredicate{Type: xdr.ClaimPredicateTypeClaimPredicateUnconditional},
		}})
	}
	return xdr.LedgerEntry{Data: xdr.LedgerEntryData{Type: xdr.LedgerEntryTypeClaimableBalance, ClaimableBalance: cb}}
}

func TestSponsorshipEvents_Transitions(t *testing.T) {
	ref := clickhouse.EntryChangeRef{Ledger: 5, TxHash: "ab", OpIndex: 2, ChangeIndex: 3, IntraLedgerSeq: 9}
	type ev struct {
		sponsor, sponsored, kind, keyID, event string
		reserves                               uint8
	}
	usdc := "USDC-" + spCarol
	pool := xdrjson.TrustLineAssetID(spTrustline(true).Data.TrustLine.Asset)
	cbID, _ := xdrjson.ClaimableBalanceIDHex(spClaimable(1).Data.ClaimableBalance.BalanceId)
	tl := spTrustline(false)
	cases := []struct {
		name       string
		prev, post *xdr.LedgerEntry
		want       []ev
	}{
		{"sponsored trustline created", nil, spSponsored(tl, spAlice), []ev{{spAlice, spBob, "trustline", usdc, "started", 1}}},
		{"unsponsored trustline created", nil, spSponsored(tl, ""), nil},
		{
			"sponsorship transferred in one change", spSponsored(tl, spAlice), spSponsored(tl, spCarol),
			[]ev{{spAlice, spBob, "trustline", usdc, "revoked", 1}, {spCarol, spBob, "trustline", usdc, "started", 1}},
		},
		{"sponsorship revoked, entry lives on", spSponsored(tl, spAlice), spSponsored(tl, ""), []ev{{spAlice, spBob, "trustline", usdc, "revoked", 1}}},
		{"sponsored entry removed", spSponsored(tl, spAlice), nil, []ev{{spAlice, spBob, "trustline", usdc, "ended", 1}}},
		{"removal without a pre-image", nil, nil, nil},
		{"pool share trustline weighs two", nil, spSponsored(spTrustline(true), spAlice), []ev{{spAlice, spBob, "pool_share", pool, "started", 2}}},
		{
			"claimable balance counts to the sponsor only", nil, spSponsored(spClaimable(3), spAlice),
			[]ev{{spAlice, "", "claimable_balance", cbID, "started", 3}},
		},
		{
			"sponsored signer added", spSponsored(spAccount(1), ""), spSponsored(spAccount(1, [2]string{spCarol, spAlice}), ""),
			[]ev{{spAlice, spBob, "signer", spCarol, "started", 1}},
		},
		{
			"sponsored signer dropped", spSponsored(spAccount(1, [2]string{spCarol, spAlice}), ""), spSponsored(spAccount(1), ""),
			[]ev{{spAlice, spBob, "signer", spCarol, "ended", 1}},
		},
		{
			"signer sponsor transferred", spSponsored(spAccount(1, [2]string{spCarol, spAlice}), ""),
			spSponsored(spAccount(1, [2]string{spCarol, spCarol}), ""),
			[]ev{{spAlice, spBob, "signer", spCarol, "revoked", 1}, {spCarol, spBob, "signer", spCarol, "started", 1}},
		},
		{
			"balance change on a sponsored signer is silent", spSponsored(spAccount(1, [2]string{spCarol, spAlice}), ""),
			spSponsored(spAccount(2, [2]string{spCarol, spAlice}), ""), nil,
		},
		{
			"sponsored account merged with its signers", spSponsored(spAccount(1, [2]string{spAlice, spCarol}, [2]string{spCarol, ""}), spCarol), nil,
			[]ev{{spCarol, spBob, "account", "", "ended", 2}, {spCarol, spBob, "signer", spAlice, "ended", 1}},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := sponsorshipEvents(ref, c.prev, c.post)
			if len(got) != len(c.want) {
				t.Fatalf("events = %+v, want %+v", got, c.want)
			}
			for i, w := range c.want {
				g := got[i]
				if (ev{g.Sponsor, g.Sponsored, g.Kind, g.KeyID, g.Event, g.Reserves}) != w {
					t.Errorf("event[%d] = %+v, want %+v", i, g, w)
				}
				if g.Ledger != ref.Ledger || g.TxHash != ref.TxHash || g.OpIndex != ref.OpIndex || g.ChangeIndex != ref.ChangeIndex || g.IntraLedgerSeq != ref.IntraLedgerSeq {
					t.Errorf("event[%d] lost its change ref: %+v", i, g)
				}
			}
		})
	}
}
