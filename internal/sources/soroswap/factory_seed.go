package soroswap

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"regexp"
	"strconv"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/scval"
	"github.com/Stellar-Index/StellarIndex/internal/stellarrpc"
)

// seedThrottle is the delay inserted between simulateTransaction
// calls during a factory sweep. Public stellar-rpc endpoints (e.g.
// mainnet.sorobanrpc.com behind Cloudflare) return 429 above
// ~3-5 req/s even on cached simulate responses. 300ms keeps us
// comfortably under that. For a 200-pair factory the full sweep
// takes ~3min of wall time — acceptable at boot and verify time.
// Behind an unthrottled endpoint this is slow; a follow-up could
// parallelise across a small worker pool or swap for paid-tier
// throughput.
const seedThrottle = 300 * time.Millisecond

// Per-call retry budget for the sweep's simulateTransaction round-trips.
//
// The sweep is 1+3N sequential calls (N ≈ 214 pairs on pubnet, so ~640
// calls) against what is, on r1, a PUBLIC third-party endpoint — the
// host runs no stellar-rpc of its own. compute-completeness and
// verify-reconciliation fail CLOSED on a seed error (RLT-416), so
// without a retry a single dropped connection or 429 anywhere in those
// ~640 calls aborts the nightly pass for every source. Fail-closed is
// the right outcome for an endpoint that is down; it must not be the
// outcome of one blip.
//
// seedMaxAttempts is the TOTAL number of tries per call (1 + retries).
// The wait before retry k (k = 1..seedMaxAttempts-1) is
// seedBackoffBase << (k-1): 1s, 2s, 4s, 8s.
//
// Worst-case ADDED wall time, from these constants:
//   - per call: 15s of backoff (1+2+4+8) plus up to four extra attempt
//     durations, each bounded by the client's own HTTP timeout;
//   - per sweep: a call that spends its budget ends the sweep, so the
//     only unbounded shape is "every call recovers on its last try",
//     and that is bounded by the CALLER's context (15 min in the recon
//     and verify-decoders callers). The typical cost of one blip is 1s.
//
// The budget is per CALL, never per sweep: a retry re-issues the one
// failed view call, it does not restart the factory walk.
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

// SeedFromFactoryRPC populates the Decoder's pair→(token0, token1)
// registry by reading the Soroswap factory's on-chain state via
// stellar-rpc simulateTransaction.
//
// The flow is the three-step sweep the factory exposes via view
// functions:
//
//  1. factory.all_pairs_length() -> u32  → N
//  2. factory.all_pairs(i) -> Address     → pair_i for i in [0, N)
//  3. pair_i.token_0() + pair_i.token_1() -> Address → token identities
//
// Each simulateTransaction round-trip runs the contract function
// locally on the RPC node and returns the SCVal result; no ledger
// state is changed, no fee is paid. Typical factory size on pubnet
// is a few hundred pairs, so the full sweep is ~3N+1 RPC calls —
// seconds of wall time at mild concurrency.
//
// Cold-start use case: the live dispatcher path records every future
// new_pair event on the fly (see Decoder.SeedPair, fired from Decode
// on each factory new_pair event), but pairs created BEFORE the
// dispatcher's first ledger are invisible to live events. This method
// fills that gap. Once seeded, the live path keeps the registry in
// sync automatically.
//
// factoryContract is the C-strkey of the Soroswap factory. For
// mainnet that's CA4HEQTL2WPEUYKYKCDOHCDNIV4QHNJ7EL4J4NQ6VADP7SYHVRYZ7AW2.
//
// Transient RPC failures (transport, HTTP 429/5xx, JSON-RPC internal
// error) are retried per call within a bounded budget — see
// seedMaxAttempts and [retryableSimulateErr]. Once a call's budget is
// spent, or on any deterministic failure, the sweep stops.
//
// Returns the number of pairs seeded and a non-nil error on any
// failure — the caller decides whether to fail-closed (refuse to
// start the dispatcher) or fail-open (log and continue with an
// empty registry, accept silent swaps). Idempotent: seeded entries
// overwrite existing ones, so re-running is safe.
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

// callView is the common simulateTransaction machinery: builds the
// envelope, submits (retrying transient round-trip failures — see
// [simulateWithRetry]), parses the result SCVal. Everything after the
// round-trip is deterministic and is never retried: a contract that
// rejects the call, an empty result set, an unparseable SCVal.
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

// simulateWithRetry issues one simulateTransaction, re-issuing it on a
// transient failure up to seedMaxAttempts total tries with exponential
// backoff (see the constants for the schedule and the worst case).
//
// A failure that is not [retryableSimulateErr] is returned at once,
// unchanged. A spent budget returns the LAST error wrapped with the
// attempt count, so the caller still fails closed and still sees the
// cause. A cancelled or expired ctx ends the loop immediately — both
// before a retry and in the middle of a backoff wait.
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

// jsonRPCInternalError is the JSON-RPC 2.0 reserved "Internal error" code.
const jsonRPCInternalError = -32603

// simulateHTTPStatus extracts the status from the stellarrpc client's
// HTTP-failure errors ("stellarrpc: <method>: HTTP <code>…"). The client
// does not expose the status as a typed error, so the text is the only
// carrier. The match is anchored to the client's own prefix so a status
// quoted inside an upstream body cannot be mistaken for it, and the
// retry tests drive the REAL client, so a change to that format turns
// them red instead of silently disabling the retry.
var simulateHTTPStatus = regexp.MustCompile(`^stellarrpc: [A-Za-z]+: HTTP (\d{3})\b`)

// retryableSimulateErr reports whether a failed simulateTransaction
// round-trip is worth re-issuing. Only three classes are:
//
//   - transport: the request never completed (dial / reset / TLS /
//     timeout — what http.Client.Do returns is always a net.Error) or
//     the body was cut short;
//   - HTTP 429 and 5xx from the endpoint or a proxy in front of it;
//   - JSON-RPC -32603 "internal error" — the envelope-level analogue of
//     a 5xx, and what the client returns INSTEAD of the HTTP status
//     when a 5xx carries a JSON error body.
//
// Everything else is treated as deterministic and fails at once: any
// other JSON-RPC error (invalid params / request, method not found),
// any other 4xx (auth, bad request, not found), an undecodable 2xx
// body. Contract-level rejections never reach here — they arrive as a
// successful round-trip with SimulationResponse.Error set.
func retryableSimulateErr(err error) bool {
	var rpcErr *stellarrpc.JSONRPCError
	if errors.As(err, &rpcErr) {
		return rpcErr.Code == jsonRPCInternalError
	}
	var netErr net.Error
	if errors.As(err, &netErr) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	if m := simulateHTTPStatus.FindStringSubmatch(err.Error()); m != nil {
		code, _ := strconv.Atoi(m[1])
		return code == 429 || code >= 500
	}
	return false
}
