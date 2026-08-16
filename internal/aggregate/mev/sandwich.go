package mev

import (
	"sort"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// KindSandwich is the mev_events.kind for cross-transaction sandwich
// candidates.
const KindSandwich = "sandwich"

// Direction opposition (cold audit 2026-08-04): trades.base_asset IS the
// trade direction — the amounts are both positive, so direction lives in
// which asset the source places in Base, under a per-source convention
// (see takerBaseIsReceived). A genuine sandwich front-runs then back-runs
// the SAME pair in OPPOSITE directions; buildSandwichCandidate now
// enforces that. Before the check, ~196/200 published candidates had both
// bracket legs in the SAME direction and could not be sandwiches — this
// feeds a publicly-served accusation, so same-direction (and
// direction-unknown) brackets are dropped rather than naming an account.
const sandwichNote = "One account's trades in two different transactions bracket at " +
	"least one other account's trade on the same pair within a single ledger " +
	"(tx_index application order from the raw lake), and the account's front and " +
	"back trades run in OPPOSITE directions on that pair — the structural signature " +
	"of a sandwich. Direction is read from the underlying rows' base asset under a " +
	"per-source convention; same-direction brackets are rejected. Profit is not " +
	"estimated, so treat as an unverified candidate, not proof, and not an accusation."

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
	// A real sandwich front-runs then back-runs the SAME pair in OPPOSITE
	// directions. The positional bracket ALONE flagged ~196/200 published
	// candidates that were same-direction and thus structurally impossible
	// (cold audit 2026-08-04). Drop same-direction — and
	// direction-unknown — brackets so no account is named on impossible
	// evidence.
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

// oppositeDirection reports whether the front and back trades sit on
// OPPOSITE sides of the same pair — the defining shape of a sandwich
// (front-run one way, back-run the other). If the direction of EITHER
// leg is indeterminate (a source whose base convention we don't know),
// it returns false: this feeds a publicly-served accusation, so we never
// assert an opposition we cannot prove.
func oppositeDirection(front, back canonical.Trade) bool {
	fl, fok := takerReceivesLowAsset(front)
	bl, bok := takerReceivesLowAsset(back)
	return fok && bok && fl != bl
}

// takerReceivesLowAsset reports whether the taker RECEIVED the
// orientation-independent LOW asset of the trade's pair (the same "low"
// unorderedPairKey sorts on), collapsing the per-source base convention
// into one comparable direction. ok is false when the source's
// convention is unknown.
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

// takerBaseIsReceived reports, per source, whether a trade's raw Base
// asset is the one the TAKER received (bought). Direction is not a sign
// on the amounts (both are positive) — it lives in which asset the
// source places in Base, and that convention is INVERTED between the
// classic orderbook and the Soroban AMMs:
//
//   - sdex writes base = the asset the maker SOLD, i.e. what the taker
//     RECEIVED (internal/sources/sdex/decode.go).
//   - the Soroban AMMs (aquarius, comet, phoenix, soroswap) write
//     base = token_in = what the taker SOLD, so the taker received the
//     QUOTE (each source's decode.go).
//
// These are the only on-chain sources that emit taker-attributed,
// ledger>0 trades (off-chain venues stamp ledger 0 and are excluded by
// orderableTrade).
// ok is false for any other source: an unknown convention cannot yield a
// proven direction, and dropping the candidate is the only safe move for
// accusation data — better a missed candidate than a named innocent.
func takerBaseIsReceived(source string) (received, ok bool) {
	switch source {
	case "sdex":
		return true, true
	case "aquarius", "comet", "phoenix", "soroswap":
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
