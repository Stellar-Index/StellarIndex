// Package mev detects MEV patterns (arbitrage, wash_trade, sandwich, oracle_sandwich,
// liquidation_cascade) over served trade batches and writes them to mev_events. Detection is
// positional evidence, not proof of intent; sandwich kinds need the lake tx_index (TxOrderResolver).
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

// Leg is one trade in a detected pattern, kept as evidence; amounts are decimal strings (ADR-0003).
type Leg struct {
	Source      string `json:"source"`
	Base        string `json:"base"`
	Quote       string `json:"quote"`
	BaseAmount  string `json:"base_amount"`
	QuoteAmount string `json:"quote_amount"`
	OpRef
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
	Assets           []string // sorted distinct assets in the pattern
	Sources          []string // sorted distinct venues spanned
	AssetID          string   // primary asset → mev_events.asset_id; "" for cross-asset kinds
	QuoteID          string   // primary quote → mev_events.quote_id; "" for cross-asset kinds
	Legs             []Leg
	NotionalUSD      string // summed USD volume across legs ("" when none priced)
	// NotionalLowerBound: NotionalUSD left out at least one unpriced leg.
	NotionalLowerBound bool
	Dedup              string // explicit dedup key; "" → kind:tx:taker
	Detail             any    // per-kind evidence payload (marshalled to mev_events.detail); nil → arbDetail
}

// DedupKey is the idempotency key for mev_events.dedup_key: (pattern, tx, actor) unless Candidate.Dedup is set.
func (c Candidate) DedupKey() string {
	if c.Dedup != "" {
		return c.Dedup
	}
	return c.Kind + ":" + c.TxHash + ":" + c.Taker
}

// DetectArbitrage returns one Candidate per atomic-arbitrage cycle grouped by (tx_hash, taker); a component is a cycle when its hop graph (distinct (venue, unordered pair) edges) has edges >= nodes, with trades collapsed to hops first since SDEX emits one trade per claim atom. usdVolume is an optional parallel size signal.
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

// buildArbCandidate evaluates one (tx, taker) group: splits it into
// asset-connected components, keeps those that are qualifying cycles,
// and assembles the Candidate from their legs. ok=false when none is.
func buildArbCandidate(trades []canonical.Trade, usdVolume []string, idxs []int) (Candidate, bool) {
	// Judged per component so an unrelated leg cannot lend a cycle its
	// missing second venue or spare hop.
	var kept []int
	for _, comp := range assetComponents(trades, idxs) {
		if isArbCycle(trades, comp) {
			kept = append(kept, comp...)
		}
	}
	if len(kept) == 0 {
		return Candidate{}, false
	}
	sort.Ints(kept)

	assetSet := map[string]struct{}{}
	sourceSet := map[string]struct{}{}
	legs := make([]Leg, 0, len(kept))
	for _, i := range kept {
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
			OpRef:       opRefOf(t),
		})
	}

	// Stable evidence ordering: legs in on-chain (operation, position)
	// order — the packed keys of different sources do not compare.
	sort.SliceStable(legs, func(a, b int) bool { return legs[a].before(legs[b].OpRef) })

	first := trades[kept[0]]
	notional, lowerBound := sumUSD(usdVolume, kept)
	c := Candidate{
		Kind:               KindArbitrage,
		Ledger:             first.Ledger,
		DetectedAtLedger:   first.Ledger,
		Timestamp:          first.Timestamp.UTC(),
		TxHash:             first.TxHash,
		Taker:              first.Taker,
		Assets:             sortedKeys(assetSet),
		Sources:            sortedKeys(sourceSet),
		Legs:               legs,
		NotionalUSD:        notional,
		NotionalLowerBound: lowerBound,
	}
	return c, true
}

// isArbCycle tests one connected component for the cycle + venue
// conditions.
func isArbCycle(trades []canonical.Trade, idxs []int) bool {
	assetSet := map[string]struct{}{}
	sourceSet := map[string]struct{}{}
	hopSet := map[string]struct{}{}
	for _, i := range idxs {
		t := trades[i]
		base := t.Pair.Base.String()
		quote := t.Pair.Quote.String()
		assetSet[base] = struct{}{}
		assetSet[quote] = struct{}{}
		sourceSet[t.Source] = struct{}{}
		hopSet[hopKey(t.Source, base, quote)] = struct{}{}
	}
	nNodes := len(assetSet)
	// Cycle condition on the hop graph, not the trade multigraph:
	// parallel claim-atom trades of one hop are not extra edges.
	if len(hopSet) < nNodes {
		return false
	}
	// Conservative single-venue rejection, kept on the raw leg count: a
	// 2-asset round-trip, or a single-venue component with more legs
	// than assets, is not published as arbitrage.
	return len(sourceSet) >= 2 || (nNodes > 2 && len(idxs) <= nNodes)
}

// assetComponents partitions a group's trade indices by the connected
// components of their asset graph, each in ascending index order.
func assetComponents(trades []canonical.Trade, idxs []int) [][]int {
	parent := map[string]string{}
	var find func(string) string
	find = func(a string) string {
		p, ok := parent[a]
		if !ok || p == a {
			parent[a] = a
			return a
		}
		r := find(p)
		parent[a] = r
		return r
	}
	for _, i := range idxs {
		p := trades[i].Pair
		parent[find(p.Base.String())] = find(p.Quote.String())
	}
	byRoot := map[string]int{}
	var out [][]int
	for _, i := range idxs {
		r := find(trades[i].Pair.Base.String())
		k, ok := byRoot[r]
		if !ok {
			k = len(out)
			byRoot[r] = k
			out = append(out, nil)
		}
		out[k] = append(out[k], i)
	}
	return out
}

// hopKey names one hop edge: a venue plus an unordered asset pair, so
// fills in either direction on the same venue share the edge.
func hopKey(source, a, b string) string {
	if b < a {
		a, b = b, a
	}
	return source + "\x00" + a + "\x00" + b
}

func sortedKeys(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// sumUSD adds the legs' USD volumes with exact decimal arithmetic. It returns "" when no leg was priced
// (never a misleading 0); lowerBound is true when a non-empty sum left out an unpriced leg.
func sumUSD(usdVolume []string, idxs []int) (notional string, lowerBound bool) {
	total := new(big.Rat)
	priced, unpriced := false, false
	for _, i := range idxs {
		if i >= len(usdVolume) || usdVolume[i] == "" {
			unpriced = true
			continue
		}
		v, ok := new(big.Rat).SetString(usdVolume[i])
		if !ok {
			unpriced = true
			continue
		}
		total.Add(total, v)
		priced = true
	}
	if !priced {
		return "", false
	}
	return total.FloatString(2), unpriced
}
