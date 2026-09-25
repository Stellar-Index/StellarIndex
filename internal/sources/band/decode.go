package band

import (
	"fmt"
	"math/big"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/scval"
)

// opIndexFanoutStride is the synthetic OpIndex spacing per Band
// call, analogous to the Reflector / Redstone strides. Band's
// symbol_rates vector in practice is small (one batch per relayer
// submission), well under 1024.
const opIndexFanoutStride = 1024

// bandMaxFutureResolveTime mirrors the Band contract's own acceptance
// window: relay() applies an update only while
// `resolve_time < ledger.timestamp + OFFSET`, OFFSET = 3600 seconds
// (ref_data.rs). A relay outside it is a silent on-chain no-op that
// still leaves a successful transaction, so the decoder has to apply
// the same bound or it records prices the chain refused.
const bandMaxFutureResolveTime = time.Hour

// safeUnixEpochFloorSeconds mirrors canonical.SafeUnixSeconds's own
// pre-2001 floor (unexported there). A raw resolve_time below it is
// garbage SafeUnixSeconds itself would have clamped to closedAt; band
// needs the raw comparison directly (not just the clamped result) to
// tell "garbage" apart from a resolve_time that legitimately equals
// the ledger close.
const safeUnixEpochFloorSeconds = 1_000_000_000

