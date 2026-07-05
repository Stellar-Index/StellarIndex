package mev

import (
	"sort"

	"github.com/StellarIndex/stellar-index/internal/canonical"
)

// KindWashTrade is the mev_events.kind for wash-trading candidates.
const KindWashTrade = "wash_trade"

const (
	washSelfNote = "The same account is BOTH maker and taker of the trade — a " +
		"self-cross. Volume changed hands from the account to itself; the " +
		"observation inflates apparent activity without transferring value."
	washRoundTripNote = "Two accounts repeatedly took each other's offers on the same " +
		"pair in both directions within one UTC day (≥2 fills each direction in the " +
		"scan window). Back-and-forth of this shape is the classic wash signature, " +
		"but it is also what tight two-party market-making looks like — treat as a " +
		"candidate, not proof."

	// roundTripMinPerDirection is the per-direction fill floor for the
	// two-account round-trip variant: one crossing each way happens in
	// ordinary trading; repeated back-and-forth is the signal.
	roundTripMinPerDirection = 2
)

// washLeg is one trade in a wash-trading candidate's evidence.
type washLeg struct {
	Source      string `json:"source"`
	Ledger      uint32 `json:"ledger"`
	TxHash      string `json:"tx_hash"`
	OpIndex     uint32 `json:"op_index"`
	Maker       string `json:"maker"`
	Taker       string `json:"taker"`
	Base        string `json:"base"`
	Quote       string `json:"quote"`
	BaseAmount  string `json:"base_amount"`
	QuoteAmount string `json:"quote_amount"`
}

// washDetail is the mev_events.detail payload for a wash_trade
// candidate.
type washDetail struct {
	Variant     string    `json:"variant"` // "self_trade" | "round_trip"
	Pair        string    `json:"pair"`
	Accounts    []string  `json:"accounts"`
	Legs        []washLeg `json:"legs"`
	NotionalUSD string    `json:"notional_usd,omitempty"`
	Note        string    `json:"note"`
}

// DetectWashTrades scans a batch of trades for two wash signatures,
// both answerable from the served rows alone:
//
//   - self_trade: maker == taker on a single trade (an account
//     crossing its own offer). Unambiguous; one candidate per
//     (tx, account).
//   - round_trip: two accounts fill each other's offers on the same
//     (unordered) pair in BOTH directions, ≥2 fills per direction,
//     bucketed by UTC day so re-scans of overlapping windows dedup
//     deterministically. One candidate per (pair, account-pair, day).
//
// Both variants only fire where maker is populated — i.e. SDEX rows;
// AMM rows carry the pool as maker and can't self-cross this way.
func DetectWashTrades(trades []canonical.Trade, usdVolume []string) []Candidate {
	out := detectSelfTrades(trades, usdVolume)
	out = append(out, detectRoundTrips(trades, usdVolume)...)
	return out
}

func detectSelfTrades(trades []canonical.Trade, usdVolume []string) []Candidate {
	groups := map[string][]int{}
	order := []string{}
	for i, t := range trades {
		if !orderableTrade(t) || t.Maker == "" || t.Maker != t.Taker {
			continue
		}
		key := t.TxHash + "\x00" + t.Taker
		if _, seen := groups[key]; !seen {
			order = append(order, key)
		}
		groups[key] = append(groups[key], i)
	}

	var out []Candidate
	for _, key := range order {
		idxs := groups[key]
		t0 := trades[idxs[0]]
		notional := sumUSD(usdVolume, idxs)
		out = append(out, Candidate{
			Kind:             KindWashTrade,
			Ledger:           t0.Ledger,
			DetectedAtLedger: t0.Ledger,
			Timestamp:        t0.Timestamp.UTC(),
			TxHash:           t0.TxHash,
			Taker:            t0.Taker,
			Assets:           pairAssets(t0),
			Sources:          distinctSources(trades, idxs),
			NotionalUSD:      notional,
			// Default dedup (kind:tx:actor) is exactly the self-trade
			// identity — no explicit Dedup needed.
			Detail: washDetail{
				Variant:     "self_trade",
				Pair:        unorderedPairKey(t0),
				Accounts:    []string{t0.Taker},
				Legs:        washLegs(trades, idxs),
				NotionalUSD: notional,
				Note:        washSelfNote,
			},
		})
	}
	return out
}

