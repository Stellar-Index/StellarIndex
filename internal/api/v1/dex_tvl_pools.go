package v1

import (
	"context"
	"math/big"
	"net/http"
	"sort"
)

// Per-leg exclusion reasons on the DEX TVL drill-down (#338). A reserve
// leg that carries one of these contributed EXACTLY 0 to its pool and
// marked the pool unpriced; the reason says which rule in
// docs/methodology/dex-tvl.md did it. The set is closed and each value
// is a distinct cause, so a consumer can tell "the platform refuses to
// price this" (withheld) from "nobody prices this" (no_served_price)
// from "the token is unknown" (unresolved_token / malformed_token).
const (
	// DEXTVLLegWithheld — the serving trust gates withhold a USD
	// valuation of this asset (directory-flagged issuer, or a market
	// below the substance floor). /v1/assets/{asset} serves
	// price_usd: null for it, and so does this leg.
	DEXTVLLegWithheld = "withheld"
	// DEXTVLLegNoServedPrice — no USD price is served for this asset
	// through the price tiers (declared peg → direct VWAP → XLM bridge).
	DEXTVLLegNoServedPrice = "no_served_price"
	// DEXTVLLegUnresolvedToken — the pool reports a position but the
	// token address for it is unknown (Aquarius update_reserves carries
	// positions, not addresses; migration 0089).
	DEXTVLLegUnresolvedToken = "unresolved_token"
	// DEXTVLLegMalformedToken — the captured token identity is not a
	// well-formed contract id, so there is no asset to price.
	DEXTVLLegMalformedToken = "malformed_token"
	// DEXTVLLegInvalidReserve — the captured reserve is missing or
	// negative, which no honest valuation can be built on.
	DEXTVLLegInvalidReserve = "invalid_reserve"
	// DEXTVLPoolUndecodable is the POOL-level exclusion: the pool's
	// captured storage did not decode to the recognised shape, so its
	// legs are unknown. It is counted (pools_total + unpriced_pools),
	// published with no legs and tvl_usd "0.00", never guessed.
	DEXTVLPoolUndecodable = "undecodable_storage"
)

// Per-leg valuation bases: HOW a valued leg's usd figure was arrived
// at. Exactly one of basis/usd or excluded is present on a leg.
const (
	// DEXTVLBasisDeclaredUSDPeg — $1 per whole unit at the token's
	// declared decimals, because the operator declared the asset a
	// USD peg ([supply].usd_pegs). Applied only AFTER the trust gates.
	DEXTVLBasisDeclaredUSDPeg = "declared_usd_peg"
	// DEXTVLBasisServedUSDPrice — reserve × the same served USD rate
	// /v1/assets/{asset} publishes (the trades.usd_volume tiers).
	DEXTVLBasisServedUSDPrice = "served_usd_price"
	// DEXTVLBasisEmptyReserve — the reserve is zero, so the leg is
	// worth exactly $0 regardless of price; no price was consulted.
	DEXTVLBasisEmptyReserve = "empty_reserve"
)

// DEXTVLLegView is one reserve leg of one pool on the drill-down: the
// reserve as captured, the identity it was priced under, and either its
// valuation or the reason it has none.
type DEXTVLLegView struct {
	// Token is the token contract id exactly as the pool's storage
	// carries it (C-strkey). Omitted when the position's token address
	// never resolved (excluded = unresolved_token).
	Token string `json:"token,omitempty"`
	// Reserve is the captured reserve in the token's base units (i128
	// decimal string — ADR-0003). Omitted when nothing was captured
	// (excluded = invalid_reserve).
	Reserve string `json:"reserve,omitempty"`
	// Asset is the canonical identity the served price path values this
	// leg under — the same id /v1/assets/{id} answers for. A configured
	// classic↔SAC wrapper collapses to its classic twin here, exactly as
	// the trust gates were asked about it. Omitted when the token could
	// not be resolved to an asset at all.
	Asset string `json:"asset,omitempty"`
	// Basis is how USD was derived (see the DEXTVLBasis* constants).
	// Present iff the leg was valued.
	Basis string `json:"basis,omitempty"`
	// USD is the leg's value to the cent (decimal string, exactly two
	// places). Present iff the leg was valued; a valued leg worth less
	// than half a cent reads "0.00".
	USD string `json:"usd,omitempty"`
	// Excluded is why the leg contributed nothing (see the DEXTVLLeg*
	// constants). Present iff the leg was NOT valued.
	Excluded string `json:"excluded,omitempty"`
}