// decodeRelayArgs converts one Band relay/force_relay InvokeContract
// call into a slice of canonical.OracleUpdate — one per (symbol,
// rate) pair in symbol_rates.
//
// fnName selects between the two function shapes:
//
//   - relay:       args = [from, symbol_rates, resolve_time, request_id]
//   - force_relay: args = [symbol_rates, resolve_time, request_id]
//
// Updates share (ledger, tx_hash) but get distinct OpIndex values
// derived from the vector position, mirroring the Reflector /
// Redstone fan-out.
func decodeRelayArgs( //nolint:gocognit,gocyclo,funlen // dispatch-heavy; splitting would reduce linearity
	fnName string,
	args []string,
	contractID string,
	ledger uint32,
	txHash string,
	opIndex int,
	opSource, txSource string,
	closedAt time.Time,
) ([]canonical.OracleUpdate, error) {
	var ratesIdx, timeIdx int
	var relayerFrom string // observer strkey

	switch fnName {
	case FnRelay:
		if len(args) < 4 {
			return nil, fmt.Errorf("%w: relay expects 4 args, got %d", ErrMalformedArgs, len(args))
		}
		// args[0] = from: Address
		fromSv, err := scval.Parse(args[0])
		if err != nil {
			return nil, fmt.Errorf("%w: args[0] from: %w", ErrMalformedArgs, err)
		}
		relayerFrom, err = scval.AsAddressStrkey(fromSv)
		if err != nil {
			return nil, fmt.Errorf("%w: args[0] from: %w", ErrMalformedArgs, err)
		}
		ratesIdx, timeIdx = 1, 2
	case FnForceRelay:
		if len(args) < 3 {
			return nil, fmt.Errorf("%w: force_relay expects 3 args, got %d", ErrMalformedArgs, len(args))
		}
		// No `from` arg — admin-only path. Observer falls back to the
		// op source account (or tx source) so the row still carries
		// attribution rather than being anonymous.
		relayerFrom = pickObserver(opSource, txSource)
		ratesIdx, timeIdx = 0, 1
	default:
		return nil, ErrNotBandCall
	}

	// args[ratesIdx] = symbol_rates: Vec<(Symbol, u64)>.
	// Bound guaranteed by the len(args) check in the switch above
	// — FnRelay: len ≥ 4, ratesIdx = 1; FnForceRelay: len ≥ 3,
	// ratesIdx = 0. gosec can't trace the invariant across cases.
	ratesSv, err := scval.Parse(args[ratesIdx]) //nolint:gosec // bounds-checked in switch case above
	if err != nil {
		return nil, fmt.Errorf("%w: symbol_rates: %w", ErrMalformedArgs, err)
	}
	pairs, err := scval.AsVec(ratesSv)
	if err != nil {
		return nil, fmt.Errorf("%w: symbol_rates not a Vec: %w", ErrMalformedArgs, err)
	}
	if len(pairs) == 0 {
		return nil, ErrEmptyRates
	}
	if len(pairs) > opIndexFanoutStride {
		return nil, fmt.Errorf("band: symbol_rates length %d exceeds fanout stride %d",
			len(pairs), opIndexFanoutStride)
	}

	// args[timeIdx] = resolve_time: u64 (UNIX seconds).
	// Bound guaranteed by the len(args) check in the switch above
	// — FnRelay: len ≥ 4, timeIdx = 2; FnForceRelay: len ≥ 3,
	// timeIdx = 1. gosec can't trace the invariant across cases.
	timeSv, err := scval.Parse(args[timeIdx]) //nolint:gosec // bounds-checked in switch case above
	if err != nil {
		return nil, fmt.Errorf("%w: resolve_time: %w", ErrMalformedArgs, err)
	}
	resolveSeconds, err := scval.AsU64(timeSv)
	if err != nil {
		return nil, fmt.Errorf("%w: resolve_time: %w", ErrMalformedArgs, err)
	}
	// Defensive fallback: relayer-supplied resolve_time is a u64;
	// canonical.SafeUnixSeconds bound-checks the RAW u64 (pre-2001
	// floor + close+24h ceiling, catching the >MaxInt64 wrap class of
	// the router deadline_ts bug) and falls back to the ledger close
	// on garbage. Real-world Band payloads are post-2020 UNIX seconds
	// ≤ the close.
	ts := canonical.SafeUnixSeconds(resolveSeconds, closedAt)
	// relay() (not force_relay) applies an update only while
	// `resolve_time < ledger.timestamp + OFFSET` (ref_data.rs) — outside
	// that, the on-chain call is a silent no-op though the tx succeeds.
	// Clamping such a resolve_time to closedAt and still writing it let
	// a rate the chain never applied win our `ORDER BY ts DESC`
	// latest-read for up to one relay interval (previously up to 24h
	// before ts was clamped instead of left future-dated: cold audit
	// 2026-08-03). Drop it instead — see bandRelayWouldNoOp.
	//
	// We can't see the per-symbol stored resolve_time the contract also
	// gates on (on-chain state, not in the call args), so a genuine
	// first write for a symbol with no live RefData entry (which the
	// contract accepts with no resolve_time bound) is indistinguishable
	// from a rejected relay and gets dropped too — an accepted
	// false-negative over the false-positive of serving a rejected
	// rate. Both cases surface via the existing decode-error counter
	// (obs.SourceDecodeErrorsTotal{source="band"}).
	//
	// force_relay is the unconditional admin path with no such gate to
	// mirror, so it keeps the clamp-to-close fallback below.
	//
	// Checked here but applied after the symbol_rates loop below, so a
	// structurally malformed pair still surfaces its own ErrMalformedArgs
	// rather than being masked by this reject.
	relayRejected := fnName == FnRelay && bandRelayWouldNoOp(resolveSeconds, closedAt)
	if !ts.Before(closedAt.Add(bandMaxFutureResolveTime)) {
		ts = closedAt.UTC()
	}

	usdQuote, err := canonical.NewFiatAsset("USD")
	if err != nil {
		return nil, fmt.Errorf("band: USD fiat quote unavailable: %w", err)
	}

	out := make([]canonical.OracleUpdate, 0, len(pairs))
	for i, pair := range pairs {
		elts, err := scval.AsTupleN(pair, 2)
		if err != nil {
			return nil, fmt.Errorf("%w: symbol_rates[%d]: %w", ErrMalformedArgs, i, err)
		}
		sym, err := scval.AsSymbol(elts[0])
		if err != nil {
			return nil, fmt.Errorf("%w: symbol_rates[%d] symbol: %w", ErrMalformedArgs, i, err)
		}
		rate, err := scval.AsU64(elts[1])
		if err != nil {
			return nil, fmt.Errorf("%w: symbol_rates[%d] rate: %w", ErrMalformedArgs, i, err)
		}
		// USD is a special-case in Band's storage (always 1 at E9,
		// not relayer-set). We skip it in relay payloads — if a
		// relayer somehow pushes USD its storage write is rejected
		// on-chain and emitting an OracleUpdate would be false
		// signal. See band-soroban/src/storage/ref_data.rs:30-38.
		if sym == "USD" {
			continue
		}
		asset, err := symbolToAsset(sym)
		if err != nil {
			return nil, fmt.Errorf("%w: symbol_rates[%d] %q: %w", ErrMalformedArgs, i, sym, err)
		}
		if !asset.IsMapped() {
			// Oracle capture-totality (PR-2): a symbol outside the
			// allow-lists is RECORDED verbatim as raw:<symbol> at
			// this same vector slot, not skipped (same pattern as
			// Reflector / RedStone). F-1234 (codex audit-2026-05-12):
			// still count it so the unknown-symbols runbook signals
			// on upstream coverage drift — a raw row is a mapping
			// gap the allow-list owner has to close.
			obs.SourceUnknownSymbolsTotal.WithLabelValues("band").Inc()
		}
		if rate == 0 {
			continue
		}
		u := canonical.OracleUpdate{
			Source:     SourceName,
			ContractID: contractID,
			Ledger:     ledger,
			TxHash:     txHash,
			OpIndex:    uint32(opIndex)*opIndexFanoutStride + uint32(i),
			Timestamp:  ts,
			Asset:      asset,
			Quote:      usdQuote,
			Price:      canonical.NewAmount(new(big.Int).SetUint64(rate)),
			Decimals:   DefaultDecimals,
			Observer:   relayerFrom,
		}
		out = append(out, u)
	}
	if len(out) == 0 {
		// Only reachable when every slot was USD or rate 0: since the
		// oracle capture-totality change an unmapped symbol is a raw
		// row, not a skip, so an all-unknown vector no longer lands
		// here.
		return nil, ErrEmptyRates
	}
	if relayRejected {
		return nil, fmt.Errorf("%w: resolve_time %d outside Band's acceptance window around close %v",
			ErrEmptyRates, resolveSeconds, closedAt)
	}
	return out, nil
}