func detectRoundTrips(trades []canonical.Trade, usdVolume []string) []Candidate {
	groups := map[string][]int{}
	order := []string{}
	for i, t := range trades {
		if !orderableTrade(t) || t.Maker == "" || t.Maker == t.Taker {
			continue
		}
		a, b := t.Maker, t.Taker
		if a > b {
			a, b = b, a
		}
		day := t.Timestamp.UTC().Format("2006-01-02")
		key := day + ":" + unorderedPairKey(t) + ":" + a + "|" + b
		if _, seen := groups[key]; !seen {
			order = append(order, key)
		}
		groups[key] = append(groups[key], i)
	}

	var out []Candidate
	for _, key := range order {
		c, ok := buildRoundTripCandidate(trades, usdVolume, key, groups[key])
		if ok {
			out = append(out, c)
		}
	}
	return out
}

// buildRoundTripCandidate tests one (day, pair, account-pair) bucket
// for the ≥2-fills-each-direction shape.
func buildRoundTripCandidate(trades []canonical.Trade, usdVolume []string, key string, idxs []int) (Candidate, bool) {
	// Direction = who took. The bucket holds exactly two accounts.
	perTaker := map[string]int{}
	for _, i := range idxs {
		perTaker[trades[i].Taker]++
	}
	if len(perTaker) != 2 {
		return Candidate{}, false
	}
	for _, n := range perTaker {
		if n < roundTripMinPerDirection {
			return Candidate{}, false
		}
	}

	sort.Slice(idxs, func(a, b int) bool {
		ta, tb := trades[idxs[a]], trades[idxs[b]]
		if ta.Ledger != tb.Ledger {
			return ta.Ledger < tb.Ledger
		}
		if ta.TxHash != tb.TxHash {
			return ta.TxHash < tb.TxHash
		}
		return ta.OpIndex < tb.OpIndex
	})

	t0 := trades[idxs[0]]
	accounts := sortedKeys(map[string]struct{}{t0.Maker: {}, t0.Taker: {}})
	notional := sumUSD(usdVolume, idxs)
	c := Candidate{
		Kind:             KindWashTrade,
		Ledger:           t0.Ledger,
		DetectedAtLedger: t0.Ledger,
		Timestamp:        t0.Timestamp.UTC(),
		TxHash:           t0.TxHash,
		Taker:            accounts[0],
		TxHashes:         distinctTxHashes(trades, idxs),
		Accounts:         accounts,
		Assets:           pairAssets(t0),
		Sources:          distinctSources(trades, idxs),
		NotionalUSD:      notional,
		// The bucket key (day + pair + account pair) IS the identity:
		// a sliding scan window re-detects the same bucket without
		// duplicating it.
		Dedup: KindWashTrade + ":rt:" + key,
		Detail: washDetail{
			Variant:     "round_trip",
			Pair:        unorderedPairKey(t0),
			Accounts:    accounts,
			Legs:        washLegs(trades, idxs),
			NotionalUSD: notional,
			Note:        washRoundTripNote,
		},
	}
	return c, true
}

// washLegs renders evidence legs, capped so a high-frequency bucket
// doesn't bloat the detail jsonb.
func washLegs(trades []canonical.Trade, idxs []int) []washLeg {
	const maxLegs = 20
	if len(idxs) > maxLegs {
		idxs = idxs[:maxLegs]
	}
	legs := make([]washLeg, 0, len(idxs))
	for _, i := range idxs {
		t := trades[i]
		legs = append(legs, washLeg{
			Source:      t.Source,
			Ledger:      t.Ledger,
			TxHash:      t.TxHash,
			OpIndex:     t.OpIndex,
			Maker:       t.Maker,
			Taker:       t.Taker,
			Base:        t.Pair.Base.String(),
			Quote:       t.Pair.Quote.String(),
			BaseAmount:  t.BaseAmount.String(),
			QuoteAmount: t.QuoteAmount.String(),
		})
	}
	return legs
}
