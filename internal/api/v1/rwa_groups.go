package v1

import "sort"

// rwaUnclassified groups the oracle-basis assets, which declare no anchor class; inventing one
// would be wrong.
const rwaUnclassified = "unclassified"

// rwaUnclassifiedNote travels with the group so the word is not read as a gap in this index's
// data: the classic arm classifies only on what the ISSUERS declared.
const rwaUnclassifiedNote = "Admitted because an independent oracle prices the instrument, not because an issuer declared a class. " +
	"No class is published for these rows because none was declared in the SEP-1 vocabulary — one issuer's own attestation " +
	"says `other`, another declares no anchor type at all. A class here would contradict the first issuer on their own asset " +
	"and invent one for the second, so the group is named rather than filled."

func rwaByClass(assets []RWAAsset, trunc rwaTruncation) []RWAGroupTotal {
	byClass := map[string][]RWAAsset{}
	for _, a := range assets {
		c := a.AnchorClass
		if c == "" {
			c = rwaUnclassified
		}
		byClass[c] = append(byClass[c], a)
	}
	out := make([]RWAGroupTotal, 0, len(byClass))
	for c, group := range byClass {
		total, valued := rwaSumMarketCaps(group)
		refTotal, refValued := rwaSumReferenceValues(group)
		row := RWAGroupTotal{
			Class:                   c,
			Assets:                  len(group),
			MarketCapUSD:            total,
			AssetsUnvalued:          len(group) - valued,
			ReferenceValueUSD:       refTotal,
			AssetsReferenceUnvalued: len(group) - refValued,
		}
		// A member a cap or a failed read left out is in no row, and its class is unknown: every class total may be missing it.
		row.MarketCapLowerBound, row.ReferenceValueLowerBound = rwaGroupLowerBounds(group, valued, refValued, trunc.any())
		if c == rwaUnclassified {
			row.Note = rwaUnclassifiedNote
		}
		out = append(out, row)
	}
	sortRWAGroups(out)
	return out
}

// rwaGroupLowerBounds reports, per basis, whether a group's published total leaves a member out or sums a floor supply.
func rwaGroupLowerBounds(group []RWAAsset, valued, refValued int, membersUnread bool) (marketCap, reference bool) {
	marketCap = valued > 0 && (membersUnread || valued < len(group) ||
		rwaSumsAFloor(group, func(a RWAAsset) *string { return a.Valuation.MarketCapUSD }))
	reference = refValued > 0 && (membersUnread || refValued < len(group) ||
		rwaSumsAFloor(group, func(a RWAAsset) *string { return a.ReferenceValuation.ValueUSD }))
	return marketCap, reference
}

func sortRWAGroups(out []RWAGroupTotal) {
	sort.SliceStable(out, func(i, j int) bool {
		li, lj := ratFromOptionalString(out[i].MarketCapUSD), ratFromOptionalString(out[j].MarketCapUSD)
		switch {
		case li != nil && lj == nil:
			return true
		case li == nil && lj != nil:
			return false
		case li != nil && lj != nil && li.Cmp(lj) != 0:
			return li.Cmp(lj) > 0
		}
		return out[i].Class < out[j].Class
	})
}

func rwaByIssuer(assets []RWAAsset, trunc rwaTruncation) []RWAIssuerTotal {
	order := make([]string, 0, 8)
	byIssuer := map[string][]RWAAsset{}
	for _, a := range assets {
		// A contract row's empty Issuer is not a key, or all contract assets merge into one.
		if a.Issuer == "" {
			continue
		}
		if _, ok := byIssuer[a.Issuer]; !ok {
			order = append(order, a.Issuer)
		}
		byIssuer[a.Issuer] = append(byIssuer[a.Issuer], a)
	}
	out := make([]RWAIssuerTotal, 0, len(order))
	for _, issuer := range order {
		group := byIssuer[issuer]
		total, valued := rwaSumMarketCaps(group)
		refTotal, refValued := rwaSumReferenceValues(group)
		// Only its own full page can hide a member of an issuer that is present: contract rows carry no issuer,
		// and an issuer over the cap has no group at all.
		capFloor, refFloor := rwaGroupLowerBounds(group, valued, refValued, trunc.fullPageIssuers[issuer])
		out = append(out, RWAIssuerTotal{
			Issuer:                   issuer,
			Name:                     group[0].IssuerDirectoryName,
			HomeDomain:               group[0].HomeDomain,
			Assets:                   len(group),
			MarketCapUSD:             total,
			AssetsUnvalued:           len(group) - valued,
			ReferenceValueUSD:        refTotal,
			AssetsReferenceUnvalued:  len(group) - refValued,
			MarketCapLowerBound:      capFloor,
			ReferenceValueLowerBound: refFloor,
		})
	}
	sort.SliceStable(out, func(i, j int) bool {
		li, lj := ratFromOptionalString(out[i].MarketCapUSD), ratFromOptionalString(out[j].MarketCapUSD)
		switch {
		case li != nil && lj == nil:
			return true
		case li == nil && lj != nil:
			return false
		case li != nil && lj != nil && li.Cmp(lj) != 0:
			return li.Cmp(lj) > 0
		}
		return out[i].Issuer < out[j].Issuer
	})
	return out
}