// symbolToAsset maps a Band symbol to a canonical.Asset. Band
// publishes a mix of crypto tickers (BTC, ETH, XLM …) and fiat
// codes (USD is special-cased above; EUR, JPY, ...) via the same
// symbol_rates channel. canonical.MapOracleSymbol tries fiat
// (ADR-0010), then crypto (ADR-0014), then RWA (ADR-0028) — the one
// shared precedence for every oracle decoder (Band used to try crypto
// before fiat; the lists are disjoint so no row changes type) — and
// returns a verbatim raw:<symbol> asset for anything else, so the
// slot is recorded rather than dropped. The only error is a symbol
// the raw validator cannot represent (impossible for an ScSymbol).
func symbolToAsset(sym string) (canonical.Asset, error) {
	return canonical.MapOracleSymbol(sym)
}

// bandRelayWouldNoOp reports whether the Band contract's relay()
// would silently reject resolveSeconds outside its own OFFSET window
// (ref_data.rs: `resolve_time < ledger.timestamp + OFFSET`), checked
// on the RAW value rather than canonical.SafeUnixSeconds's
// already-clamped result — that helper's 24h ceiling is looser than
// Band's 1h one, so a raw value inside canonical's window but outside
// Band's would otherwise slip through unrejected.
func bandRelayWouldNoOp(resolveSeconds uint64, closedAt time.Time) bool {
	if resolveSeconds < safeUnixEpochFloorSeconds {
		return true
	}
	ceil := closedAt.Add(bandMaxFutureResolveTime).Unix()
	if ceil < 0 {
		// Mirrors canonical.SafeUnixSeconds's own pre-1970 guard: no
		// production closedAt is ever this old, but fail toward
		// rejecting rather than let an unchecked uint64 cast wrap the
		// ceiling and disable the check.
		return true
	}
	// Strict, like the contract: resolve_time == close + OFFSET is refused.
	return resolveSeconds >= uint64(ceil)
}

// pickObserver returns the best-effort attribution strkey for a
// force_relay call (which has no `from` arg). Op source wins if
// the op carries its own source account; otherwise the tx source.
// Empty string is acceptable — OracleUpdate.Observer is optional.
func pickObserver(opSource, txSource string) string {
	if opSource != "" {
		return opSource
	}
	return txSource
}
