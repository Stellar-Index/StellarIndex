package entryhistory

import (
	"sort"
	"strconv"

	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/xdrjson"
)

// Sponsorship event names: an arrangement starts, is revoked while its entry lives on, or ends with the entry.
const (
	SponsorshipStarted = "started"
	SponsorshipRevoked = "revoked"
	SponsorshipEnded   = "ended"
)

type sponsorshipSubject struct {
	sponsored, kind, keyID string
	reserves               uint8
}

// sponsorshipEvents derives the sponsorship transitions between an entry's pre- and post-image: the entry's own
// sponsor and, for an account, each signer's. A removal with no pre-image yields nothing: its sponsor is unknowable.
func sponsorshipEvents(ref clickhouse.EntryChangeRef, prev, post *xdr.LedgerEntry) []clickhouse.SponsorshipEvent {
	e := prev
	if e == nil {
		e = post
	}
	if e == nil {
		return nil
	}
	subj, ok := subjectOf(e)
	if !ok {
		return nil
	}
	var out []clickhouse.SponsorshipEvent
	transition := func(before, after string, gone bool, kind, keyID string, reserves uint8) {
		if before == after {
			return
		}
		emit := func(sponsor, event string) {
			out = append(out, clickhouse.SponsorshipEvent{
				Ledger: ref.Ledger, CloseTime: ref.CloseTime, TxHash: ref.TxHash, OpIndex: ref.OpIndex,
				ChangeIndex: ref.ChangeIndex, IntraLedgerSeq: ref.IntraLedgerSeq,
				Sponsor: sponsor, Sponsored: subj.sponsored, Kind: kind, KeyID: keyID, Reserves: reserves, Event: event,
			})
		}
		if before != "" {
			if gone {
				emit(before, SponsorshipEnded)
			} else {
				emit(before, SponsorshipRevoked)
			}
		}
		if after != "" {
			emit(after, SponsorshipStarted)
		}
	}
	transition(sponsorOf(prev), sponsorOf(post), post == nil, subj.kind, subj.keyID, subj.reserves)
	if e.Data.Type != xdr.LedgerEntryTypeAccount {
		return out
	}
	before, after := signerSponsors(prev), signerSponsors(post)
	keys := make([]string, 0, len(before)+len(after))
	for k := range before {
		keys = append(keys, k)
	}
	for k := range after {
		if _, seen := before[k]; !seen {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	for _, k := range keys {
		_, present := after[k]
		transition(before[k], after[k], !present, "signer", k, 1)
	}
	return out
}

// subjectOf names what a sponsorship of e pays for; reserve weights follow CAP-33 and CAP-38.
func subjectOf(e *xdr.LedgerEntry) (sponsorshipSubject, bool) {
	switch e.Data.Type {
	case xdr.LedgerEntryTypeAccount:
		return sponsorshipSubject{sponsored: e.Data.Account.AccountId.Address(), kind: "account", reserves: 2}, true
	case xdr.LedgerEntryTypeTrustline:
		tl := e.Data.TrustLine
		s := sponsorshipSubject{sponsored: tl.AccountId.Address(), kind: "trustline", keyID: xdrjson.TrustLineAssetID(tl.Asset), reserves: 1}
		if tl.Asset.Type == xdr.AssetTypeAssetTypePoolShare {
			s.kind, s.reserves = "pool_share", 2
		}
		return s, true
	case xdr.LedgerEntryTypeOffer:
		o := e.Data.Offer
		return sponsorshipSubject{sponsored: o.SellerId.Address(), kind: "offer", keyID: strconv.FormatInt(int64(o.OfferId), 10), reserves: 1}, true
	case xdr.LedgerEntryTypeData:
		d := e.Data.Data
		return sponsorshipSubject{sponsored: d.AccountId.Address(), kind: "data", keyID: string(d.DataName), reserves: 1}, true
	case xdr.LedgerEntryTypeClaimableBalance:
		// Nobody is sponsored: the sponsor alone carries one reserve per claimant.
		cb := e.Data.ClaimableBalance
		id, _ := xdrjson.ClaimableBalanceIDHex(cb.BalanceId)
		return sponsorshipSubject{kind: "claimable_balance", keyID: id, reserves: uint8(min(len(cb.Claimants), 255))}, true
	default:
		return sponsorshipSubject{}, false
	}
}

func sponsorOf(e *xdr.LedgerEntry) string {
	if e == nil {
		return ""
	}
	if id := e.SponsoringID(); id != nil {
		return id.Address()
	}
	return ""
}

// signerSponsors maps each signer of an account image to its sponsor ("" when unsponsored); nil for no image.
func signerSponsors(e *xdr.LedgerEntry) map[string]string {
	if e == nil || e.Data.Type != xdr.LedgerEntryTypeAccount {
		return nil
	}
	acc := e.Data.Account
	ids := acc.SignerSponsoringIDs()
	m := make(map[string]string, len(acc.Signers))
	for i, s := range acc.Signers {
		sponsor := ""
		if i < len(ids) && ids[i] != nil {
			sponsor = ids[i].Address()
		}
		m[s.Key.Address()] = sponsor
	}
	return m
}
