package soroswap

import (
	"errors"
	"sort"
	"sync"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/consumer"
	"github.com/Stellar-Index/StellarIndex/internal/events"
)

// PairTokens holds the (token0, token1) identities of a Soroswap pair from factory new_pair events; exported to build a seed map for [WithSeededPairTokensDecoder].
type PairTokens struct {
	Token0 canonical.Asset
	Token1 canonical.Asset
}

// Decoder is the dispatcher-facing Soroswap view: a swap+sync correlation buffer (a SwapEvent is followed immediately by a SyncEvent in the same transaction, paired by (ledger, tx_hash, op_index)) and a pair->(token0, token1) registry from factory new_pair events, since swap events carry only amounts; LP-share SEP-41 events are classified but NOT claimed.
type Decoder struct {
	// mu guards buf, pairTokens and the skip counters. Critical sections that call anything release
	// it via defer; see [Decoder.absorbEvent].
	mu  sync.RWMutex
	buf *buffer
	// pairTokens maps pair-contract C-strkey to (token0, token1); seeded at startup via SeedPair.
	pairTokens map[string]PairTokens

	// onNewPair, when non-nil, runs for every factory new_pair event after the registry update, to persist the mapping.
	// Runs with mu NOT held but on the caller's goroutine, so keep it short.
	onNewPair func(pairStrkey, token0Strkey, token1Strkey string)

	// evictedOrphans counts swap-without-sync evictions ONLY (the real loss class); evictedBareSync counts sync-without-swap,
	// which deposit/withdraw/skim produce on every call.
	evictedOrphans     int
	evictedBareSync    int
	skippedUnknownPair int
	// skippedNonDirectional counts completed pairs whose swap carried no cross-token exchange
	// (ErrNonDirectionalSwap): recognized no-ops, not decode errors.
	skippedNonDirectional int
}

// NewDecoder constructs a Soroswap Decoder with empty state.
func NewDecoder(opts ...DecoderOption) *Decoder {
	d := &Decoder{
		buf:        newBuffer(),
		pairTokens: map[string]PairTokens{},
	}
	for _, opt := range opts {
		opt(d)
	}
	return d
}

// DecoderOption configures a Decoder at construction time.
type DecoderOption func(*Decoder)

// WithSeededPairTokensDecoder pre-loads the pair->tokens cache so startup need not re-walk factory events from genesis.
func WithSeededPairTokensDecoder(seed map[string]PairTokens) DecoderOption {
	return func(d *Decoder) {
		for k, v := range seed {
			d.pairTokens[k] = v
		}
	}
}

// WithPairUpsertHook installs a callback fired on each factory new_pair event with the pair and token_0/token_1 C-strkeys; it runs on the dispatch goroutine after the decoder mutex is released, so keep it cheap.
func WithPairUpsertHook(hook func(pairStrkey, token0Strkey, token1Strkey string)) DecoderOption {
	return func(d *Decoder) {
		d.onNewPair = hook
	}
}

// SeedPair adds a pair->tokens mapping live, safe from any goroutine, and fires the onNewPair hook so SeedFromFactoryRPC callers also persist.
func (d *Decoder) SeedPair(pair string, token0, token1 canonical.Asset) {
	hook := d.storePairTokens(pair, token0, token1)
	if hook != nil {
		hook(pair, token0.ContractID, token1.ContractID)
	}
}

// storePairTokens records the mapping under the write lock and returns the hook for the caller to fire OUTSIDE it (the hook
// does IO). Unlock is deferred because the map write can panic (nil registry map), as in [Decoder.absorbEvent].
func (d *Decoder) storePairTokens(pair string, token0, token1 canonical.Asset) func(string, string, string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.pairTokens[pair] = PairTokens{Token0: token0, Token1: token1}
	return d.onNewPair
}

// Name implements [dispatcher.Decoder].
func (*Decoder) Name() string { return SourceName }

// Matches implements [dispatcher.Decoder], gating on CONTRACT IDENTITY, not topic (ADR-0035): factory new_pair events match only from a MainnetFactories entry (else a foreign contract could inject pair->tokens), pair events only from a REGISTERED pair; an un-seeded real pair has its events dropped.
func (d *Decoder) Matches(ev events.Event) bool {
	kind := classify(&ev)
	if kind == "" {
		return false
	}
	// LP-share SEP-41 events are classified but deliberately NOT claimed: they belong to the
	// sep41_transfers/sep41_supply domain, and the dispatcher routes each event to the FIRST
	// decoder whose Matches returns true, so claiming them would swallow the events of any
	// LP-share token later added to [supply] watched_sep41_contracts.
	if kind == EventPairToken {
		return false
	}
	if kind == EventNewPair {
		// Soroswap has more than one factory; gate on the full verified set (ADR-0035).
		return IsMainnetFactory(ev.ContractID)
	}
	_, known := d.pairTokensFor(ev.ContractID)
	return known
}

