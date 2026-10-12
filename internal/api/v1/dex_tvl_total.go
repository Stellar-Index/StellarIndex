package v1

import (
	"math/big"
	"sort"
	"strings"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/obs"
)

// DEXTVLTotalView is the headline TVL: the EXACT sum of the published protocols[] tvl_usd strings (never re-derived), deliberately not whole-network, with `excluded` naming what is left out.
type DEXTVLTotalView struct {
	// TVLUSD is the exact sum of the included protocols' published tvl_usd strings (decimal string).
	TVLUSD string `json:"tvl_usd"`
	// Protocols names, sorted, exactly the protocols summed into TVLUSD.
	Protocols []string `json:"protocols"`
	// LowerBound is true when an included protocol has unpriced pools, the reconciliation
	// dropped a protocol, or a derived protocol published no figure this cycle: the named
	// protocols' true value is at least TVLUSD. It says nothing about `Excluded`, which is a
	// scope statement, not a valuation gap.
	LowerBound bool `json:"lower_bound"`
	// PoolsTotal / PoolsPriced / UnpricedPools sum the included protocols' pool counts.
	PoolsTotal    int `json:"pools_total"`
	PoolsPriced   int `json:"pools_priced"`
	UnpricedPools int `json:"unpriced_pools"`
	// AsOfLedger is the highest chain high-water across the included protocols; omitted when none
	// carried a ledger.
	AsOfLedger uint32 `json:"as_of_ledger,omitempty"`
	// AsOf (RFC3339) equals every included protocol's as_of, since reconciliation refuses a
	// protocol whose as_of differs.
	AsOf string `json:"as_of"`
	// Basis is a one-line provenance statement for the total.
	Basis string `json:"basis"`
	// Excluded lists the pooled-value surfaces NOT in TVLUSD with reasons: the standing scope
	// decisions plus any protocol refused this cycle. Never empty.
	Excluded []DEXTVLExclusion `json:"excluded"`
}

// DEXTVLExclusion is one thing the headline total omits and why; `subject` is a protocol name where one applies.
type DEXTVLExclusion struct {
	Subject string `json:"subject"`
	Reason  string `json:"reason"`
}

// dexTVLScopeExclusions are the standing scope decisions: pooled- or locked-value surfaces we
// index but deliberately do not sum. They ship on every response so the total cannot be read
// as a whole-network claim by omission. Each entry must name something we hold data for today.
var dexTVLScopeExclusions = []DEXTVLExclusion{
	{
		Subject: "classic liquidity pools",
		Reason: "Stellar's protocol-native CAP-38 constant-product pools are indexed and served " +
			"per-pool at /v1/liquidity-pools (two-sided reserves + as_of_ledger). They belong to no " +
			"protocol row: they are a ledger primitive, not a deployed contract or venue, and filing " +
			"them under sdex would mix pooled reserves with order-book depth. If valued, they are " +
			"summed as their own class, never folded into another protocol's figure",
	},
	{
		Subject: "sdex",
		Reason: "the classic order book holds offers, not pooled reserves — resting depth is a " +
			"different quantity from locked value and is served separately at /v1/sdex/orderbook",
	},
	{
		Subject: "sushiswap_v3",
		Reason: "concentrated liquidity is not constant product: a V3 pool spreads its depth " +
			"across per-position tick ranges, so the pool's token balances are not a two-sided " +
			"reserve and their sum is not the pool's value — that needs the live sqrt price read " +
			"against every open position's bounds, which is not captured as current state. " +
			"Running the reserve path over the balances anyway would produce a number with no " +
			"meaning, so no figure is derived at all; the protocol's swaps are served at " +
			"/v1/pools and its per-contract coverage at /v1/protocols/{name}",
	},
	{
		Subject: "soroswap-router",
		Reason: "the router holds no liquidity of its own — its rows are multi-hop swap intents " +
			"routed through the same Soroswap pairs already summed under soroswap, so adding it " +
			"would double-count those pools",
	},
	{
		Subject: "blend",
		Reason: "lending supplied-value is a different quantity from AMM pooled liquidity; Blend's " +
			"current-state figure is served per-pool as tvl_usd on /v1/lending/pools/{pool}/reserves " +
			"(ADR-0039, decoded from contract storage) and summing lending into an AMM headline " +
			"would flatter it",
	},
	{
		Subject: "sorocredit",
		Reason: "lending supplied-value, excluded on the same basis as blend; no current-state " +
			"figure exists for it — its protocol block carries event and user counts only",
	},
	{
		Subject: "defindex",
		Reason: "vault capital is deployed into Blend strategy contracts, so counting vault AUM " +
			"alongside the protocols holding those positions would double-count it",
	},
	{
		Subject: "upshift",
		Reason: "no current-state total-assets figure exists for it — the only on-event total is " +
			"the DEPLOYED leg (deployed_assets_changed), the idle leg is unobservable from indexed " +
			"events, and a TVL reader must not zero-fill the missing leg; its protocol block " +
			"carries event and user counts only",
	},
}

// Reconciliation refusal reasons. A protocol tripping one is LEFT OUT of the total and named in
// `excluded`, rather than absorbing a figure whose own claims do not hold.
const (
	dexTVLRefusedStale = "carried forward from an earlier refresh (its reserve read failed this cycle), " +
		"so it cannot be published under this snapshot's as_of"
	dexTVLRefusedUnparseable = "its published tvl_usd is not a usable non-negative decimal, " +
		"so it cannot be summed"
	dexTVLRefusedPoolAccounting = "its pool accounting does not balance " +
		"(pools_total != pools_priced + unpriced_pools), so the figure's coverage is unknown"
)