// DEXTVLPoolView is one pool's contribution to its protocol's tvl_usd.
// Its tvl_usd is the EXACT sum of its legs' published usd strings, and
// the protocol's tvl_usd is the exact sum of its pools' — so a reader
// can add the rows at any level and land on the level above
// byte-for-byte, the same contract tvl_total keeps over the protocols.
type DEXTVLPoolView struct {
	// Pool is the pool (pair) contract C-strkey.
	Pool string `json:"pool"`
	// TVLUSD is the exact sum of the legs' usd strings; "0.00" when no
	// leg was valued.
	TVLUSD string `json:"tvl_usd"`
	// Priced is true when EVERY leg was valued — the pool counts in
	// pools_priced; otherwise it counts in unpriced_pools and TVLUSD is
	// a lower bound on the pool.
	Priced bool `json:"priced"`
	// AsOfLedger is the ledger at which the pool's reserves last
	// changed (current as of it and unchanged since). Omitted when the
	// reader carried none.
	AsOfLedger uint32 `json:"as_of_ledger,omitempty"`
	// Excluded is the pool-level exclusion (undecodable_storage), set
	// when the pool's legs are unknown. Legs is then empty.
	Excluded string `json:"excluded,omitempty"`
	// Legs are the pool's reserve legs in the reader's order. Never
	// null on the wire: an excluded pool publishes [].
	Legs []DEXTVLLegView `json:"legs"`
}

// ProtocolTVLDetailView is the wire shape of GET /v1/protocols/{name}/tvl.
type ProtocolTVLDetailView struct {
	Protocol string `json:"protocol"`
	// TVL is the SAME block /v1/protocols publishes on this protocol's
	// row for the same snapshot — same fields, same figure — so the two
	// surfaces can be checked against each other.
	TVL ProtocolTVLView `json:"tvl"`
	// CarriedForward is true when this protocol's reserve read failed on
	// the most recent refresh and the figure (and pools) shown are the
	// previous cycle's. The envelope's flags.stale says the same thing;
	// the headline tvl_total refuses such a figure (see
	// dexTVLRefusedStale) and this surface labels it instead of hiding
	// it.
	CarriedForward bool `json:"carried_forward"`
	// Pools are every pool the protocol figure was summed from, highest
	// tvl_usd first (ties by pool id), including the ones that
	// contributed nothing and say why.
	Pools []DEXTVLPoolView `json:"pools"`
}

// DEXTVLProtocolSnapshot is one protocol's entry in the cache: the
// published view plus the pools it was built from.
type DEXTVLProtocolSnapshot struct {
	TVL            ProtocolTVLView
	Pools          []DEXTVLPoolView
	CarriedForward bool
}

// tvlLegInput is one reserve leg as a reader delivers it, before
// valuation. An empty Token is a position whose address never resolved.
type tvlLegInput struct {
	token string
	raw   *big.Int
}

// tvlProtocolAccumulator folds pools into one protocol's ProtocolTVLView
// and its per-pool breakdown. Every one of the four refresh paths feeds
// it the same way, so the accounting rules (priced XOR unpriced, the
// lower-bound contract, the as-of high-water, the leaf rounding) live
// in exactly one place.
//
// Money is rounded ONCE, at the leaf: each valued leg is published to
// the cent, and every figure above it — the pool, the protocol, the
// headline total — is the exact sum of the published strings beneath.
// Rounding at every level instead would let a pool's legs sum to a cent
// more or less than the pool, and the whole point of this surface is
// that the rows reconcile with the figure above them.
type tvlProtocolAccumulator struct {
	valuer *tvlValuer
	view   ProtocolTVLView
	pools  []DEXTVLPoolView
	sums   map[string]*big.Rat // pool → its exact cent sum, for ordering
	total  *big.Rat
}

func newTVLProtocolAccumulator(valuer *tvlValuer, asOf, basis string) *tvlProtocolAccumulator {
	return &tvlProtocolAccumulator{
		valuer: valuer,
		view:   ProtocolTVLView{AsOf: asOf, Basis: basis},
		sums:   map[string]*big.Rat{},
		total:  new(big.Rat),
	}
}

// addPool values every leg and files the pool as priced or unpriced.
func (a *tvlProtocolAccumulator) addPool(ctx context.Context, pool string, ledger uint32, legs []tvlLegInput) {
	a.view.PoolsTotal++
	a.view.AsOfLedger = max(a.view.AsOfLedger, ledger)

	pv := DEXTVLPoolView{Pool: pool, AsOfLedger: ledger, Priced: true, Legs: make([]DEXTVLLegView, 0, len(legs))}
	sum := new(big.Rat)
	for _, leg := range legs {
		lv := a.valueLeg(ctx, leg)
		if lv.Excluded != "" {
			pv.Priced = false
		} else {
			// Always parses: USD was rendered by FloatString(2) a
			// moment ago. Summing the PUBLISHED cents rather than the
			// unrounded rational is what makes the pool figure the
			// exact sum of its rows.
			cents, _ := new(big.Rat).SetString(lv.USD)
			sum.Add(sum, cents)
		}
		pv.Legs = append(pv.Legs, lv)
	}
	pv.TVLUSD = sum.FloatString(2)
	a.total.Add(a.total, sum)
	a.sums[pool] = sum
	if pv.Priced {
		a.view.PoolsPriced++
	} else {
		a.view.UnpricedPools++
	}
	a.pools = append(a.pools, pv)
}

