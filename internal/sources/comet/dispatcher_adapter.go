package comet

import (
	"errors"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/consumer"
	"github.com/Stellar-Index/StellarIndex/internal/contractid"
	"github.com/Stellar-Index/StellarIndex/internal/events"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
)

// Decoder is the dispatcher-facing view of Comet. Single instance per indexer. Comet uses a shared
// ("POOL", <event_name>) topic namespace across every pool contract, so ROUTING is by topic bytes
// but ATTRIBUTION is gated on contract identity (ADR-0035/0040): any contract deployed from (or
// mimicking) the Balancer-v1 WASM emits the same topic shape, and without the gate a look-alike
// could inject fabricated trades under `source = "comet"`.
//
// No creation event announces new pools, so the gate is the curated set (ADR-0040 section 1,
// mechanism 2/3): the in-code MainnetGatedSet is the trust root and the protocol_contracts warm
// is the operator seam for admitting a pool without a redeploy. No WASM-hash sweep admits pools.
//
// Claims the five POOL events from a registered pool: swap (TradeEvent) and join_pool /
// exit_pool / deposit / withdraw (LiquidityEvent). BPT transfers use the SEP-41 surface
// (sep41_supply), not the POOL namespace.
type Decoder struct {
	reg *contractid.Registry

	// now sources wall-clock for the self-pair metric's recency gate only; decoded timestamps
	// come from the ledger (EventClosedAt). Injectable for deterministic tests.
	now func() time.Time

	// countMetrics gates the exploit-detection counters (obs.AMMSelfPairSwapTotal /
	// obs.AMMNonPositiveSwapTotal). The dispatcher and the projector (ADR-0032 Phase-3 parallel
	// double-write) each build a Decoder over the same live events, deduped at the DB by ON
	// CONFLICT DO NOTHING; without this gate both would count one detection twice. The projector
	// registry disables it via [Decoder.WithoutMetrics] so the dispatcher's instance stays the
	// single canonical counter.
	countMetrics bool
}

// selfPairLiveWindow bounds how recent a self-pair swap's ledger close time must be to bump the
// exploit-detection metric. Backfill re-ingest and the completeness re-derive re-run Decode over
// the historical exploit window, and counting there would re-fire the burst alert for a
// non-event. The window is generous: live decode lags by seconds, and a >1h tip lag pages on
// its own.
const selfPairLiveWindow = time.Hour

// NewDecoder constructs a Comet Decoder. The in-code MainnetGatedSet is installed first; caller
// opts (the protocol_contracts warm) layer on top. There are no factories: pools have no
// creation event to self-register from.
func NewDecoder(opts ...contractid.Option) *Decoder {
	base := []contractid.Option{contractid.WithSeed(MainnetGatedSet())}
	return &Decoder{reg: contractid.New(append(base, opts...)...), now: time.Now, countMetrics: true}
}

// WithoutMetrics disables this Decoder's exploit-detection counters; see countMetrics.
func (d *Decoder) WithoutMetrics() *Decoder {
	d.countMetrics = false
	return d
}

// bumpDroppedSwapMetric counts a determinate zero-row swap drop (self-pair or non-positive
// amount), gated on countMetrics and close-time recency (see selfPairLiveWindow).
func (d *Decoder) bumpDroppedSwapMetric(counter *prometheus.CounterVec, closedAt time.Time) {
	if d.countMetrics && d.now().Sub(closedAt) < selfPairLiveWindow {
		counter.WithLabelValues(SourceName).Inc()
	}
}

// Name implements [dispatcher.Decoder].
func (d *Decoder) Name() string { return SourceName }

// Matches implements [dispatcher.Decoder]. Gates on CONTRACT IDENTITY, not topic bytes
// (ADR-0035/0040): the bare ("POOL", <event>) tuple is shared by every deployment of the
// Balancer-v1 code, so it is forgeable. A comet-shaped event from an unregistered contract is
// left for the recognition audit (ADR-0033 Claim 2a): visible, never silently attributed.
func (d *Decoder) Matches(ev events.Event) bool {
	if classify(&ev) == "" {
		return false
	}
	return d.reg.Has(ev.ContractID)
}

// Decode implements [dispatcher.Decoder]. It returns one consumer.Event on success: TradeEvent
// for swap, LiquidityEvent for the other four kinds. A decode error is non-fatal: counted and
// skipped.
//
// Decode is shape-only (no registry lookup); the identity gate lives in Matches, which the
// dispatcher consults first. Direct callers (tests, fixture tooling) bypass it by design.
func (d *Decoder) Decode(ev events.Event) ([]consumer.Event, error) {
	kind := classify(&ev)
	if kind == "" {
		return nil, ErrNotCometEvent
	}

	closedAt, err := ev.EventClosedAt()
	if err != nil {
		// Fail closed rather than substituting time.Now(): during a backfill replay that would
		// stamp every event with the replay run's wall-clock, not the historical ledger time.
		return nil, err
	}

	if kind == EventSwap {
		trade, err := decodeSwap(&ev, closedAt)
		if err != nil {
			// Determinate "not a serveable trade": the body decoded cleanly but maps to ZERO rows
			// (a self-pair swap, token_in == token_out, which is canonical.NewPair's
			// ErrPairMismatch, or non-positive amounts). Return no output and NO error so the
			// completeness re-derive counts expected=0 instead of an undecodable blind spot,
			// which would hold `comet` at complete=false forever. Safe because the body fully
			// decoded, so nothing is hidden; the error path stays for indeterminate parse
			// failures, which must remain blind.
			if errors.Is(err, canonical.ErrPairMismatch) {
				// Self-pair swap is the exploit primitive. These rows never reach the served
				// `trades` table, so this counter is the only place the self-swap burst is
				// observable. Detection only: it changes no serving or freeze decision. The
				// recency gate keeps backfill/re-derive from re-firing the alert.
				d.bumpDroppedSwapMetric(obs.AMMSelfPairSwapTotal, closedAt)
				return nil, nil
			}
			if errors.Is(err, ErrNonPositiveAmounts) {
				// Same drop shape and recency gate as the self-pair branch: tools that replay
				// historical ledgers build their own un-disabled Decoder, and an ungated counter
				// would be inflated by every completeness sweep.
				d.bumpDroppedSwapMetric(obs.AMMNonPositiveSwapTotal, closedAt)
				return nil, nil
			}
			return nil, err
		}
		return []consumer.Event{TradeEvent{Trade: trade}}, nil
	}

	// All other recognised kinds are liquidity events.
	liq, err := decodeLiquidityEvent(&ev, closedAt)
	if err != nil {
		return nil, err
	}
	return []consumer.Event{liq}, nil
}
