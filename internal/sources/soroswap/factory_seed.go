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

// seedThrottle is the delay between simulateTransaction calls in a factory sweep. Public
// stellar-rpc endpoints behind Cloudflare return 429 above ~3-5 req/s even for cached responses;
// 300ms stays under that, so a 200-pair sweep takes ~3min.
const seedThrottle = 300 * time.Millisecond

// Per-call retry budget for the sweep's simulateTransaction round-trips.
//
// The sweep is 1+3N sequential calls (~640 on pubnet) against a PUBLIC third-party endpoint.
// compute-completeness and verify-reconciliation fail CLOSED on a seed error, so without a retry
// one dropped connection or 429 would abort the nightly pass for every source. Fail-closed is
// right for a down endpoint, not for one blip.
//
// seedMaxAttempts is the TOTAL tries per call; the wait before retry k is seedBackoffBase <<
// (k-1): 1s, 2s, 4s, 8s. The budget is per CALL, not per sweep, and is bounded overall by the
// caller's context (15 min).
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

// SeedFromFactoryRPC populates the Decoder's pair->(token0, token1) registry from the factory's
// on-chain state via read-only simulateTransaction: all_pairs_length(), all_pairs(i), then
// token_0()/token_1() per pair (~3N+1 calls).
//
// Cold-start only: the live path records every new_pair event (Decoder.SeedPair), but pairs
// created BEFORE the dispatcher's first ledger are invisible to it.
//
// factoryContract is the factory C-strkey (mainnet
// CA4HEQTL2WPEUYKYKCDOHCDNIV4QHNJ7EL4J4NQ6VADP7SYHVRYZ7AW2). Transient RPC failures are retried per
// call (see seedMaxAttempts and [retryableSimulateErr]); a spent budget or deterministic failure
// stops the sweep.
//
// It returns the pairs seeded and an error on any failure; the caller decides between fail-closed
// and fail-open (empty registry, silent swaps accepted). Idempotent: seeded entries overwrite.
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

// seedOnePair resolves pair i of the factory: its address and both
// token identities. Three throttled view calls; the throttle waits
// honour ctx like the retry backoff does.
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

// callView builds the envelope, submits it (retrying transient round-trip failures, see
// [simulateWithRetry]) and parses the result SCVal. Everything after the round-trip is
// deterministic and never retried: a contract rejection, an empty result set, an unparseable SCVal.
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

// simulateWithRetry issues one simulateTransaction, retrying transient failures up to
// seedMaxAttempts tries with exponential backoff.
//
// A failure that is not [retryableSimulateErr] is returned at once, unchanged. A spent budget
// returns the LAST error wrapped with the attempt count, so the caller still fails closed and
// sees the cause. A cancelled ctx ends the loop immediately, including mid-backoff.
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

// JSON-RPC 2.0 error codes the classification names. -32603 is "Internal error"; -32000..-32099 is
// the implementation-defined SERVER range where hosted providers put "rate limit exceeded" and
// "upstream unavailable". The other reserved codes describe the REQUEST and are deterministic.
const (
	jsonRPCInternalError  = -32603
	jsonRPCServerErrorMin = -32099
	jsonRPCServerErrorMax = -32000
)

// retryableSimulateErr reports whether a failed simulateTransaction round-trip is worth
// re-issuing. It classifies on the TYPED stellarrpc errors, never message text. In order:
//
//  1. HTTP status >= 400 decides alone, whatever the body: 408, 429 and 5xx retry; other 4xx fail
//     at once. Status outranks an envelope code because it is the proxy or rate limiter speaking.
//  2. A JSON-RPC error envelope on status < 400 retries for -32603, -32000..-32099 and HTTP-style
//     codes (408, 429, 5xx); others fail at once.
//  3. A body that does not decode as an envelope on status < 400 (empty, cut short, a proxy page
//     served as 200) retries; a deterministic one costs only the bounded budget.
//  4. Transport errors: the request never completed, or the body was cut short.
//
// Anything else fails at once. Contract-level rejections arrive as a successful round-trip with
// SimulationResponse.Error set and never reach here.
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

// retryableStatusCode reports whether an HTTP status (or a JSON-RPC code
// a provider borrowed from HTTP) names a transient condition: request
// timeout, rate limit, or any server-side failure.
func retryableStatusCode(code int) bool {
	return code == http.StatusRequestTimeout || code == http.StatusTooManyRequests || (code >= 500 && code <= 599)
}