// valueLeg renders one leg for the wire: the captured reserve, the
// identity it was priced under, and its valuation or exclusion.
func (a *tvlProtocolAccumulator) valueLeg(ctx context.Context, leg tvlLegInput) DEXTVLLegView {
	lv := DEXTVLLegView{Token: leg.token}
	if leg.raw != nil {
		lv.Reserve = leg.raw.String()
	}
	if leg.token == "" {
		lv.Excluded = DEXTVLLegUnresolvedToken
		return lv
	}
	val := a.valuer.value(ctx, leg.token, leg.raw)
	lv.Asset, lv.Basis, lv.Excluded = val.asset, val.basis, val.excluded
	if val.usd != nil {
		lv.USD = val.usd.FloatString(2)
	}
	return lv
}

// addUndecodable files pools whose captured storage shape the reader
// refused to decode: counted in both pools_total and unpriced_pools,
// contributing exactly 0, published with no legs and the pool-level
// exclusion — an honest gap, never a fabricated figure.
func (a *tvlProtocolAccumulator) addUndecodable(pools []string) {
	for _, pool := range pools {
		a.view.PoolsTotal++
		a.view.UnpricedPools++
		a.sums[pool] = new(big.Rat)
		a.pools = append(a.pools, DEXTVLPoolView{
			Pool:     pool,
			TVLUSD:   "0.00",
			Excluded: DEXTVLPoolUndecodable,
			Legs:     []DEXTVLLegView{},
		})
	}
}

// finish renders the protocol figure — the exact sum of the pools'
// published cents — and orders the pools largest first, ties by id, so
// the drill-down is deterministic across refreshes.
func (a *tvlProtocolAccumulator) finish() (ProtocolTVLView, []DEXTVLPoolView) {
	a.view.TVLUSD = a.total.FloatString(2)
	sort.SliceStable(a.pools, func(i, j int) bool {
		if c := a.sums[a.pools[i].Pool].Cmp(a.sums[a.pools[j].Pool]); c != 0 {
			return c > 0
		}
		return a.pools[i].Pool < a.pools[j].Pool
	})
	return a.view, a.pools
}

// handleProtocolTVL serves GET /v1/protocols/{name}/tvl — the per-pool
// breakdown behind the `tvl` block on the protocol's directory row.
//
// It reads ONLY the in-process snapshot the background refresher
// maintains; no reserve table or price tier is touched per request, so
// the handler needs no budget of its own and cannot be made expensive by
// traffic. Degradation is explicit, never a fabricated empty figure:
//
//   - unknown protocol → 404 (same problem as /v1/protocols/{name});
//   - a known protocol with no pooled-liquidity derivation → 404 naming
//     the reason (the standing scope exclusion where one applies);
//   - no cache wired, or wired but not yet refreshed → 503 problem.
//
// A protocol whose figure is carried forward from an earlier cycle is
// SERVED, labelled carried_forward with flags.stale set — the headline
// total refuses such a figure, and this surface is where an operator
// goes to see exactly what was refused.
func (s *Server) handleProtocolTVL(w http.ResponseWriter, r *http.Request) {
	meta, ok := protocolByName(r.PathValue("name"))
	if !ok {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/protocol-not-found",
			"Protocol not found", http.StatusNotFound,
			"unknown protocol name; GET /v1/protocols lists every known protocol")
		return
	}
	if s.dexTVL == nil {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/dex-tvl-unavailable",
			"DEX TVL unavailable", http.StatusServiceUnavailable,
			"This deployment hasn't wired the DEX TVL snapshot cache.")
		return
	}
	if _, at := s.dexTVL.Snapshot(); at.IsZero() {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/dex-tvl-unavailable",
			"DEX TVL unavailable", http.StatusServiceUnavailable,
			"the first DEX TVL snapshot refresh has not completed; retry in a few seconds")
		return
	}
	snap, ok := s.dexTVL.Protocol(meta.Name)
	if !ok {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/protocol-tvl-not-derived",
			"No TVL derivation for this protocol", http.StatusNotFound,
			dexTVLNotDerivedReason(meta.Name))
		return
	}
	view := ProtocolTVLDetailView{
		Protocol:       meta.Name,
		TVL:            snap.TVL,
		CarriedForward: snap.CarriedForward,
		Pools:          snap.Pools,
	}
	if view.Pools == nil {
		view.Pools = []DEXTVLPoolView{}
	}
	writeJSON(w, view, Flags{Stale: snap.CarriedForward}, meta.Name)
}

// dexTVLNotDerivedReason explains a 404 on a KNOWN protocol: the
// standing scope decision where one names it (so /v1/protocols/sdex/tvl
// says the same thing tvl_total.excluded does), otherwise the generic
// "not wired here" — a protocol with an absolute reserve source whose
// readers this deployment did not wire is simply absent from the
// snapshot, and absence is honest "unavailable", never zero TVL.
func dexTVLNotDerivedReason(name string) string {
	for _, ex := range dexTVLScopeExclusions {
		if ex.Subject == name {
			return ex.Reason
		}
	}
	return "this protocol's pooled-liquidity derivation is not wired on this deployment; " +
		"tvl_total.excluded on GET /v1/protocols names every surface the headline omits"
}
