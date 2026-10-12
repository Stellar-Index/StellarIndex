package mev

import (
	"sort"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/domain"
)

// KindSandwich is the mev_events.kind for cross-transaction sandwich candidates.
const KindSandwich = "sandwich"

// trades.base_asset IS the direction (amounts are both positive) under a per-source convention
// (takerBaseIsReceived). A sandwich needs OPPOSITE directions on the same pair; same-direction and
// unknown brackets are dropped because this is a publicly served accusation.
const sandwichNote = "One account's trades in two different transactions bracket at " +
	"least one other account's trade on the same pair within a single ledger " +
	"(tx_index application order from the raw lake), and the account's front and " +
	"back trades run in OPPOSITE directions on that pair — the structural signature " +
	"of a sandwich. Direction is read from the underlying rows' base asset under a " +
	"per-source convention; same-direction brackets are rejected. Profit is not " +
	"estimated, so treat as an unverified candidate, not proof, and not an accusation."

// OrderedLeg is one trade in an ordering-aware pattern's evidence with its lake tx_index; amounts are decimal strings (ADR-0003).
type OrderedLeg struct {
	Source  string `json:"source"`
	TxHash  string `json:"tx_hash"`
	TxIndex uint32 `json:"tx_index"`
	OpRef
	Account     string `json:"account,omitempty"`
	Base        string `json:"base"`
	Quote       string `json:"quote"`
	BaseAmount  string `json:"base_amount"`
	QuoteAmount string `json:"quote_amount"`
	Role        string `json:"role"` // "bracket" | "victim" | "before" | "after"
}

// sandwichDetail is the mev_events.detail payload for a sandwich
// candidate.
type sandwichDetail struct {
	Pair               string       `json:"pair"`
	Attacker           string       `json:"attacker"`
	Legs               []OrderedLeg `json:"legs"`
	NotionalUSD        string       `json:"notional_usd,omitempty"`
	NotionalLowerBound bool         `json:"notional_usd_lower_bound,omitempty"`
	NotionalEstimated  bool         `json:"notional_usd_estimated,omitempty"`
	Note               string       `json:"note"`
}

// DetectSandwiches finds, in one ledger and unordered pair, an account trading in two DIFFERENT txs whose tx_index brackets another account's trade; trades missing from txIdx are ignored, so a partial map degrades detection, never fabricates order. usdVolume is as in DetectArbitrage.
func DetectSandwiches(trades []canonical.Trade, usdVolume []domain.MEVLegUSD, txIdx map[string]uint32) []Candidate {
	if len(txIdx) == 0 {
		return nil
	}
	groups := map[string][]int{}
	order := []string{}
	for i, t := range trades {
		if !orderableTrade(t) {
			continue
		}
		if _, ok := txIdx[t.TxHash]; !ok {
			continue
		}
		key := ledgerKey(t.Ledger) + unorderedPairKey(t)
		if _, seen := groups[key]; !seen {
			order = append(order, key)
		}
		groups[key] = append(groups[key], i)
	}

	var out []Candidate
	for _, key := range order {
		out = append(out, sandwichesInGroup(trades, usdVolume, txIdx, groups[key])...)
	}
	return out
}

// sandwichesInGroup evaluates one (ledger, pair) trade group,
// emitting one candidate per bracketing account.
func sandwichesInGroup(trades []canonical.Trade, usdVolume []domain.MEVLegUSD, txIdx map[string]uint32, idxs []int) []Candidate {
	byTaker := map[string][]int{}
	takerOrder := []string{}
	for _, i := range idxs {
		taker := trades[i].Taker
		if _, seen := byTaker[taker]; !seen {
			takerOrder = append(takerOrder, taker)
		}
		byTaker[taker] = append(byTaker[taker], i)
	}
	if len(byTaker) < 2 {
		return nil
	}

	var out []Candidate
	for _, attacker := range takerOrder {
		c, ok := buildSandwichCandidate(trades, usdVolume, txIdx, idxs, attacker, byTaker[attacker])
		if ok {
			out = append(out, c)
		}
	}
	return out
}

