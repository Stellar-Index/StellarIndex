// Package mev detects on-chain MEV (maximal-extractable-value)
// patterns from the canonical trade stream and writes them to
// mev_events for the explorer's /mev feed.
//
// Detected kinds:
//
//   - arbitrage (detector.go): one taker trades a closed asset cycle
//     inside a single transaction. Purely structural — the served
//     trades rows carry everything needed.
//   - wash_trade (washtrade.go): self-trades (maker == taker) and
//     repeated two-account back-and-forth on one pair. Served rows
//     only.
//   - sandwich (sandwich.go): one account's trades in two different
//     transactions bracket another account's trade on the same pair
//     within one ledger. Needs intra-ledger TRANSACTION ordering,
//     which the served trades table does not carry — the raw lake
//     does (stellar.transactions.tx_index, application order), so
//     this detector runs only when a TxOrderResolver is wired.
//   - oracle_sandwich (oracle_sandwich.go): one account's trades
//     bracket an on-chain oracle update on an asset the trades touch,
//     within one ledger. Same tx_index requirement as sandwich.
//   - liquidation_cascade (cascade.go): Blend liquidation-auction
//     fills against distinct positions clustered within a short
//     ledger window with an on-chain oracle update in the bracket.
//     Served rows only (blend_auctions + oracle_updates).
//
// Every detector is a pure function over batches of served rows (plus
// an optional tx_hash → tx_index map from the lake); the worker
// (worker.go) supplies the inputs and persists candidates. Detection
// is positional/structural evidence, not proof of intent — the served
// rows don't carry trade direction, so direction-dependent claims
// (front-run vs back-run) are never asserted. See each detail Note.
package mev

import (
	"math/big"
	"sort"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
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
	TxHash           string   // primary transaction (the dedup anchor)
	Taker            string   // primary actor
	TxHashes         []string // all involved txs; nil → [TxHash]
	Accounts         []string // all involved accounts; nil → [Taker]
	Assets           []string // sorted distinct assets in the cycle
	Sources          []string // sorted distinct venues spanned
	Legs             []Leg
	NotionalUSD      string // summed USD volume across legs ("" when none priced)
	Dedup            string // explicit dedup key; "" → kind:tx:taker
	Detail           any    // per-kind evidence payload (marshalled to mev_events.detail); nil → arbDetail
}

// DedupKey is the deterministic idempotency key persisted to
// mev_events.dedup_key. Default shape: one detection per (pattern,
// tx, actor); detectors whose identity needs more dimensions set
// Candidate.Dedup explicitly.
func (c Candidate) DedupKey() string {
	if c.Dedup != "" {
		return c.Dedup
	}
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
