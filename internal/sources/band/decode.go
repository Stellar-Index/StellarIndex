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

// bandMaxFutureResolveTime mirrors the contract's relay() window (resolve_time < ledger
// timestamp + 3600s). A relay outside it is a silent on-chain no-op in a successful tx, so
// the decoder must apply the same bound or it records prices the chain refused.
const bandMaxFutureResolveTime = time.Hour

// safeUnixEpochFloorSeconds mirrors canonical.SafeUnixSeconds's unexported year-2001 floor.
// Band needs the raw comparison, not the clamped result, to tell garbage apart from a
// resolve_time that legitimately equals the ledger close.
const safeUnixEpochFloorSeconds = 1_000_000_000

// decodeRelayArgs converts one relay/force_relay call into one canonical.OracleUpdate per
// symbol_rates pair. fnName selects the shape:
//
//   - relay:       args = [from, symbol_rates, resolve_time, request_id]
//   - force_relay: args = [symbol_rates, resolve_time, request_id]
//
// Updates share (ledger, tx_hash) and get distinct OpIndex values from the vector position.
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
	if len(pairs) == 0 {
		// A well-formed call with an empty symbol_rates writes nothing
		// on-chain: a recognised no-op, not a decode error that blinds the ledger.
		return nil, nil
	}
	// resolve_time is a relayer-supplied u64; SafeUnixSeconds bound-checks the RAW value
	// (catching the >MaxInt64 wrap class) and falls back to the ledger close on garbage.
	ts := canonical.SafeUnixSeconds(resolveSeconds, closedAt)
	// relay() (not force_relay) silently no-ops outside its window though the tx succeeds.
	// Writing such a rate would let one the chain never applied win our `ORDER BY ts DESC`
	// latest-read, so drop it (bandRelayWouldNoOp). We can't see the per-symbol stored
	// resolve_time the contract also gates on, so a genuine first write for a symbol with no
	// live RefData entry is dropped too: an accepted false-negative over serving a rejected
	// rate. Both surface via obs.SourceDecodeErrorsTotal{source="band"}. force_relay is the
	// unconditional admin path and keeps the clamp-to-close fallback below. Applied after the
	// symbol_rates loop so a malformed pair still surfaces its own ErrMalformedArgs.
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
		// USD is fixed at 1 (E9) in Band's storage, not relayer-set; a pushed USD write is
		// rejected on-chain, so emitting an update for it would be false signal.
		if sym == "USD" {
			continue
		}
		asset, err := symbolToAsset(sym)
		if err != nil {
			return nil, fmt.Errorf("%w: symbol_rates[%d] %q: %w", ErrMalformedArgs, i, sym, err)
		}
		if !asset.IsMapped() {
			// An unmapped symbol is recorded verbatim as raw:<symbol> at this vector slot,
			// never skipped. Still count it: a raw row is a mapping gap the allow-list owner
			// must close.
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
		// Only reachable when every slot was USD or rate 0: an unmapped
		// symbol is a raw row, not a skip, so an all-unknown vector does not
		// land here.
		return nil, ErrEmptyRates
	}
	if relayRejected {
		return nil, fmt.Errorf("%w: resolve_time %d outside Band's acceptance window around close %v",
			ErrEmptyRates, resolveSeconds, closedAt)
	}
	return out, nil
}

// symbolToAsset maps a Band symbol (crypto tickers and fiat codes share symbol_rates) to a
// canonical.Asset via canonical.MapOracleSymbol, the one precedence shared by every oracle
// decoder. Unknown symbols come back as raw:<symbol> rather than dropped.
func symbolToAsset(sym string) (canonical.Asset, error) {
	return canonical.MapOracleSymbol(sym)
}

// bandRelayWouldNoOp reports whether relay() would silently reject resolveSeconds. It checks
// the RAW value: SafeUnixSeconds's 24h ceiling is looser than Band's 1h one, so a clamped
// value would let an out-of-window rate through.
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