// buildSandwichCandidate tests whether `attacker` brackets a victim
// inside the group and assembles the Candidate.
func buildSandwichCandidate(trades []canonical.Trade, usdVolume []domain.MEVLegUSD, txIdx map[string]uint32,
	groupIdxs []int, attacker string, attackerIdxs []int,
) (Candidate, bool) {
	front, back, ok := bracketTrades(trades, txIdx, attackerIdxs)
	if !ok {
		return Candidate{}, false
	}
	// A real sandwich front-runs then back-runs the SAME pair in OPPOSITE
	// directions; the positional bracket alone admits same-direction
	// ones. Drop same-direction and direction-unknown brackets so no
	// account is named on impossible evidence.
	if !oppositeDirection(trades[front], trades[back]) {
		return Candidate{}, false
	}
	frontIdx := txIdx[trades[front].TxHash]
	backIdx := txIdx[trades[back].TxHash]

	var victims []int
	for _, i := range groupIdxs {
		if trades[i].Taker == attacker {
			continue
		}
		vi := txIdx[trades[i].TxHash]
		if vi > frontIdx && vi < backIdx {
			victims = append(victims, i)
		}
	}
	if len(victims) == 0 {
		return Candidate{}, false
	}

	involved := append([]int{front}, victims...)
	involved = append(involved, back)
	legs := make([]OrderedLeg, 0, len(involved))
	for n, i := range involved {
		role := "victim"
		if n == 0 || n == len(involved)-1 {
			role = "bracket"
		}
		legs = append(legs, orderedLegFrom(trades[i], txIdx, role))
	}

	t0 := trades[front]
	pair := unorderedPairKey(t0)
	assetID, quoteID := pairIDs(t0)
	notional, lowerBound, estimated := sumUSD(usdVolume, involved)
	c := Candidate{
		AssetID:            assetID,
		QuoteID:            quoteID,
		Kind:               KindSandwich,
		Ledger:             t0.Ledger,
		DetectedAtLedger:   t0.Ledger,
		Timestamp:          t0.Timestamp.UTC(),
		TxHash:             t0.TxHash,
		Taker:              attacker,
		TxHashes:           distinctTxHashes(trades, involved),
		Accounts:           distinctAccounts(trades, involved, attacker),
		Assets:             pairAssets(t0),
		Sources:            distinctSources(trades, involved),
		NotionalUSD:        notional,
		NotionalLowerBound: lowerBound,
		NotionalEstimated:  estimated,
		Dedup:              KindSandwich + ":" + t0.TxHash + ":" + attacker + ":" + pair,
		Detail: sandwichDetail{
			Pair:               pair,
			Attacker:           attacker,
			Legs:               legs,
			NotionalUSD:        notional,
			NotionalLowerBound: lowerBound,
			NotionalEstimated:  estimated,
			Note:               sandwichNote,
		},
	}
	return c, true
}

// bracketTrades picks the attacker's outermost (min, max tx_index)
// trades, requiring them to sit in two different transactions.
func bracketTrades(trades []canonical.Trade, txIdx map[string]uint32, idxs []int) (front, back int, ok bool) {
	front, back = idxs[0], idxs[0]
	for _, i := range idxs[1:] {
		if txIdx[trades[i].TxHash] < txIdx[trades[front].TxHash] {
			front = i
		}
		if txIdx[trades[i].TxHash] > txIdx[trades[back].TxHash] {
			back = i
		}
	}
	if trades[front].TxHash == trades[back].TxHash {
		return 0, 0, false // one atomic tx → arbitrage territory, not a sandwich
	}
	return front, back, true
}

// oppositeDirection reports whether front and back sit on OPPOSITE sides of the pair. If either direction is
// indeterminate it returns false: we never assert an opposition we cannot prove.
func oppositeDirection(front, back canonical.Trade) bool {
	fl, fok := takerReceivesLowAsset(front)
	bl, bok := takerReceivesLowAsset(back)
	return fok && bok && fl != bl
}

