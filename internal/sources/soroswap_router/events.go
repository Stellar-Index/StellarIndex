// Package soroswap_router decodes Soroban InvokeContract calls
// against the Soroswap Router contract. The router emits no events
// itself (its work is calling down to per-pair contracts that
// emit `SoroswapPair("swap")`); this package observes the router's
// invocation directly via dispatcher.ContractCallDecoder so we
// capture user-level intent (path, amount_in, amount_out_min)
// distinct from the per-pair leg-level swaps.
//
// Sister package: internal/sources/soroswap (pair + factory event
// decoder). Same upstream protocol; different vantage point.
package soroswap_router

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// SourceName is the registry key for this source. Used in
// `external.Registry`, `routers.name`, and trade attribution.
const SourceName = "soroswap-router"

// MainnetRouter is the contract ID of the Soroswap router on
// Stellar pubnet. Verified in
// docs/discovery/dexes-amms/soroswap.md against the
// `public/router.json` config in the soroswap-core repo.
const MainnetRouter = "CAG5LRYQ5JVEUI5TEID72EYOVX44TTUJT5BQR2J6J77FH65PCCFAJDDH"

// Function names the router exposes. We track only the swap
// entry points; admin / read-only methods (set_pair_fee,
// router_pairs, init, …) don't move tokens and aren't useful
// for attribution. Per docs/operations/wasm-audits/soroswap-router.md
// (2026-05-19 export-name dump of the router's single, never-upgraded
// WASM hash), these two entry points are the router's COMPLETE
// token-moving surface — there is no third swap function this decoder
// could be missing.
const (
	FnSwapExactTokensForTokens = "swap_exact_tokens_for_tokens"
	FnSwapTokensForExactTokens = "swap_tokens_for_exact_tokens"
)

// CallKind discriminates where in a tx's Soroban auth tree a router
// call was observed. ROADMAP #11 / docs/architecture/contract-call-
// coverage-audit.md: most real router traffic arrives as a
// sub-invocation (the router invoked by an aggregator or another
// contract), which the dispatcher only started walking as of task #48
// Phase 1 — pre-fix, ContractCallDecoder routing saw top-level calls
// only and undercounted router activity ~8,729x.
const (
	// CallKindTopLevel is a direct call: the router is the tx op's own
	// InvokeContract target (CallDepth == 0, CallPath == [router]).
	CallKindTopLevel = "top_level"
	// CallKindSubInvocation is a nested call: some other contract
	// (an aggregator, a batcher, …) invoked the router as part of its
	// own authorized call tree (CallDepth > 0).
	CallKindSubInvocation = "sub_invocation"
)

