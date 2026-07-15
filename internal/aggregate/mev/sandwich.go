package mev

import (
	"sort"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// KindSandwich is the mev_events.kind for cross-transaction sandwich
// candidates.
const KindSandwich = "sandwich"

const sandwichNote = "One account's trades in two different transactions bracket at " +
	"least one other account's trade on the same pair within a single ledger " +
	"(tx_index application order from the raw lake). Positional signature only: " +
	"the served rows don't carry trade direction, so front/back opposition is " +
	"not verified and profit is not estimated — treat as a candidate, not proof."

// OrderedLeg is one trade in an ordering-aware pattern's evidence,
// carrying the lake-resolved tx_index that placed it. Amounts are
// decimal strings (ADR-0003).
type OrderedLeg struct {
	Source      string `json:"source"`
	TxHash      string `json:"tx_hash"`
	TxIndex     uint32 `json:"tx_index"`
	OpIndex     uint32 `json:"op_index"`
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
	Pair        string       `json:"pair"`
	Attacker    string       `json:"attacker"`
	Legs        []OrderedLeg `json:"legs"`
	NotionalUSD string       `json:"notional_usd,omitempty"`
	Note        string       `json:"note"`
}

// DetectSandwiches scans a batch of trades for the cross-transaction
// sandwich shape: within one ledger and one (unordered) pair, account
// A trades in two DIFFERENT transactions whose tx_index bracket ≥1
// trade by a different account. txIdx maps tx_hash → tx_index
// (application order within the ledger, resolved from the raw lake);
// trades whose hash is not in the map are ignored — a partial map
// degrades detection, never fabricates order.
//
// usdVolume is the optional parallel USD-notional slice (same
// convention as DetectArbitrage).
func DetectSandwiches(trades []canonical.Trade, usdVolume []string, txIdx map[string]uint32) []Candidate {
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
func sandwichesInGroup(trades []canonical.Trade, usdVolume []string, txIdx map[string]uint32, idxs []int) []Candidate {
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
func buildSandwichCandidate(trades []canonical.Trade, usdVolume []string, txIdx map[string]uint32,
	groupIdxs []int, attacker string, attackerIdxs []int,
) (Candidate, bool) {
	front, back, ok := bracketTrades(trades, txIdx, attackerIdxs)
	if !ok {
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
	notional := sumUSD(usdVolume, involved)
	c := Candidate{
		Kind:             KindSandwich,
		Ledger:           t0.Ledger,
		DetectedAtLedger: t0.Ledger,
		Timestamp:        t0.Timestamp.UTC(),
		TxHash:           t0.TxHash,
		Taker:            attacker,
		TxHashes:         distinctTxHashes(trades, involved),
		Accounts:         distinctAccounts(trades, involved, attacker),
		Assets:           pairAssets(t0),
		Sources:          distinctSources(trades, involved),
		NotionalUSD:      notional,
		Dedup:            KindSandwich + ":" + t0.TxHash + ":" + attacker + ":" + pair,
		Detail: sandwichDetail{
			Pair:        pair,
			Attacker:    attacker,
			Legs:        legs,
			NotionalUSD: notional,
			Note:        sandwichNote,
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

func orderedLegFrom(t canonical.Trade, txIdx map[string]uint32, role string) OrderedLeg {
	return OrderedLeg{
		Source:      t.Source,
		TxHash:      t.TxHash,
		TxIndex:     txIdx[t.TxHash],
		OpIndex:     t.OpIndex,
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
