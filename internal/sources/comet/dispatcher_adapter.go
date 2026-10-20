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

// Decoder is the dispatcher-facing view of Comet (one per indexer). Routing is by the shared ("POOL", <event_name>) topic but attribution is gated on contract identity (ADR-0035/0040), since any Balancer-v1 look-alike emits the same topics and could inject fabricated trades. No creation event announces pools, so MainnetGatedSet is the trust root and the protocol_contracts warm admits a pool without a redeploy. BPT transfers use the SEP-41 surface.
type Decoder struct {
	reg *contractid.Registry

	// now sources wall-clock for the self-pair metric's recency gate only; decoded timestamps
	// come from the ledger (EventClosedAt). Injectable for deterministic tests.
	now func() time.Time

	// countMetrics gates the exploit-detection counters. The dispatcher and the projector both
	// decode the same live events (the DB dedupes rows, not counters), so the projector disables
	// it via [Decoder.WithoutMetrics] to avoid counting one detection twice.
	countMetrics bool
}

// selfPairLiveWindow bounds how recent a dropped swap's close time must be to bump the metric.
// Backfill and the completeness re-derive re-run Decode over historical exploits and would
// re-fire the burst alert. Generous: live decode lags by seconds, and a >1h tip lag pages itself.
const selfPairLiveWindow = time.Hour

// NewDecoder constructs a Comet Decoder with the in-code MainnetGatedSet installed first and caller opts (the protocol_contracts warm) on top; there are no factories since pools have no creation event.
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

// Matches implements [dispatcher.Decoder], gating on contract identity, not topic: the bare ("POOL", <event>) tuple is shared by every Balancer-v1 deployment and so forgeable. Events from unregistered contracts stay visible to the recognition audit, never attributed.
func (d *Decoder) Matches(ev events.Event) bool {
	if classify(&ev) == "" {
		return false
	}
	return d.reg.Has(ev.ContractID)
}

// Decode implements [dispatcher.Decoder]: a TradeEvent for swap, a LiquidityEvent for the other four kinds; a decode error is counted and skipped. Shape-only (no registry lookup): the identity gate is Matches, so direct callers bypass it by design.
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
			// A cleanly decoded body that maps to zero rows (self-pair or non-positive amounts) returns no output and no error,
			// so the completeness re-derive counts expected=0 instead of an undecodable blind spot holding `comet` at
			// complete=false forever. Indeterminate parse failures keep the error path.
			if errors.Is(err, canonical.ErrPairMismatch) {
				// Self-pair swap is the exploit primitive; these rows never reach `trades`, so
				// this counter is the only place the burst is observable. Detection only.
				d.bumpDroppedSwapMetric(obs.AMMSelfPairSwapTotal, closedAt)
				return nil, nil
			}
			if errors.Is(err, ErrNonPositiveAmounts) {
				// Same drop shape and recency gate as the self-pair branch.
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
