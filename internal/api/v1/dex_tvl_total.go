package v1

import (
	"math/big"
	"sort"
	"strings"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/obs"
)

// DEXTVLTotalView is the headline TVL figure across the pooled-liquidity
// protocols the snapshot covers (#338) — the sum of the per-protocol
// figures actually published on the same response, never an independent
// re-derivation.
//
// That definition is the point. `tvl_usd` is the EXACT sum of the
// `tvl_usd` strings on the `protocols[]` rows, so a consumer who adds up
// what it can see reconciles with us byte-for-byte; a total computed
// from the underlying rationals would round-trip to a different number
// than the published parts and there would be no way for a caller to
// tell which of us was wrong. Each part carries exactly two decimal
// places, so their sum does too and no rounding occurs here.
//
// It is deliberately NOT a whole-network figure: `excluded` enumerates
// every pooled-value surface we index and consciously leave out, with
// the reason. See docs/methodology/dex-tvl.md.
type DEXTVLTotalView struct {
	// TVLUSD is the exact sum of the included protocols' published
	// tvl_usd strings, as a decimal string (ADR-0003).
	TVLUSD string `json:"tvl_usd"`
	// Protocols names, in registry-independent sorted order, exactly
	// which protocols' figures are summed into TVLUSD. A consumer can
	// re-add these rows and get TVLUSD.
	Protocols []string `json:"protocols"`
	// LowerBound is true when at least one included protocol has
	// unpriced pools, or a protocol was dropped by the reconciliation
	// below — i.e. the named protocols' true pooled value is at least
	// TVLUSD. It says nothing about `Excluded`, which is a SCOPE
	// statement, not a valuation gap: overloading one boolean with both
	// meanings would make neither legible.
	LowerBound bool `json:"lower_bound"`
	// PoolsTotal / PoolsPriced / UnpricedPools are the summed pool
	// counts across the included protocols.
	PoolsTotal    int `json:"pools_total"`
	PoolsPriced   int `json:"pools_priced"`
	UnpricedPools int `json:"unpriced_pools"`
	// AsOfLedger is the highest chain high-water across the included
	// protocols (see ProtocolTVLView.AsOfLedger). 0/omitted when none
	// carried a ledger.
	AsOfLedger uint32 `json:"as_of_ledger,omitempty"`
	// AsOf is when the snapshot the total was reconciled from was
	// computed (RFC3339) — identical to every included protocol's
	// as_of, because a protocol whose as_of differs is refused
	// admission by the reconciliation.
	AsOf string `json:"as_of"`
	// Basis is a one-line provenance statement for the total.
	Basis string `json:"basis"`
	// Excluded enumerates the pooled-value surfaces deliberately NOT in
	// TVLUSD, each with its reason — both the standing scope decisions
	// and any protocol the reconciliation refused this cycle. Never
	// empty: the scope entries always apply.
	Excluded []DEXTVLExclusion `json:"excluded"`
}

// DEXTVLExclusion is one thing the headline total does not contain and
// why. `subject` is a protocol name where one applies, otherwise a
// short noun phrase for the surface.
type DEXTVLExclusion struct {
	Subject string `json:"subject"`
	Reason  string `json:"reason"`
}

// dexTVLScopeExclusions are the STANDING scope decisions — the pooled-
// or locked-value surfaces Stellar Index indexes and this total
// consciously omits. They are published on every response so the number
// can never be read as a whole-network claim by omission.
//
// Written from what ships, not from intent: each entry names something
// we genuinely hold data for today and genuinely do not sum here.
var dexTVLScopeExclusions = []DEXTVLExclusion{
	{
		Subject: "classic liquidity pools",
		Reason: "Stellar's protocol-native CAP-38 constant-product pools are indexed and served " +
			"per-pool at /v1/liquidity-pools (two-sided reserves + as_of_ledger) but are not yet " +
			"valued into a protocol row; which protocol they attach to is an open product decision (#338)",
	},
	{
		Subject: "sdex",
		Reason: "the classic order book holds offers, not pooled reserves — resting depth is a " +
			"different quantity from locked value and is served separately at /v1/sdex/orderbook",
	},
	{
		Subject: "blend",
		Reason: "lending supplied-value is a different quantity from AMM pooled liquidity; it is " +
			"published per-protocol as bespoke.tvl_usd and summing the two would flatter the headline",
	},
	{
		Subject: "sorocredit",
		Reason:  "lending supplied-value, excluded on the same basis as blend",
	},
	{
		Subject: "defindex",
		Reason: "vault capital is deployed into Blend strategy contracts, so counting vault AUM " +
			"alongside the protocols holding those positions would double-count it",
	},
}