// Unavailability reasons: a protocol whose TVL is derived here but published no figure this cycle.
// Named in `excluded` and quoted by its /v1/protocols/{name}/tvl 404, so it is never mistaken for
// an unwired one.
const (
	dexTVLUnavailableReadFailed = "its reserve read failed this cycle and no earlier figure exists " +
		"to carry forward, so no figure is published — the protocol is derived here, its TVL is not zero"
	dexTVLUnavailableNoPools = "its reserve read returned no pools this cycle; absence is " +
		"unavailable, never zero TVL, so no figure is published"
)

// Total returns the latest reconciled headline total, or nil before the first refresh (handlers omit the field, never 503).
func (c *DEXTVLCache) Total() *DEXTVLTotalView {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.total
}

// reconcileDEXTVLTotal folds a per-protocol snapshot into the headline total, admitting only
// parts whose own published claims hold. It REFUSES rather than warns: a protocol failing
// admission is dropped from the sum and named in `excluded`, so a wrong total is never served.
//
// Refusals:
//
//   - stale: Refresh's carryPrev keeps a protocol's previous entry when its reserve read
//     fails. Refresh NAMES carried protocols instead of comparing stamps: RFC3339 is
//     second-resolution, so a carried figure could share an as_of and pass an equality test.
//   - unparseable: the sum must be exact; a part that is not a non-negative
//     decimal has no defined contribution.
//   - pool accounting: every pool is priced XOR unpriced, so pools_total must
//     equal their sum; otherwise the lower-bound claim is unprovable.
//
// carried and unavailable protocols are listed in `excluded` and make the total
// a lower bound. Returns nil for an empty snapshot.
func reconcileDEXTVLTotal(snapshot map[string]ProtocolTVLView, at time.Time, carried []string, unavailable []DEXTVLExclusion) *DEXTVLTotalView {
	if len(snapshot) == 0 {
		return nil
	}
	names := make([]string, 0, len(snapshot))
	for name := range snapshot {
		names = append(names, name)
	}
	sort.Strings(names)

	stamp := at.Format(time.RFC3339)
	carriedSet := make(map[string]bool, len(carried))
	for _, name := range carried {
		carriedSet[name] = true
	}
	sum := new(big.Rat)
	out := &DEXTVLTotalView{AsOf: stamp}
	var refused []DEXTVLExclusion

	for _, name := range names {
		part := snapshot[name]
		amount, reason, ok := dexTVLAdmit(part, stamp, carriedSet[name])
		if !ok {
			refused = append(refused, DEXTVLExclusion{Subject: name, Reason: reason})
			continue
		}
		sum.Add(sum, amount)
		out.Protocols = append(out.Protocols, name)
		out.PoolsTotal += part.PoolsTotal
		out.PoolsPriced += part.PoolsPriced
		out.UnpricedPools += part.UnpricedPools
		out.AsOfLedger = max(out.AsOfLedger, part.AsOfLedger)
	}

	if len(out.Protocols) == 0 {
		// Publishing "0.00" would read as a real zero-dollar total; tvl_total is an omitempty
		// pointer so it can be absent while per-protocol figures stay on the wire.
		obs.DEXTVLReconcileTotal.WithLabelValues("divergent").Inc()
		return nil
	}
	out.TVLUSD = sum.FloatString(2)
	out.LowerBound = out.UnpricedPools > 0 || len(refused) > 0 || len(unavailable) > 0
	out.Excluded = make([]DEXTVLExclusion, 0, len(unavailable)+len(refused)+len(dexTVLScopeExclusions))
	out.Excluded = append(out.Excluded, unavailable...)
	out.Excluded = append(out.Excluded, refused...)
	out.Excluded = append(out.Excluded, dexTVLScopeExclusions...)
	out.Basis = dexTVLTotalBasis(out.Protocols)

	outcome := "ok"
	if len(refused) > 0 {
		outcome = "divergent"
	}
	obs.DEXTVLReconcileTotal.WithLabelValues(outcome).Inc()
	return out
}

// dexTVLAdmit decides whether a per-protocol figure may be summed into the headline total and
// returns the parsed amount from the SAME call that validated it, so a caller can never sum a
// figure this function did not vouch for. ok=false returns the refusal reason for `excluded`.
func dexTVLAdmit(part ProtocolTVLView, stamp string, carried bool) (*big.Rat, string, bool) {
	// The stamp comparison is defence in depth: a part built against a different instant is the
	// same inconsistency Refresh reports as carried.
	if carried || part.AsOf != stamp {
		return nil, dexTVLRefusedStale, false
	}
	if part.PoolsTotal != part.PoolsPriced+part.UnpricedPools {
		return nil, dexTVLRefusedPoolAccounting, false
	}
	amount, parsed := new(big.Rat).SetString(part.TVLUSD)
	if !parsed || amount.Sign() < 0 {
		return nil, dexTVLRefusedUnparseable, false
	}
	return amount, "", true
}

// dexTVLTotalBasis names the protocols summed. It never claims network coverage; `excluded` is
// the other half of the statement.
func dexTVLTotalBasis(protocols []string) string {
	if len(protocols) == 0 {
		return "no protocol figure could be admitted to the total this cycle; see excluded"
	}
	return "exact sum of the published per-protocol tvl_usd for " +
		strings.Join(protocols, ", ") +
		" — add those rows and you get this number; see each protocol's own basis for how it " +
		"was measured and valued, and excluded for the pooled-value surfaces this total omits"
}