// Decode implements [dispatcher.Decoder].
func (d *Decoder) Decode(ev events.Event) ([]consumer.Event, error) {
	kind := classify(&ev)
	if kind == "" {
		return nil, nil
	}

	// Factory new_pair: populate the registry, emit nothing.
	if kind == EventNewPair {
		fields, err := decodeNewPair(ev.Value)
		if err != nil {
			return nil, err
		}
		d.SeedPair(fields.Pair, fields.Token0, fields.Token1)
		return nil, nil
	}

	// Skim is a standalone pair-state mutation, not a trade: it does NOT feed the swap+sync buffer.
	if kind == EventSkim {
		closedAt, err := ev.EventClosedAt()
		if err != nil {
			return nil, err
		}
		fields, err := decodeSkim(ev.Value)
		if err != nil {
			return nil, err
		}
		return []consumer.Event{SkimEvent{
			ContractID: ev.ContractID,
			Ledger:     ev.Ledger,
			TxHash:     ev.TxHash,
			OpIndex:    uint32(ev.OperationIndex),
			//nolint:gosec // EventIndex is non-negative by Soroban spec.
			EventIndex: uint32(ev.EventIndex),
			ObservedAt: closedAt,
			To:         fields.To,
			Amount0:    fields.Amount0,
			Amount1:    fields.Amount1,
		}}, nil
	}

	// Deposit/withdraw are self-contained and do NOT feed the swap+sync buffer.
	if kind == EventDeposit || kind == EventWithdraw {
		return d.emitLiquidity(ev, kind)
	}

	// Only swap and sync feed trade emission; EventPairToken (left for sep41) falls out here.
	if kind != EventSwap && kind != EventSync {
		return nil, nil
	}

	closedAt, err := ev.EventClosedAt()
	if err != nil {
		return nil, err
	}

	completed := d.absorbEvent(&ev, kind, closedAt)
	if len(completed) == 0 {
		return nil, nil // still buffering
	}
	return d.emitCompleted(completed)
}

// absorbEvent feeds one swap/sync into the correlation buffer and returns the pairs that just completed. It unlocks via defer
// because the dispatcher RECOVERS decoder panics; a skipped Unlock would leave d.mu held and deadlock dispatch silently.
func (d *Decoder) absorbEvent(ev *events.Event, kind string, closedAt time.Time) []RawPair {
	d.mu.Lock()
	defer d.mu.Unlock()
	completed, evicted := d.buf.absorb(ev, kind, closedAt)
	for _, p := range evicted {
		// An evicted entry is always missing exactly one side. Swap==nil is a bare sync from
		// deposit/withdraw/skim traffic, not a lost trade.
		if p.Swap == nil {
			d.evictedBareSync++
			continue
		}
		d.evictedOrphans++
	}
	return completed
}

// pairTokensFor reads the pair→tokens registry under the read lock, with the release deferred in
// one place for panic safety (see [Decoder.absorbEvent]).
func (d *Decoder) pairTokensFor(contractID string) (PairTokens, bool) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	tokens, ok := d.pairTokens[contractID]
	return tokens, ok
}

// GatedContractSet returns every contract Matches can accept (verified factories plus registered pairs), sorted; the completeness re-derive scopes its lake read to it, so it must never be narrower than the gate.
func (d *Decoder) GatedContractSet() []string {
	d.mu.RLock()
	defer d.mu.RUnlock()
	out := make([]string, 0, len(MainnetFactories)+len(d.pairTokens))
	out = append(out, MainnetFactories...)
	for pair := range d.pairTokens {
		if !IsMainnetFactory(pair) {
			out = append(out, pair)
		}
	}
	sort.Strings(out)
	return out
}