// Reconciliation refusal reasons. A protocol that trips one of these is
// LEFT OUT of the total and named in `excluded` — the total shrinks and
// says why, rather than absorbing a figure whose own claims do not hold.
const (
	dexTVLRefusedStale = "carried forward from an earlier refresh (its reserve read failed this cycle), " +
		"so it cannot be published under this snapshot's as_of"
	dexTVLRefusedUnparseable = "its published tvl_usd is not a usable non-negative decimal, " +
		"so it cannot be summed"
	dexTVLRefusedPoolAccounting = "its pool accounting does not balance " +
		"(pools_total != pools_priced + unpriced_pools), so the figure's coverage is unknown"
)

// Total returns the reconciled headline total for the most recent
// refresh, or nil before the first refresh completes (cold start —
// handlers omit the field, never 503).
func (c *DEXTVLCache) Total() *DEXTVLTotalView {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.total
}

// reconcileDEXTVLTotal folds a per-protocol snapshot into the headline
// total, admitting only the parts whose own published claims hold
// (#338). This is the divergence check the issue asks for, and its
// action is to REFUSE rather than to warn: a protocol that fails
// admission is dropped from the sum and named in `excluded`, so a
// wrong total is never served in the first place.
//
// The three refusals are the ways a part can be internally inconsistent
// with the response it is published in:
//
//   - stale — Refresh's carryPrev keeps a protocol's PREVIOUS entry when
//     its reserve read fails, so `snapshot` can hold a figure from an
//     earlier cycle. Summing it would stamp the total with an `as_of`
//     no component honours. This is the live failure mode: it happens
//     every time stellarindex_dex_tvl_refresh_failing is true. Refresh
//     NAMES the carried protocols rather than leaving this to be
//     inferred from a stamp comparison — RFC3339 is second-resolution,
//     so two refreshes can share an as_of and a carried figure would
//     pass an equality test while being a cycle old.
//   - unparseable — the sum must be exact; a part that does not parse
//     as a non-negative decimal has no defined contribution.
//   - pool accounting — every pool is counted priced XOR unpriced, so
//     pools_total must equal their sum. When it doesn't, the money and
//     the coverage claim came from different accountings and the
//     lower-bound story is no longer provable.
//
// at is the refresh instant the snapshot was computed at and carried
// names the protocols serving a previous cycle's figure. Returns nil
// for an empty snapshot (nothing to total).
func reconcileDEXTVLTotal(snapshot map[string]ProtocolTVLView, at time.Time, carried []string) *DEXTVLTotalView {
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
		// Nothing was admitted. Publishing "0.00" here would be a
		// NUMBER where the honest answer is silence: a consumer reading
		// tvl_total.tvl_usd without also reading basis and excluded
		// would see a real total of zero dollars, which is the one
		// reading that is definitely wrong. tvl_total is `omitempty` on
		// a pointer precisely so it can be absent, and the per-protocol
		// figures are still on the wire beside it. Same posture the
		// listing takes when its price rollup is stale: render
		// unpriced, never a plausible-looking wrong figure.
		obs.DEXTVLReconcileTotal.WithLabelValues("divergent").Inc()
		return nil
	}
	out.TVLUSD = sum.FloatString(2)
	out.LowerBound = out.UnpricedPools > 0 || len(refused) > 0
	out.Excluded = append(refused, dexTVLScopeExclusions...)
	out.Basis = dexTVLTotalBasis(out.Protocols)

	outcome := "ok"
	if len(refused) > 0 {
		outcome = "divergent"
	}
	obs.DEXTVLReconcileTotal.WithLabelValues(outcome).Inc()
	return out
}

// dexTVLAdmit decides whether a per-protocol figure may be summed into
// the headline total, and on admission returns the parsed amount. The
// admitted value comes back from the SAME call that validated it, so a
// caller can never sum a figure this function did not vouch for —
// re-parsing at the call site would leave a nil *big.Rat reachable the
// moment an admission rule was loosened. ok=false returns the refusal
// reason verbatim for `excluded`.
func dexTVLAdmit(part ProtocolTVLView, stamp string, carried bool) (*big.Rat, string, bool) {
	// Carried-forward is reported by Refresh; the stamp comparison is a
	// second, independent way the same inconsistency can show up (a part
	// built against a different instant), kept as defence in depth.
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

// dexTVLTotalBasis states what the total is over, naming the protocols
// summed. It never claims network coverage — the `excluded` list is the
// other half of the same sentence.
func dexTVLTotalBasis(protocols []string) string {
	if len(protocols) == 0 {
		return "no protocol figure could be admitted to the total this cycle; see excluded"
	}
	return "exact sum of the published per-protocol tvl_usd for " +
		strings.Join(protocols, ", ") +
		" — add those rows and you get this number; see each protocol's own basis for how it " +
		"was measured and valued, and excluded for the pooled-value surfaces this total omits"
}
