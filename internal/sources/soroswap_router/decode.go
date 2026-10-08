package soroswap_router

import (
	"errors"
	"fmt"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/scval"
)

// ErrMalformedArgs flags call args that don't match the function's shape; the call is skipped and counted.
var ErrMalformedArgs = errors.New("soroswap_router: malformed args")

// ErrUnknownFunction is a defensive double-check behind the dispatcher's Matches().
var ErrUnknownFunction = errors.New("soroswap_router: unknown function")

// decodeRouterArgs converts one Soroswap router InvokeContract call into a RouterSwap, returning
// ErrMalformedArgs / ErrUnknownFunction for skip-and-count cases. Signatures (soroswap-core
// contracts/router/src/lib.rs):
//
//	swap_exact_tokens_for_tokens(
//	    amount_in:        i128,
//	    amount_out_min:   i128,
//	    path:             Vec<Address>,
//	    to:               Address,
//	    deadline:         u64,
//	) -> Vec<i128>   // realized per-hop amounts
//
//	swap_tokens_for_exact_tokens(
//	    amount_out:       i128,
//	    amount_in_max:    i128,
//	    path:             Vec<Address>,
//	    to:               Address,
//	    deadline:         u64,
//	) -> Vec<i128>
//
// The return value is not routed to us; realized amounts come from the same tx's SoroswapPair("swap") events.
func decodeRouterArgs(
	fnName string,
	args []string,
	contractID string,
	ledger uint32,
	txHash string,
	opIndex int,
	opSource, txSource string,
	closedAt time.Time,
	callPath []string,
) (*RouterSwap, error) {
	if fnName != FnSwapExactTokensForTokens && fnName != FnSwapTokensForExactTokens {
		return nil, ErrUnknownFunction
	}
	// Exact arity: a call with extra args is a shape we have not audited,
	// and decoding its first five positionally could misread every field.
	if len(args) != 5 {
		return nil, fmt.Errorf("%w: %s expects 5 args, got %d", ErrMalformedArgs, fnName, len(args))
	}
	// Named locals keep the bounds check visible to gosec G602 across the long body.
	rawAmount0, rawAmount1, rawPath, rawTo, rawDeadline := args[0], args[1], args[2], args[3], args[4]

	// Positions 0 and 1 are i128 whichever function; their meaning is mapped below.
	a0, err := parseI128(rawAmount0)
	if err != nil {
		return nil, fmt.Errorf("%w: args[0]: %w", ErrMalformedArgs, err)
	}
	a1, err := parseI128(rawAmount1)
	if err != nil {
		return nil, fmt.Errorf("%w: args[1]: %w", ErrMalformedArgs, err)
	}

	// Position 2: Vec<Address> path (2 = direct, 3+ = multi-hop). The router refuses len < 2, so a
	// short path is something the router itself would reject.
	pathSv, err := scval.Parse(rawPath)
	if err != nil {
		return nil, fmt.Errorf("%w: args[2] path: %w", ErrMalformedArgs, err)
	}
	pathVec, err := scval.AsVec(pathSv)
	if err != nil {
		return nil, fmt.Errorf("%w: args[2] path: %w", ErrMalformedArgs, err)
	}
	if len(pathVec) < 2 {
		return nil, fmt.Errorf("%w: path len=%d (want >= 2)", ErrMalformedArgs, len(pathVec))
	}
	path := make([]string, len(pathVec))
	for i, sv := range pathVec {
		s, err := scval.AsAddressStrkey(sv)
		if err != nil {
			return nil, fmt.Errorf("%w: path[%d]: %w", ErrMalformedArgs, i, err)
		}
		path[i] = s
	}

	// Position 3: Address `to` (recipient).
	toSv, err := scval.Parse(rawTo)
	if err != nil {
		return nil, fmt.Errorf("%w: args[3] to: %w", ErrMalformedArgs, err)
	}
	to, err := scval.AsAddressStrkey(toSv)
	if err != nil {
		return nil, fmt.Errorf("%w: args[3] to: %w", ErrMalformedArgs, err)
	}

	// Position 4: u64 deadline. Unix-seconds.
	dlSv, err := scval.Parse(rawDeadline)
	if err != nil {
		return nil, fmt.Errorf("%w: args[4] deadline: %w", ErrMalformedArgs, err)
	}
	deadline, err := scval.AsU64(dlSv)
	if err != nil {
		return nil, fmt.Errorf("%w: args[4] deadline: %w", ErrMalformedArgs, err)
	}

	// deadline == 0 is a "no deadline" sentinel, not a 1970 expiry: leave the zero time.Time so the
	// sink's IsZero() guard NULLs the column.
	var deadlineTs time.Time
	if deadline != 0 {
		// Guards the int64 cast: a deadline near math.MaxUint64 would wrap to a bogus near-epoch value.
		// The sink NULLs anything still outside timestamptz range.
		if t, ok := canonical.UnboundedUnixSeconds(deadline); ok {
			deadlineTs = t
		}
	}

	// Map (a0, a1) → (AmountIn, AmountOut). These are the DECLARED call args: exactly one side is the
	// exact amount, the other a slippage BOUND (amount_out_min floor for exact-in, amount_in_max ceiling
	// for exact-out). Realized per-hop amounts come from the same tx's SoroswapPair("swap") events.
	amountIn, amountOut := a0, a1 // exact_tokens_for_tokens default
	if fnName == FnSwapTokensForExactTokens {
		amountIn, amountOut = a1, a0
	}

	callDepth, callKind := callPosition(callPath)

	return &RouterSwap{
		Source:     SourceName,
		Ledger:     ledger,
		ClosedAt:   closedAt,
		TxHash:     txHash,
		OpIndex:    opIndex,
		OpSource:   opSource,
		TxSource:   txSource,
		ContractID: contractID,
		Function:   fnName,
		Recipient:  to,
		Path:       path,
		AmountIn:   amountIn,
		AmountOut:  amountOut,
		DeadlineTs: deadlineTs,
		CallPath:   callPath,
		CallDepth:  callDepth,
		CallKind:   callKind,
	}, nil
}

// callPosition derives (CallDepth, CallKind) from the ancestor+self contract chain, which always ends
// in this call's contract (so top-level is len 1, depth 0). An empty callPath is treated as top-level.
func callPosition(callPath []string) (int, string) {
	depth := 0
	if n := len(callPath); n > 1 {
		depth = n - 1
	}
	if depth > 0 {
		return depth, CallKindSubInvocation
	}
	return depth, CallKindTopLevel
}

// parseI128 chains Parse → AsAmountFromI128 so decodeRouterArgs' per-arg error paths stay one line.
func parseI128(b64 string) (canonical.Amount, error) {
	sv, err := scval.Parse(b64)
	if err != nil {
		return canonical.Amount{}, err
	}
	return scval.AsAmountFromI128(sv)
}