// emitLiquidity decodes a pair deposit/withdraw into a LiquidityEvent; token identities come from the new_pair registry
// (the body has only amounts). An UNSEEDED pair fails CLOSED into an ADR-0033 recognition gap, never a NULL-token row.
func (d *Decoder) emitLiquidity(ev events.Event, kind string) ([]consumer.Event, error) {
	closedAt, err := ev.EventClosedAt()
	if err != nil {
		return nil, err
	}
	fields, err := decodeLiquidity(ev.Value)
	if err != nil {
		return nil, err
	}
	var tok0, tok1 string
	if tokens, ok := d.pairTokensFor(ev.ContractID); ok {
		tok0, tok1 = tokens.Token0.String(), tokens.Token1.String()
	}
	return []consumer.Event{LiquidityEvent{
		ContractID: ev.ContractID,
		Ledger:     ev.Ledger,
		TxHash:     ev.TxHash,
		OpIndex:    uint32(ev.OperationIndex),
		//nolint:gosec // EventIndex is non-negative by Soroban spec.
		EventIndex:  uint32(ev.EventIndex),
		ObservedAt:  closedAt,
		Action:      kind,
		To:          fields.To,
		Token0:      tok0,
		Token1:      tok1,
		Amount0:     fields.Amount0,
		Amount1:     fields.Amount1,
		Liquidity:   fields.Liquidity,
		NewReserve0: fields.NewReserve0,
		NewReserve1: fields.NewReserve1,
	}}, nil
}

// emitCompleted turns completed swap+sync pairs into TradeEvents, skipping (with counters) unknown
// pair token mappings and non-directional swaps.
func (d *Decoder) emitCompleted(completed []RawPair) ([]consumer.Event, error) {
	out := make([]consumer.Event, 0, len(completed))
	for _, r := range completed {
		tokens, ok := d.pairTokensFor(r.Pair)
		if !ok {
			// No factory event seen yet (mid-history start, or out-of-order within the ledger).
			// Skip and count; missing pairs can be backfilled from the factory's new_pair history.
			d.mu.Lock()
			d.skippedUnknownPair++
			d.mu.Unlock()
			continue
		}
		trade, err := decodeSwapLeg(r, tokens.Token0, tokens.Token1)
		if err != nil {
			// A non-directional swap (see ErrNonDirectionalSwap) is a recognized on-chain event that
			// is NOT a trade: project zero rows so the ADR-0033 completeness re-derive counts it as
			// expected-zero rather than undecodable-but-matched.
			if errors.Is(err, ErrNonDirectionalSwap) {
				d.mu.Lock()
				d.skippedNonDirectional++
				d.mu.Unlock()
				continue
			}
			return nil, err
		}
		out = append(out, TradeEvent{Trade: trade})
	}
	return out, nil
}

// Drain implements [dispatcher.Drainer], flushing open swap/sync groups at the end of a bounded stream: a swap-only group is final (the trade reads from the swap body alone), a sync-only group is LP noise and only counted.
func (d *Decoder) Drain() []consumer.Event {
	d.mu.Lock()
	open := d.buf.orphans()
	clear(d.buf.m)
	d.mu.Unlock()
	var out []consumer.Event
	for _, p := range open {
		if p.Swap == nil {
			d.mu.Lock()
			d.evictedBareSync++
			d.mu.Unlock()
			continue
		}
		evs, err := d.emitCompleted([]RawPair{p})
		if err != nil {
			d.mu.Lock()
			d.evictedOrphans++
			d.mu.Unlock()
			continue
		}
		out = append(out, evs...)
	}
	return out
}

// EvictedOrphans is the count of swap-only buffer entries dropped by age-out (a real lost trade); bare-sync evictions are in [Decoder.EvictedBareSync].
func (d *Decoder) EvictedOrphans() int {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.evictedOrphans
}

// EvictedBareSync is the count of sync-only entries dropped by age-out (expected LP traffic from deposits, withdrawals and skims), kept apart so [Decoder.EvictedOrphans] stays a loss signal.
func (d *Decoder) EvictedBareSync() int {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.evictedBareSync
}

// SkippedUnknownPair counts completed pairs whose token mapping was not in the registry.
func (d *Decoder) SkippedUnknownPair() int {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.skippedUnknownPair
}

// UnknownContractDrops implements the dispatcher's duck-typed reporter so a swap dropped for want of a pair-token mapping reaches obs.SourceDecodeErrorsTotal.
func (d *Decoder) UnknownContractDrops() int {
	return d.SkippedUnknownPair()
}

// SkippedNonDirectional counts completed pairs whose swap carried no cross-token exchange.
func (d *Decoder) SkippedNonDirectional() int {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.skippedNonDirectional
}