// oppositeOnAsset is oppositeDirection keyed on one asset rather than a
// shared pair: the two trades may be on different pairs that both touch
// asset. False when either direction is indeterminate.
func oppositeOnAsset(a, b canonical.Trade, asset string) bool {
	ar, aok := takerReceivesAsset(a, asset)
	br, bok := takerReceivesAsset(b, asset)
	return aok && bok && ar != br
}

// takerReceivesAsset reports whether the taker RECEIVED asset (compared
// normalised) in t. ok is false when the source's convention is unknown
// or t does not touch asset.
func takerReceivesAsset(t canonical.Trade, asset string) (received, ok bool) {
	baseReceived, known := takerBaseIsReceived(t.Source)
	if !known {
		return false, false
	}
	switch normAsset(asset) {
	case normAsset(t.Pair.Base.String()):
		return baseReceived, true
	case normAsset(t.Pair.Quote.String()):
		return !baseReceived, true
	default:
		return false, false
	}
}

// takerReceivesLowAsset reports whether the taker RECEIVED the pair's orientation-independent LOW asset
// (as unorderedPairKey sorts); ok is false when the source's convention is unknown.
func takerReceivesLowAsset(t canonical.Trade) (low, ok bool) {
	received, ok := takerBaseIsReceived(t.Source)
	if !ok {
		return false, false
	}
	baseIsLow := normAsset(t.Pair.Base.String()) <= normAsset(t.Pair.Quote.String())
	// taker received the low asset iff it received the base AND base is the
	// low, or it received the quote AND base is the high.
	return received == baseIsLow, true
}

// takerBaseIsReceived reports whether a source's Base asset is what the TAKER received. The convention is INVERTED
// between sdex (base = maker sold = taker received) and the Soroban AMMs (base = token_in = taker sold; sushiswap_v3 is the
// positive pool delta). ok is false for any other source: a missed candidate beats a named innocent.
func takerBaseIsReceived(source string) (received, ok bool) {
	switch source {
	case "sdex":
		return true, true
	case "aquarius", "comet", "phoenix", "soroswap", "sushiswap_v3":
		return false, true
	default:
		return false, false
	}
}

func orderedLegFrom(t canonical.Trade, txIdx map[string]uint32, role string) OrderedLeg {
	return OrderedLeg{
		Source:      t.Source,
		TxHash:      t.TxHash,
		TxIndex:     txIdx[t.TxHash],
		OpRef:       opRefOf(t),
		Account:     t.Taker,
		Base:        t.Pair.Base.String(),
		Quote:       t.Pair.Quote.String(),
		BaseAmount:  t.BaseAmount.String(),
		QuoteAmount: t.QuoteAmount.String(),
		Role:        role,
	}
}

// distinctTxHashes returns the involved trades' distinct tx hashes in
// first-seen order.
func distinctTxHashes(trades []canonical.Trade, idxs []int) []string {
	seen := map[string]struct{}{}
	out := []string{}
	for _, i := range idxs {
		h := trades[i].TxHash
		if _, ok := seen[h]; ok {
			continue
		}
		seen[h] = struct{}{}
		out = append(out, h)
	}
	return out
}

// distinctAccounts returns primary first, then the other involved
// takers in first-seen order.
func distinctAccounts(trades []canonical.Trade, idxs []int, primary string) []string {
	seen := map[string]struct{}{primary: {}}
	out := []string{primary}
	for _, i := range idxs {
		a := trades[i].Taker
		if a == "" {
			continue
		}
		if _, ok := seen[a]; ok {
			continue
		}
		seen[a] = struct{}{}
		out = append(out, a)
	}
	return out
}

func distinctSources(trades []canonical.Trade, idxs []int) []string {
	set := map[string]struct{}{}
	for _, i := range idxs {
		set[trades[i].Source] = struct{}{}
	}
	return sortedKeys(set)
}

func pairAssets(t canonical.Trade) []string {
	assets := []string{t.Pair.Base.String(), t.Pair.Quote.String()}
	sort.Strings(assets)
	return assets
}