// RouterSwap is the canonical wire shape one router invocation
// projects to. One `RouterSwap` corresponds to ONE call to
// `swap_exact_tokens_for_tokens` / `swap_tokens_for_exact_tokens`,
// which in turn emits N per-pair `Trade` events (one per hop) from
// the existing soroswap pair decoder.
//
// Path is the hop sequence the user requested (or that the router
// computed). Length 2 = direct swap (single pair); length 3+ =
// multi-hop. Each adjacent pair (Path[i], Path[i+1]) maps to one
// pair contract that emits the underlying swap event.
//
// Function discriminates the two router entry points:
//   - `swap_exact_tokens_for_tokens`: user fixes input, accepts
//     any output ≥ AmountOutMin.
//   - `swap_tokens_for_exact_tokens`: user fixes output, accepts
//     any input ≤ AmountInMax.
//
// AmountIn / AmountOut both populate; the "min" / "max" semantics
// depend on Function. Slippage analysis happens at the aggregator
// level by comparing requested vs realized amounts (the realized
// amount comes from the per-pair swap events with matching tx_hash).
type RouterSwap struct {
	Source     string // always SourceName
	Ledger     uint32
	ClosedAt   time.Time
	TxHash     string
	OpIndex    int
	OpSource   string // operation source (G-strkey / muxed)
	TxSource   string // tx source (G-strkey)
	ContractID string // always MainnetRouter (mainnet)

	Function  string // FnSwap*
	Recipient string // `to` arg — where output lands
	// Path is the hop sequence of token contract C-strkeys
	// the router walked. Stored as raw C-strkeys so the
	// downstream SAC-wrapper resolver (cfg.Supply.SacWrappers)
	// can map to canonical.Asset on its own schedule. Length
	// ≥ 2 by router contract precondition.
	Path []string
	// AmountIn / AmountOut mix a REALIZED amount with a user-supplied LIMIT,
	// per Function: swap_exact_tokens_for_tokens fixes AmountIn (realized) and
	// AmountOut is `amount_out_min` (a lower bound); swap_tokens_for_exact_tokens
	// fixes AmountOut (realized) and AmountIn is `amount_in_max` (an upper
	// bound). NEVER treat AmountOut/AmountIn as an execution price — one leg is
	// a slippage guardrail, not a fill. The realized price comes from the
	// per-pair swap events (matching tx_hash), which carry both actual amounts;
	// this struct is the router's INTENT record only.
	AmountIn   canonical.Amount // realized (exact-in fn) OR amount_in_max upper bound
	AmountOut  canonical.Amount // realized (exact-out fn) OR amount_out_min lower bound
	DeadlineTs time.Time        // user-supplied expiry

	// CallPath is the ordered chain of contract C-strkeys from the
	// top-level invocation down to and including the router itself —
	// sourced from dispatcher.ContractCallContext.CallPathContracts
	// (the task #48 auth-tree walk). Length 1 for a direct call
	// (CallPath == [ContractID]); length >1 for a sub-invocation, e.g.
	// [aggregator, router] when an aggregator wraps the router one
	// level deep. CallPath[0] is always the outermost invoked
	// contract; CallPath[len(CallPath)-1] always equals ContractID.
	//
	// ROADMAP #11: this is the column that turns "the dispatcher now
	// SEES sub-invocations" into "we can tell operators WHO wrapped
	// the router and how deep" — without it every captured call looks
	// identical regardless of whether it was direct or aggregator-routed.
	CallPath []string
	// CallDepth is len(CallPath)-1: 0 for a direct call, N for an
	// N-level-deep sub-invocation. Redundant with CallPath's length
	// but stored/queried directly so a coverage dashboard doesn't need
	// array-length arithmetic in SQL.
	CallDepth int
	// CallKind is the discriminator: CallKindTopLevel when CallDepth
	// == 0, CallKindSubInvocation otherwise. Cheap to filter/aggregate
	// on directly (e.g. "what fraction of router activity is
	// aggregator-routed").
	CallKind string
}

// Event wraps a RouterSwap so it satisfies consumer.Event for
// the dispatcher / pipeline path. The persist layer writes one
// soroswap_router_swaps row per invocation (pipeline/sink.go);
// same-tx Trade rows are then tagged with trades.routed_via =
// SourceName by the routed-via sweeper (Phase B —
// internal/pipeline/routedvia.go live, `stellarindex-ops
// tag-routed-via` historical).
type Event struct {
	Swap RouterSwap
}

// EventKind implements [consumer.Event].
func (e Event) EventKind() string { return "soroswap-router.swap" }

// Source implements [consumer.Event].
func (e Event) Source() string { return SourceName }

// CallSig is the per-call discriminator in the soroswap_router_swaps PK. A
// single InvokeContract op can carry MULTIPLE distinct router swaps — an
// aggregator splitting a trade, or a batch distributing to several recipients —
// which all share (ledger, tx_hash, op_index). Without a discriminator the
// served PK collapses them to one row (verified: 106 genuinely-distinct swaps
// across pubnet history were being lost). CallSig is a deterministic 128-bit
// content hash over the swap's economic identity (function + recipient + path +
// requested amounts), so distinct swaps get distinct PKs (all stored), while
// auth-tree DUPLICATES of the same call — a multi-entry (co-signed) tx surfaces
// the identical call at several CallPaths — hash equal and dedup via ON
// CONFLICT. deadline is excluded: it's a user sentinel (often garbage; see the
// deadline_ts NULL-clamp) that doesn't distinguish economic intent.
//
// CallPath/CallDepth/CallKind (ROADMAP #11) are deliberately EXCLUDED
// from the hash for the same reason as deadline: they describe WHERE
// in the tx a call was observed, not the economic content of the
// swap. Including them would break the auth-tree-duplicate dedup this
// doc comment describes — the same economic call surfacing at two
// CallPaths in a co-signed tx must still collapse to one row.
func (s RouterSwap) CallSig() string {
	parts := make([]string, 0, len(s.Path)+4)
	parts = append(parts, s.Function, s.Recipient)
	parts = append(parts, s.Path...)
	parts = append(parts, s.AmountIn.String(), s.AmountOut.String())
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(sum[:16])
}
