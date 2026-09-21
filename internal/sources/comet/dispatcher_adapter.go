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

// Decoder is the dispatcher-facing view of Comet. Single instance
// per indexer — Comet uses a shared ("POOL", <event_name>) topic
// namespace across every pool contract, so event ROUTING is by topic
// bytes, but ATTRIBUTION is gated on contract identity (ADR-0035/
// 0040, CS-026): any pubnet contract deployed from (or mimicking)
// the Balancer-v1 WASM emits the identical topic shape, and without
// the gate a look-alike could inject fabricated trades under
// `source = "comet"`.
//
// Comet has NO factory namespace — no creation event announces new
// pools — so the gate is the curated-set mechanism (ADR-0040 §1
// mechanism 2/3): the in-code MainnetGatedSet (today exactly one
// pool, Blend's backstop) is the trust root; caller opts layer the
// protocol_contracts DB warm on top (the operator seam for admitting
// a future pool without a redeploy). The WASM-hash sweep is the
// registered upkeep loop for discovering new byte-identical pools.
//
// No goroutines, no polling. Claims any of the five Soroban-emitted
// POOL events from a REGISTERED pool: swap (→ TradeEvent), join_pool
// / exit_pool / deposit / withdraw (→ LiquidityEvent). Admin
// functions (set_controller, gulp, init) exist but do NOT publish
// events in the Soroban port; BPT transfers go through the SEP-41
// standard token-event surface (handled by sep41_supply when the
// pool is in scope), not the POOL namespace.
type Decoder struct {
	reg *contractid.Registry

	// now sources wall-clock for the self-pair detection metric's recency
	// gate ONLY (never for decoded timestamps — those come from the ledger,
	// see EventClosedAt). Injectable so tests are deterministic; defaults to
	// time.Now in NewDecoder.
	now func() time.Time

	// countMetrics gates the exploit-detection counters
	// (obs.AMMSelfPairSwapTotal / obs.AMMNonPositiveSwapTotal). The
	// dispatcher ALWAYS builds a comet.Decoder when comet is enabled,
	// and the projector builds an INDEPENDENT second instance alongside
	// it whenever Projector.Enabled (ADR-0032 Phase-3 parallel
	// double-write — both decode the same live events, deduped at the
	// DB layer by ON CONFLICT DO NOTHING). Without this gate both
	// instances would increment the same detection event, doubling the
	// exploit signal (Q018). Default true; the projector registry
	// disables it via [Decoder.WithoutMetrics] so the dispatcher's
	// instance — which exists whether or not the projector runs — stays
	// the single canonical counter.
	countMetrics bool
}

// selfPairLiveWindow bounds how recent a self-pair swap's ledger close time
// must be for it to bump the exploit-detection metric. The SAME Decode runs
// during backfill re-ingest AND the completeness re-derive (which re-runs the
// decoder over historical soroban_events to count expected rows) — both
// re-process the 2026-08-25 exploit window, and counting there would re-fire
// the burst alert in present wall-clock time for a non-event. Gating on
// close-time recency keeps the metric a "happening now" signal. The window is
// generous (live decode lags by seconds; a real >1h tip lag is its own page).
const selfPairLiveWindow = time.Hour

// NewDecoder constructs a Comet Decoder. The in-code curated set
// (MainnetGatedSet) is always installed first; caller opts (WithSeed
// from the protocol_contracts warm) layer discovered pools on top.
// There are no factories — comet pools have no on-chain creation
// event to self-register from, so live fan-out never fires.
func NewDecoder(opts ...contractid.Option) *Decoder {
	base := []contractid.Option{contractid.WithSeed(MainnetGatedSet())}
	return &Decoder{reg: contractid.New(append(base, opts...)...), now: time.Now, countMetrics: true}
}

// WithoutMetrics disables this Decoder's exploit-detection counter
// increments. See the countMetrics field doc for why: it is the
// projector registry's opt-out when the dispatcher already runs its
// own comet.Decoder over the same event stream (Q018).
func (d *Decoder) WithoutMetrics() *Decoder {
	d.countMetrics = false
	return d
}

// bumpDroppedSwapMetric increments counter for a determinate,
// zero-row swap drop (self-pair or non-positive-amount) — gated on
// countMetrics (Q018) and close-time recency (T108), same rationale
// as the countMetrics field doc and selfPairLiveWindow.
func (d *Decoder) bumpDroppedSwapMetric(counter *prometheus.CounterVec, closedAt time.Time) {
	if d.countMetrics && d.now().Sub(closedAt) < selfPairLiveWindow {
		counter.WithLabelValues(SourceName).Inc()
	}
}

