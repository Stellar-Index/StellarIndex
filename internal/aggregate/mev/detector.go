// Package mev detects on-chain MEV (maximal-extractable-value)
// patterns from the canonical trade stream and writes them to
// mev_events for the explorer's /mev feed.
//
// v1 detects ATOMIC ARBITRAGE only: a single transaction in which one
// taker trades a closed asset cycle (≥2 legs that return to a starting
// asset) across pools/venues. This is the one pattern our served trade
// data supports without guessing — a row carries (ledger, tx_hash,
// op_index, taker) but NOT intra-ledger transaction ordering, so
// cross-transaction sandwich detection (which is fundamentally about
// block position) would be unreliable. A closed cycle inside ONE
// atomic transaction, by contrast, is an unambiguous arbitrage
// signature: the structure itself is the evidence.
//
// The detector is a pure function over a batch of trades; the worker
// (worker.go) supplies recent trades and persists the candidates.
package mev

import (
	"math/big"
	"sort"
	"time"

	"github.com/StellarIndex/stellar-index/internal/canonical"
)

// KindArbitrage is the mev_events.kind value v1 emits.
const KindArbitrage = "arbitrage"

// minLegs is the floor on legs for a cycle — a single trade can't form
// one.
const minLegs = 2

// Leg is one trade in a detected pattern, kept as evidence in the
// stored detail jsonb. Amounts are decimal strings (ADR-0003).
type Leg struct {
	Source      string `json:"source"`
	Base        string `json:"base"`
	Quote       string `json:"quote"`
	BaseAmount  string `json:"base_amount"`
	QuoteAmount string `json:"quote_amount"`
	OpIndex     uint32 `json:"op_index"`
}

// Candidate is one detected MEV event before persistence.
type Candidate struct {
	Kind             string
	Ledger           uint32
	DetectedAtLedger uint32
	Timestamp        time.Time
	TxHash           string
	Taker            string
	Assets           []string // sorted distinct assets in the cycle
	Sources          []string // sorted distinct venues spanned
	Legs             []Leg
	NotionalUSD      string // summed USD volume across legs ("" when none priced)
}

// DedupKey is the deterministic idempotency key persisted to
// mev_events.dedup_key: one detection per (pattern, tx, actor).
func (c Candidate) DedupKey() string {
	return c.Kind + ":" + c.TxHash + ":" + c.Taker
}

// DetectArbitrage scans a batch of trades and returns one Candidate
// per atomic-arbitrage cycle found. Trades are grouped by (tx_hash,
// taker) — a cycle must be a single actor inside a single transaction.
//
// A group is a cycle when its trade graph (assets = nodes, trades =
// edges) has at least as many edges as nodes (a connected graph with
// edges ≥ nodes contains a cycle; a tree has edges = nodes−1). To
// exclude a degenerate same-venue round-trip, a 2-asset cycle must
// span ≥2 distinct venues; ≥3-asset cycles (triangular+) are accepted
// on any venues.
//
// usdVolume[i] is the optional USD notional of trades[i] (parallel
// slice; nil or short → no notional). It's summed across a cycle's
// legs into Candidate.NotionalUSD purely as a size signal — v1 does
// not estimate attacker profit (direction is ambiguous in the served
// rows), so profit_usd stays null downstream.
func DetectArbitrage(trades []canonical.Trade, usdVolume []string) []Candidate {
	type group struct {
		idxs []int
	}
	groups := make(map[string]*group)
	order := make([]string, 0, len(trades))
	for i, t := range trades {
		if t.Ledger == 0 || t.Taker == "" || t.TxHash == "" {
			continue // off-chain / no actor → not an atomic on-chain cycle
		}
		key := t.TxHash + "\x00" + t.Taker
		g, ok := groups[key]
		if !ok {
			g = &group{}
			groups[key] = g
			order = append(order, key)
		}
		g.idxs = append(g.idxs, i)
	}

	var out []Candidate
	for _, key := range order {
		idxs := groups[key].idxs
		if len(idxs) < minLegs {
			continue
		}
		c, ok := buildArbCandidate(trades, usdVolume, idxs)
		if ok {
			out = append(out, c)
		}
	}
	return out
}

// buildArbCandidate evaluates one (tx, taker) group: builds the leg
// set, tests the cycle + venue conditions, and assembles the
// Candidate. ok=false when the group isn't a qualifying cycle.
func buildArbCandidate(trades []canonical.Trade, usdVolume []string, idxs []int) (Candidate, bool) {
	assetSet := map[string]struct{}{}
	sourceSet := map[string]struct{}{}
	legs := make([]Leg, 0, len(idxs))
	for _, i := range idxs {
		t := trades[i]
		base := t.Pair.Base.String()
		quote := t.Pair.Quote.String()
		assetSet[base] = struct{}{}
		assetSet[quote] = struct{}{}
		sourceSet[t.Source] = struct{}{}
		legs = append(legs, Leg{
			Source:      t.Source,
			Base:        base,
			Quote:       quote,
			BaseAmount:  t.BaseAmount.String(),
			QuoteAmount: t.QuoteAmount.String(),
			OpIndex:     t.OpIndex,
		})
	}

	nNodes := len(assetSet)
	nEdges := len(legs)
	// Cycle condition: edges ≥ nodes in the connected leg graph. (The
	// legs share an actor + tx and a cycle of trades is necessarily
	// connected, so a global edges ≥ nodes test is sufficient here.)
	if nEdges < nNodes {
		return Candidate{}, false
	}
	// Reject the degenerate 2-asset single-venue round-trip.
	if nNodes <= 2 && len(sourceSet) < 2 {
		return Candidate{}, false
	}

	// Stable evidence ordering: legs by op_index.
	sort.Slice(legs, func(a, b int) bool { return legs[a].OpIndex < legs[b].OpIndex })

	first := trades[idxs[0]]
	c := Candidate{
		Kind:             KindArbitrage,
		Ledger:           first.Ledger,
		DetectedAtLedger: first.Ledger,
		Timestamp:        first.Timestamp.UTC(),
		TxHash:           first.TxHash,
		Taker:            first.Taker,
		Assets:           sortedKeys(assetSet),
		Sources:          sortedKeys(sourceSet),
		Legs:             legs,
		NotionalUSD:      sumUSD(usdVolume, idxs),
	}
	return c, true
}

func sortedKeys(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// sumUSD adds the parallel USD-volume entries for the group's legs
// with exact decimal arithmetic. Returns "" when no leg carried a USD
// valuation (so the field is omitted downstream rather than reported
// as a misleading 0); otherwise a 2dp decimal string.
func sumUSD(usdVolume []string, idxs []int) string {
	total := new(big.Rat)
	priced := false
	for _, i := range idxs {
		if i >= len(usdVolume) || usdVolume[i] == "" {
			continue
		}
		v, ok := new(big.Rat).SetString(usdVolume[i])
		if !ok {
			continue
		}
		total.Add(total, v)
		priced = true
	}
	if !priced {
		return ""
	}
	return total.FloatString(2)
}
