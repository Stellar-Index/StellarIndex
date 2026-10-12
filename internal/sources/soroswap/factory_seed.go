package soroswap

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/scval"
	"github.com/Stellar-Index/StellarIndex/internal/stellarrpc"
)

// seedThrottle is the delay between simulateTransaction calls in a factory sweep: public stellar-rpc endpoints behind
// Cloudflare return 429 above ~3-5 req/s even for cached responses; 300ms stays under that.
const seedThrottle = 300 * time.Millisecond

// Per-call retry budget for the sweep's ~640 sequential simulateTransaction calls against a PUBLIC endpoint.
// compute-completeness and verify-reconciliation fail CLOSED on a seed error, so one dropped connection or 429 would abort
// the nightly pass for every source. seedMaxAttempts is TOTAL tries per call; the wait before retry k is seedBackoffBase <<
// (k-1). The budget is per CALL, bounded overall by the caller's context (15 min).
const (
	seedMaxAttempts = 5
	seedBackoffBase = time.Second
)

// seedSleep waits d or until ctx is done, whichever is first. A package
// var only so tests can record the schedule instead of sleeping it.
var seedSleep = sleepCtx

// sleepCtx blocks for d, returning ctx.Err() early if ctx is cancelled
// or its deadline passes first.
func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// SeedFromFactoryRPC populates the pair->(token0, token1) registry from the factory's on-chain state via read-only simulateTransaction (~3N+1 calls); cold-start only, since the live path records every new_pair but pairs created before the first ledger are invisible. Transient RPC failures are retried per call; it returns the pairs seeded and an error on any failure, and the caller decides fail-closed vs fail-open. Idempotent.
func (d *Decoder) SeedFromFactoryRPC(ctx context.Context, rpc *stellarrpc.Client, factoryContract string) (int, error) {
	length, err := callU32(ctx, rpc, factoryContract, "all_pairs_length", nil)
	if err != nil {
		return 0, fmt.Errorf("soroswap seed: all_pairs_length: %w", err)
	}

	seeded := 0
	for i := uint32(0); i < length; i++ {
		pairAddr, token0, token1, err := seedOnePair(ctx, rpc, factoryContract, i)
		if err != nil {
			return seeded, err
		}
		d.SeedPair(pairAddr, token0, token1)
		seeded++
	}
	return seeded, nil
}

// seedOnePair resolves pair i of the factory: its address and both token identities, via three throttled view calls
// (throttle waits honour ctx).
func seedOnePair(ctx context.Context, rpc *stellarrpc.Client, factoryContract string, i uint32) (pairAddr string, token0, token1 canonical.Asset, err error) {
	if err = seedSleep(ctx, seedThrottle); err != nil {
		return "", token0, token1, fmt.Errorf("soroswap seed: all_pairs(%d): %w", i, err)
	}
	pairAddr, err = callAddressStrkey(ctx, rpc, factoryContract, "all_pairs",
		[]scval.ScVal{scval.NewU32(i)})
	if err != nil {
		return "", token0, token1, fmt.Errorf("soroswap seed: all_pairs(%d): %w", i, err)
	}

	var tokenAddrs [2]string
	for n, fn := range [2]string{"token_0", "token_1"} {
		if err = seedSleep(ctx, seedThrottle); err != nil {
			return "", token0, token1, fmt.Errorf("soroswap seed: pair %s %s: %w", pairAddr, fn, err)
		}
		tokenAddrs[n], err = callAddressStrkey(ctx, rpc, pairAddr, fn, nil)
		if err != nil {
			return "", token0, token1, fmt.Errorf("soroswap seed: pair %s %s: %w", pairAddr, fn, err)
		}
	}

	token0, err = canonical.NewSorobanAsset(tokenAddrs[0])
	if err != nil {
		return "", token0, token1, fmt.Errorf("soroswap seed: pair %s token0 %s: %w", pairAddr, tokenAddrs[0], err)
	}
	token1, err = canonical.NewSorobanAsset(tokenAddrs[1])
	if err != nil {
		return "", token0, token1, fmt.Errorf("soroswap seed: pair %s token1 %s: %w", pairAddr, tokenAddrs[1], err)
	}
	return pairAddr, token0, token1, nil
}

// callU32 invokes a u32-returning view function via simulateTransaction.
func callU32(ctx context.Context, rpc *stellarrpc.Client, contract, fn string, args []scval.ScVal) (uint32, error) {
	sv, err := callView(ctx, rpc, contract, fn, args)
	if err != nil {
		return 0, err
	}
	return scval.AsU32(sv)
}

