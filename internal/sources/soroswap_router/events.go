// Package soroswap_router decodes InvokeContract calls to the Soroswap Router. The router emits no
// events itself (per-pair contracts emit `SoroswapPair("swap")`), so this package observes the invocation
// via dispatcher.ContractCallDecoder to capture user-level intent (path, amounts) distinct from the
// per-pair legs decoded by internal/sources/soroswap.
package soroswap_router

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// SourceName is the registry key, used in `external.Registry`, `routers.name` and trade attribution.
const SourceName = "soroswap-router"

// MainnetRouter is the pubnet router contract (docs/operations/wasm-audits/soroswap-router.md).
const MainnetRouter = "CAG5LRYQ5JVEUI5TEID72EYOVX44TTUJT5BQR2J6J77FH65PCCFAJDDH"

// Swap entry points tracked. Per the WASM audit's export dump of the router's single, never-upgraded
// hash, these two are its COMPLETE token-moving surface; admin and read-only methods are not tracked.
const (
	FnSwapExactTokensForTokens = "swap_exact_tokens_for_tokens"
	FnSwapTokensForExactTokens = "swap_tokens_for_exact_tokens"
)

// CallKind says where in a tx's Soroban auth tree a router call was observed. Most router traffic
// is a sub-invocation; routing that saw top-level calls only undercounted it ~8,729x.
const (
	// CallKindTopLevel is a direct call (CallDepth == 0, CallPath == [router]).
	CallKindTopLevel = "top_level"
	// CallKindSubInvocation is a call nested in another contract's authorized tree (CallDepth > 0).
	CallKindSubInvocation = "sub_invocation"
)

// RouterSwap is one call to `swap_exact_tokens_for_tokens` (input fixed, output >= AmountOutMin) or
// `swap_tokens_for_exact_tokens` (output fixed, input <= AmountInMax); each hop's pair emits its own
// Trade. Path has 2 entries for a direct swap, 3+ for multi-hop.
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
	// Path is the walked token C-strkeys, kept raw so the SAC-wrapper resolver
	// (cfg.Supply.SacWrappers) maps them on its own schedule. Length >= 2.
	Path []string
	// AmountIn / AmountOut mix a realized amount with a user LIMIT per Function (amount_out_min for
	// exact-in, amount_in_max for exact-out). NEVER treat them as an execution price; this is the INTENT
	// record, and realized amounts come from the per-pair swap events.
	AmountIn   canonical.Amount // realized (exact-in fn) OR amount_in_max upper bound
	AmountOut  canonical.Amount // realized (exact-out fn) OR amount_out_min lower bound
	DeadlineTs time.Time        // user-supplied expiry

	// CallPath is the contract chain from the top-level invocation down to the router
	// (dispatcher.ContractCallContext.CallPathContracts); its last element is always ContractID. It is
	// what tells operators who wrapped the router and how deep.
	CallPath []string
	// CallDepth is len(CallPath)-1, stored so dashboards avoid array-length SQL.
	CallDepth int
	// CallKind is CallKindTopLevel when CallDepth == 0, else CallKindSubInvocation.
	CallKind string
	// AuthOccurrence is dispatcher.ContractCallContext.AuthOccurrence:
	// 0 for the first identical call in its auth entry, n for the
	// (n+1)th. It enters CallSig so repeated executions stay distinct.
	AuthOccurrence int
}

// Event wraps a RouterSwap as a consumer.Event: one soroswap_router_swaps row per invocation, after
// which the routed-via sweeper tags same-tx trades (internal/pipeline/routedvia.go; `tag-routed-via` for history).
type Event struct {
	Swap RouterSwap
}

// EventKind implements [consumer.Event].
func (e Event) EventKind() string { return "soroswap-router.swap" }

// Source implements [consumer.Event].
func (e Event) Source() string { return SourceName }

// CallSig is the per-call discriminator in the soroswap_router_swaps PK: one op can carry several
// distinct router swaps sharing (ledger, tx_hash, op_index), and without it 106 were lost across pubnet
// history. It is a 128-bit hash of the economic identity (function, recipient, path, requested amounts),
// so auth-tree duplicates of one call (a co-signed tx surfaces it at several CallPaths) dedup via ON
// CONFLICT. deadline and CallPath/CallDepth/CallKind describe the sentinel or position, not the swap, so
// they are excluded. AuthOccurrence is the one positional input: two identical calls in one auth entry
// are two executions; occurrence 0 adds nothing to the hash, so stored call_sigs are unchanged.
func (s RouterSwap) CallSig() string {
	parts := make([]string, 0, len(s.Path)+5)
	parts = append(parts, s.Function, s.Recipient)
	parts = append(parts, s.Path...)
	parts = append(parts, s.AmountIn.String(), s.AmountOut.String())
	if s.AuthOccurrence > 0 {
		parts = append(parts, "occurrence="+strconv.Itoa(s.AuthOccurrence))
	}
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(sum[:16])
}