// Name implements [dispatcher.Decoder].
func (d *Decoder) Name() string { return SourceName }

// Matches implements [dispatcher.Decoder]. Gates on CONTRACT
// IDENTITY, not topic bytes (ADR-0035/0040, CS-026): the bare
// ("POOL", <event>) tuple is the Balancer-v1 event family shared by
// EVERY deployment of that code — forgeable by construction. An
// event matches ONLY when emitted by a pool in the curated registry
// (MainnetGatedSet + protocol_contracts warm). A comet-shaped event
// from an unregistered contract is left for the recognition audit to
// surface (ADR-0033 Claim 2a) — visible, never silently attributed.
func (d *Decoder) Matches(ev events.Event) bool {
	if classify(&ev) == "" {
		return false
	}
	return d.reg.Has(ev.ContractID)
}

// Decode implements [dispatcher.Decoder]. Returns exactly one
// consumer.Event on success — TradeEvent for swap, LiquidityEvent
// for the other four kinds. A decode error is non-fatal per the
// dispatcher contract — counted by the source's orphan/malformed
// metrics and skipped.
//
// Decode itself stays shape-only (no registry lookup): the identity
// gate lives in Matches, which the dispatcher consults first. Direct
// Decode callers (tests, fixture tooling) bypass the gate by design.
func (d *Decoder) Decode(ev events.Event) ([]consumer.Event, error) {
	kind := classify(&ev)
	if kind == "" {
		return nil, ErrNotCometEvent
	}

	closedAt, err := ev.EventClosedAt()
	if err != nil {
		// Comet events use ledger close time for the event timestamp
		// (unlike oracles, there's no contract-declared timestamp in
		// the body). Fail closed like the blend/phoenix/defindex
		// siblings rather than substituting time.Now() — during a
		// backfill replay that would stamp every event with the
		// wall-clock of the replay run, not the historical ledger.
		return nil, err
	}

	if kind == EventSwap {
		trade, err := decodeSwap(&ev, closedAt)
		if err != nil {
			// Determinate "not a serveable trade": the body decoded
			// cleanly but maps to ZERO rows — a self-pair swap (token_in
			// == token_out, the 2026-08 Blend/Comet exploit's primitive →
			// canonical.NewPair's ErrPairMismatch) or non-positive amounts.
			// Return zero output with NO error so the completeness
			// re-derive counts these as expected=0, NOT as undecodable
			// blind spots (which fail the source's verdict closed forever —
			// the INV-3 do-nothing re-derive trap: the 36 exploit
			// self-swaps kept `comet` permanently `complete=false`). Safe
			// precisely because the body FULLY decoded — we read both token
			// addresses and saw they're equal — so there is no hidden-drop
			// risk. The error path stays reserved for INDETERMINATE parse
			// failures, which must remain blind. (Precedent for
			// recognised-but-unserved → (nil, nil):
			// internal/sources/classicmovements/decode.go.)
			if errors.Is(err, canonical.ErrPairMismatch) {
				// Self-pair swap (token_in == token_out) — the exploit
				// primitive. Count it as an exploit-shaped detection signal
				// before dropping: these rows never reach the served `trades`
				// table, so this counter (+ the raw soroban_events landing) is
				// the ONLY place the freeze/divergence-blind self-swap burst is
				// observable. Detection only — drops to zero rows exactly as
				// before, changing no serving or freeze decision. Gated on
				// close-time recency so a backfill / completeness re-derive of
				// the historical exploit window does NOT re-fire the alert (see
				// selfPairLiveWindow).
				d.bumpDroppedSwapMetric(obs.AMMSelfPairSwapTotal, closedAt)
				return nil, nil
			}
			if errors.Is(err, ErrNonPositiveAmounts) {
				// Sibling detection signal to the self-pair branch above —
				// same "decoded cleanly, zero honest rows" drop shape, same
				// recency gate: reconciliation_catalogue.go and
				// verify_decoders.go construct their own un-disabled
				// comet.NewDecoder() to replay historical ledgers, and an
				// ungated counter would be inflated by every completeness
				// sweep rather than reflecting live activity.
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