// callAddressStrkey invokes an Address-returning view function and
// returns the address as a C-strkey (contract) or G-strkey (account).
func callAddressStrkey(ctx context.Context, rpc *stellarrpc.Client, contract, fn string, args []scval.ScVal) (string, error) {
	sv, err := callView(ctx, rpc, contract, fn, args)
	if err != nil {
		return "", err
	}
	return scval.AsAddressStrkey(sv)
}

// callView builds the envelope, submits it (retrying transient round-trip failures, see [simulateWithRetry]) and parses the
// result SCVal. Everything after the round-trip is deterministic and never retried.
func callView(ctx context.Context, rpc *stellarrpc.Client, contract, fn string, args []scval.ScVal) (scval.ScVal, error) {
	b64, err := stellarrpc.InvokeContractTxEnvelope("", contract, fn, args)
	if err != nil {
		return scval.ScVal{}, fmt.Errorf("build envelope: %w", err)
	}
	resp, err := simulateWithRetry(ctx, rpc, b64)
	if err != nil {
		return scval.ScVal{}, fmt.Errorf("simulate: %w", err)
	}
	if resp.Error != "" {
		return scval.ScVal{}, fmt.Errorf("simulate rejected: %s", resp.Error)
	}
	if len(resp.Results) == 0 {
		return scval.ScVal{}, fmt.Errorf("simulate returned no results")
	}
	return scval.Parse(resp.Results[0].XDR)
}

// simulateWithRetry issues one simulateTransaction, retrying transient failures up to seedMaxAttempts with exponential backoff.
// A non-[retryableSimulateErr] failure returns at once; a spent budget returns the LAST error wrapped with the attempt count
// (caller still fails closed); a cancelled ctx ends the loop immediately, including mid-backoff.
func simulateWithRetry(ctx context.Context, rpc *stellarrpc.Client, b64 string) (*stellarrpc.SimulationResponse, error) {
	for attempt := 1; ; attempt++ {
		resp, err := rpc.SimulateTransaction(ctx, b64)
		if err == nil {
			return resp, nil
		}
		if ctx.Err() != nil || !retryableSimulateErr(err) {
			return nil, err
		}
		if attempt >= seedMaxAttempts {
			return nil, fmt.Errorf("gave up after %d attempts: %w", attempt, err)
		}
		wait := seedBackoffBase << (attempt - 1)
		slog.Default().Warn("soroswap seed: transient RPC failure, retrying",
			"attempt", attempt, "max_attempts", seedMaxAttempts, "backoff", wait, "err", err)
		if serr := seedSleep(ctx, wait); serr != nil {
			return nil, fmt.Errorf("retry abandoned after %d attempt(s): %w (last error: %w)", attempt, serr, err)
		}
	}
}

// JSON-RPC 2.0 error codes the classification names: -32603 is "Internal error"; -32000..-32099 is the implementation-defined
// SERVER range where hosted providers put rate-limit/unavailable. Other reserved codes describe the REQUEST (deterministic).
const (
	jsonRPCInternalError  = -32603
	jsonRPCServerErrorMin = -32099
	jsonRPCServerErrorMax = -32000
)

// retryableSimulateErr reports whether a failed simulateTransaction round-trip is worth re-issuing, classifying on TYPED
// stellarrpc errors, never message text. HTTP status >= 400 decides alone (408, 429, 5xx retry; other 4xx fail at once; it
// outranks an envelope code). On status < 400: a JSON-RPC envelope retries for -32603, -32000..-32099 and 408/429/5xx; an
// undecodable body (empty, cut short, proxy page) retries; transport errors retry. Contract-level rejections arrive as a
// successful round-trip with SimulationResponse.Error set and never reach here.
func retryableSimulateErr(err error) bool {
	var statusErr *stellarrpc.HTTPStatusError
	if errors.As(err, &statusErr) {
		return retryableStatusCode(statusErr.StatusCode)
	}
	var rpcErr *stellarrpc.JSONRPCError
	if errors.As(err, &rpcErr) {
		return rpcErr.Code == jsonRPCInternalError ||
			(rpcErr.Code >= jsonRPCServerErrorMin && rpcErr.Code <= jsonRPCServerErrorMax) ||
			retryableStatusCode(rpcErr.Code)
	}
	var decodeErr *stellarrpc.ResponseDecodeError
	if errors.As(err, &decodeErr) {
		return true
	}
	var netErr net.Error
	return errors.As(err, &netErr) || errors.Is(err, io.ErrUnexpectedEOF)
}

// retryableStatusCode reports whether an HTTP status (or a JSON-RPC code borrowed from HTTP) is transient: request timeout,
// rate limit, or any server-side failure.
func retryableStatusCode(code int) bool {
	return code == http.StatusRequestTimeout || code == http.StatusTooManyRequests || (code >= 500 && code <= 599)
}
