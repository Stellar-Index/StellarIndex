package soroswap

import (
	"errors"
	"sync"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/consumer"
	"github.com/Stellar-Index/StellarIndex/internal/events"
)

// PairTokens captures the (token0, token1) identities of a
// Soroswap pair contract. Populated from factory new_pair events;
// consumed by decodeSwap via the Decoder's registry.
//
// Exported so callers outside the package can construct a seed map
// for [WithSeededPairTokensDecoder]. The indexer and backfill chunks
// build one of these from `timescale.LoadSoroswapPairRegistry` rows.
type PairTokens struct {
	Token0 canonical.Asset
	Token1 canonical.Asset
}

// Decoder is the dispatcher-facing view of Soroswap. It owns two
// pieces of state:
//
//  1. A swap+sync correlation buffer (per discovery doc Q-notes;
//     Soroswap emits a SwapEvent followed by an immediately-
//     following SyncEvent in the same transaction).
//  2. A pair→(token0, token1) registry seeded by factory new_pair
//     events. The swap event itself only carries amounts; token
//     identities come from the pair contract's deploy record.
//
// The Decoder processes four topic shapes:
//   - SoroswapPair:swap  → feeds the swap+sync buffer
//   - SoroswapPair:sync  → feeds the swap+sync buffer; completes a pair
//   - SoroswapPair:skim  → emits a SkimEvent (excess-reserves claim)
//   - SoroswapFactory:new_pair → populates the pair→tokens registry
//
// Other pair-contract events (deposit/withdraw) match but produce
// no output — they're not trades and have their own follow-ups.
// The pair's LP-SHARE SEP-41 token events (transfer/mint/burn/
// approve, Symbol topic[0]) are classified but NOT claimed — see
// the EventPairToken arm in Matches.
//
// Per docs/architecture/ingest-pipeline.md the dispatcher is
// serial, but the mutex is belt-and-braces and also lets operator
// tooling call SeedPair concurrently at startup to warm the cache
// from Timescale.
type Decoder struct {
	// mu guards buf, pairTokens and the skip counters. Every critical
	// section that CALLS anything releases it via `defer` — see
	// [Decoder.absorbEvent] for why the dispatcher's panic guard makes
	// that load-bearing rather than stylistic. (The two bare counter
	// increments in emitCompleted are single statements with no panic
	// path.)
	mu  sync.RWMutex
	buf *buffer
	// pairTokens maps pair-contract C-strkey → (token0, token1).
	// Populated from factory new_pair events live, and seedable
	// from Timescale at startup via SeedPair.
	pairTokens map[string]PairTokens

	// onNewPair, when non-nil, is invoked for every factory
	// new_pair event after the in-memory registry is updated. The
	// indexer + backfill main wire this to a postgres-backed
	// upsert so the mapping survives process restarts and is
	// visible to other parallel backfill chunks. Hook is called
	// with the decoder's mutex held — keep it cheap; the storage
	// implementation should ExecContext non-blockingly or buffer.
	onNewPair func(pairStrkey, token0Strkey, token1Strkey string)

	// Counters surfaced for test assertions. Production wiring
	// maps them to obs.SourceOrphanEventsTotal /
	// SourceDecodeErrorsTotal in PR 165d.
	evictedOrphans     int
	skippedUnknownPair int
	// skippedNonDirectional counts completed swap+sync pairs whose
	// swap carried no cross-token exchange (ErrNonDirectionalSwap) —
	// recognized no-ops, not decode errors.
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

// WithSeededPairTokensDecoder pre-loads the pair→tokens cache.
// Operator tooling calls this at startup to avoid re-walking
// factory events from genesis every boot (we can seed from the
// distinct (source, pair_contract) tuples already persisted in
// the trades hypertable).
func WithSeededPairTokensDecoder(seed map[string]PairTokens) DecoderOption {
	return func(d *Decoder) {
		for k, v := range seed {
			d.pairTokens[k] = v
		}
	}
}

// WithPairUpsertHook installs a callback fired whenever the decoder
// observes a factory new_pair event. The callback receives the C-strkey
// of the pair contract and the C-strkeys of token_0 / token_1.
//
// Lets the indexer + backfill chunks persist the (pair, tokens)
// mapping to durable storage so future restarts and other parallel
// chunks inherit the registry. The hook is fired AFTER the decoder's
// mutex is released (see [Decoder.SeedPair]), but it still runs on the
// dispatch goroutine — keep it cheap (a queued ExecContext is fine; a
// blocking network call is not).
func WithPairUpsertHook(hook func(pairStrkey, token0Strkey, token1Strkey string)) DecoderOption {
	return func(d *Decoder) {
		d.onNewPair = hook
	}
}

// SeedPair adds a pair→tokens mapping live. Safe to call at any
// time from any goroutine. Fires the registered onNewPair hook (if
// any) so callers using SeedFromFactoryRPC also get the persistence
// side-effect for free.
func (d *Decoder) SeedPair(pair string, token0, token1 canonical.Asset) {
	hook := d.storePairTokens(pair, token0, token1)
	if hook != nil {
		hook(pair, token0.ContractID, token1.ContractID)
	}
}

// storePairTokens records the mapping under the write lock and returns
// the registered hook for the caller to fire OUTSIDE it. Deferred
// unlock for the same reason [Decoder.absorbEvent] has one — a map
// write is the one statement in this critical section that CAN panic
// (a nil registry map), and the hook must not run with the decoder
// lock held because it does IO.
func (d *Decoder) storePairTokens(pair string, token0, token1 canonical.Asset) func(string, string, string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.pairTokens[pair] = PairTokens{Token0: token0, Token1: token1}
	return d.onNewPair
}

// Name implements [dispatcher.Decoder].
func (*Decoder) Name() string { return SourceName }

// Matches implements [dispatcher.Decoder]. Topic symbols are NOT unique
// across protocols (every AMM emits "swap"/"sync"/"skim", and any
// contract can emit a `("SoroswapFactory","new_pair")`-shaped event), so
// matching by topic alone would absorb other protocols' events as
// Soroswap trades. We gate on CONTRACT IDENTITY:
//
//   - factory `new_pair` events match ONLY when emitted by one of the
//     canonical Soroswap factories (MainnetFactories — Soroswap has more
//     than one; see that var). This is the load-bearing gate: without it a
//     foreign contract could inject a pair→tokens mapping into the registry
//     and have its own swaps mis-attributed as Soroswap trades (G6-02 /
//     F-1347); with only ONE factory it would miss the others' pairs.
//   - pair-contract events (swap/sync/deposit/withdraw/skim) match ONLY
//     when the emitter is a REGISTERED Soroswap pair. The registry is
//     seeded from factory new_pair events (live), a startup DB warm, and
//     the genesis factory walk (`stellarindex-ops seed-soroswap-pairs`),
//     so a real pair is always present before its events arrive
//     (chronological: a pair's new_pair precedes its first swap), while
//     a topic-collision from a non-Soroswap contract is rejected.
//
// COVERAGE NOTE: completeness of the pair registry is therefore a hard
// requirement — an un-seeded real pair would have its events dropped.
// The swap path already depended on the registry (token resolution), so
// this only extends the same dependency to skim/deposit/withdraw.
func (d *Decoder) Matches(ev events.Event) bool {
	kind := classify(&ev)
	if kind == "" {
		return false
	}
	// LP-share (pair-token) SEP-41 events are classified (the pair WASM
	// emits them — EVERY-event enumeration) but deliberately NOT
	// claimed: they are the sep41_transfers/sep41_supply domain, and
	// the dispatcher routes each event to the FIRST decoder whose
	// Matches returns true — claiming them here would silently swallow
	// the events of any LP-share token an operator later adds to
	// [supply] watched_sep41_contracts. Soroswap projects nothing from
	// them either way (expected-zero), so not claiming them is the
	// honest AND safe arm. (Verified 2026-07-31: no watched SEP-41
	// contract is a registered pair, so nothing changes today.)
	if kind == EventPairToken {
		return false
	}
	if kind == EventNewPair {
		// Soroswap has more than one factory (the primary + launch-era
		// ones); gate on the full verified set so no factory's pairs are
		// dropped (ADR-0035 multi-factory).
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

	// Factory new_pair: populate the registry (which fires the
	// onNewPair hook for persistence), emit nothing.
	if kind == EventNewPair {
		fields, err := decodeNewPair(ev.Value)
		if err != nil {
			return nil, err
		}
		d.SeedPair(fields.Pair, fields.Token0, fields.Token1)
		return nil, nil
	}

	// Pair-contract skim: emit a SkimEvent so the storage sink can
	// land a row in soroswap_skim_events. Standalone event — does
	// NOT feed the swap+sync correlation buffer (skim is its own
	// pair-state mutation, not a trade).
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

	// Pair-contract deposit/withdraw: LP add / remove. Self-contained
	// (does NOT feed the swap+sync correlation buffer). Emit a
	// LiquidityEvent so the sink lands a soroswap_liquidity row. These
	// were classified + Matched but dropped until audit 2026-08-03
	// (every-event mission).
	if kind == EventDeposit || kind == EventWithdraw {
		return d.emitLiquidity(ev, kind)
	}

	// We only care about swap + sync from pair contracts for trade
	// emission now. new_pair (factory, handled above), skim + deposit +
	// withdraw (handled above) and EventPairToken (left for sep41) all
	// fall out here as a no-op return.
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

// absorbEvent feeds one swap/sync into the correlation buffer under the
// decoder lock and returns the pairs that just completed.
//
// A method with `defer d.mu.Unlock()` rather than the inline
// Lock/…/Unlock this used to be, because the dispatcher RECOVERS a
// decoder panic and carries on (internal/dispatcher/panic_guard.go,
// #371 F1). A panic raised inside a critical section whose Unlock sits
// on the line below it never runs that Unlock: d.mu stays held for the
// life of the process, the very next event's Matches blocks on
// RLock, and the whole dispatch goroutine deadlocks with /metrics
// still answering and the unit still `active`. That is strictly worse
// than the crash-loop the guard replaced — a wedge systemd cannot see.
// Every other stateful adapter in the guarded set (phoenix,
// liquidity_pools, claimable_balances) already unlocks via defer.
func (d *Decoder) absorbEvent(ev *events.Event, kind string, closedAt time.Time) []RawPair {
	d.mu.Lock()
	defer d.mu.Unlock()
	completed, evicted := d.buf.absorb(ev, kind, closedAt)
	d.evictedOrphans += len(evicted)
	return completed
}

// pairTokensFor reads the pair→tokens registry under the read lock.
// One helper for the three readers (Matches, emitLiquidity,
// emitCompleted) so the release is a `defer` in exactly one place —
// same panic-safety reasoning as [Decoder.absorbEvent].
func (d *Decoder) pairTokensFor(contractID string) (PairTokens, bool) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	tokens, ok := d.pairTokens[contractID]
	return tokens, ok
}

// emitLiquidity decodes a pair-contract deposit/withdraw event into a
// LiquidityEvent. Each event is self-contained — it carries both token
// amounts, the LP shares minted/burned, and the post-state reserves.
// Token identities come from the factory new_pair registry (the body
// carries only amounts). This arm only runs AFTER Matches() has already
// confirmed the pair is in pairTokens (Matches gates deposit/withdraw on
// registry membership), and SeedPair always sets both tokens — so the
// tok0/tok1 lookup below is in practice always a hit and the empty-token
// branch is unreachable defence-in-depth, not the common case. An
// UNSEEDED pair never reaches here: Matches returns false and the event
// fails CLOSED into an ADR-0033 recognition gap (dropped-and-visible,
// like the swap/trade path) — it is NOT written as a NULL-token row.
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

// emitCompleted turns completed swap+sync pairs into TradeEvents,
// skipping (with counters) the two recognized non-trade classes:
// unknown-pair token mappings and non-directional swaps.
func (d *Decoder) emitCompleted(completed []RawPair) ([]consumer.Event, error) {
	out := make([]consumer.Event, 0, len(completed))
	for _, r := range completed {
		tokens, ok := d.pairTokensFor(r.Pair)
		if !ok {
			// No factory event seen for this pair yet (either we
			// started ingesting mid-history, or the factory event
			// arrived out-of-order within this same ledger). Skip
			// and count; operator tools can backfill missing pairs
			// from the factory's new_pair history.
			d.mu.Lock()
			d.skippedUnknownPair++
			d.mu.Unlock()
			continue
		}
		trade, err := decodeSwap(r, tokens.Token0, tokens.Token1)
		if err != nil {
			// Non-directional swap: the body decoded cleanly but moved
			// value within one token side only (direct pair.swap()
			// invocation — see ErrNonDirectionalSwap). A real,
			// recognized on-chain event that is NOT a trade: project
			// zero rows and return nil so the ADR-0033 completeness
			// re-derive counts it as expected-zero instead of going
			// blind (undecodable-but-matched) on its ledger. Same
			// recognized-no-op contract as redstone's empty
			// write_prices batches.
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

// EvictedOrphans is the count of swap-only (no matching sync) or
// sync-only (no matching swap) buffer entries dropped by age-out.
func (d *Decoder) EvictedOrphans() int {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.evictedOrphans
}

// SkippedUnknownPair is the count of completed swap+sync pairs
// whose token mapping wasn't in the registry at decode time.
func (d *Decoder) SkippedUnknownPair() int {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.skippedUnknownPair
}

// SkippedNonDirectional is the count of completed swap+sync pairs
// whose swap carried no cross-token exchange (recognized no-ops;
// see ErrNonDirectionalSwap).
func (d *Decoder) SkippedNonDirectional() int {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.skippedNonDirectional
}
