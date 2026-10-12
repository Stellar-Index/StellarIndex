package sushiswap_v3

import (
	"errors"
	"strconv"
	"sync"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/consumer"
	"github.com/Stellar-Index/StellarIndex/internal/contractid"
	"github.com/Stellar-Index/StellarIndex/internal/events"
)

// PoolTokens is a pool's (token0, token1) pair as canonical assets, exported so operator tooling can build a seed map.
type PoolTokens struct {
	Token0 canonical.Asset
	Token1 canonical.Asset
}

// Decoder is the dispatcher-facing SushiSwap V3 view (ADR-0035 factory-anchored gating) owning two states that must agree: the contract-identity gate and pool -> (token0, token1), which exists only in the factory's `pool_created` body; a pool admitted by the DB warm without a token mapping fails closed and is counted ([Decoder.SkippedUnknownPool]), never written with invented assets. No correlation buffer: a V3 `swap` is self-contained.
type Decoder struct {
	reg *contractid.Registry

	mu         sync.RWMutex
	poolTokens map[string]PoolTokens

	skippedUnknownPool    int
	skippedNonDirectional int
}

// NewDecoder constructs a SushiSwap V3 Decoder; options layer the protocol_contracts warm and live-upsert hook (contractid.WithSeed / WithHook) on the intrinsic trust roots.
func NewDecoder(opts ...contractid.Option) *Decoder {
	base := []contractid.Option{
		contractid.WithFactories(MainnetFactories),
		contractid.WithSeed(MainnetGatedSet()),
	}
	d := &Decoder{
		reg:        contractid.New(append(base, opts...)...),
		poolTokens: make(map[string]PoolTokens, len(MainnetPools)),
	}
	for pool, meta := range MainnetPools {
		tok0, err0 := canonical.NewSorobanAsset(meta.Token0)
		tok1, err1 := canonical.NewSorobanAsset(meta.Token1)
		if err0 != nil || err1 != nil {
			// Unreachable for the curated table (TestMainnetPools_AllTokensAreValidAssets).
			// Skipping rather than panicking keeps a hand-edit mistake from taking the
			// indexer down; the pool stays gated and its swaps fail closed (ErrUnknownPool).
			continue
		}
		d.poolTokens[pool] = PoolTokens{Token0: tok0, Token1: tok1}
	}
	d.seedPersistedPools()
	return d
}

// seedPersistedPools restores the token map for pools from the durable pool table without
// overriding a curated entry. An unparsable row is skipped: the pool stays gated and fails closed.
func (d *Decoder) seedPersistedPools() {
	for pool, a := range d.reg.AllAttrs() {
		if _, ok := d.poolTokens[pool]; ok {
			continue
		}
		tok0, err0 := canonical.NewSorobanAsset(a[AttrToken0])
		tok1, err1 := canonical.NewSorobanAsset(a[AttrToken1])
		if err0 != nil || err1 != nil {
			continue
		}
		d.poolTokens[pool] = PoolTokens{Token0: tok0, Token1: tok1}
	}
}

// Name implements [dispatcher.Decoder].
func (*Decoder) Name() string { return SourceName }

// GatedContractSet returns the factory roots plus every registered pool, the tightest sound contract-id prefilter for a lake read; topic exclusion would drop this source's own `mint` / `burn` events.
func (d *Decoder) GatedContractSet() []string { return d.reg.GatedSet() }

// Matches implements [dispatcher.Decoder], gating on contract identity, not topic bytes (ADR-0035), since topics are generic one-element Symbols and any contract may emit a Map under `swap`: `pool_created` matches only from [MainnetFactories] (else a contract could seed itself with chosen token identities), every other event only from a registered pool; an un-seeded real pool fails closed into a visible gap.
func (d *Decoder) Matches(ev events.Event) bool {
	kind := classify(&ev)
	if kind == "" {
		return false
	}
	if kind == EventPoolCreated {
		return d.reg.IsFactory(ev.ContractID)
	}
	return d.reg.Has(ev.ContractID)
}

// Decode implements [dispatcher.Decoder]: `swap` yields a trade, `mint` / `burn` / `collect` a PositionEvent, and lifecycle events (`init` / `upgraded` / `migrated`) are gated and projected as zero rows so the ADR-0033 re-derive counts them as expected-zero.
func (d *Decoder) Decode(ev events.Event) ([]consumer.Event, error) {
	switch kind := classify(&ev); kind {
	case EventPoolCreated:
		return nil, d.seedFromCreation(ev)
	case EventSwap:
		return d.emitTrade(ev)
	case EventMint, EventBurn, EventCollect:
		return d.emitPosition(ev, kind)
	default:
		return nil, nil
	}
}

// seedFromCreation registers a newly created pool in both the identity gate and the token map.
// Matches has already proven the event came from a canonical factory, so the body's pool address
// is trustworthy. Idempotent: the factory re-emits `pool_created` for a pool inside its creation
// transaction, and a replay re-observes every event.
func (d *Decoder) seedFromCreation(ev events.Event) error {
	fields, err := decodePoolCreated(ev.Value)
	if err != nil {
		return err
	}
	if fields.Pool == "" {
		return nil
	}
	d.mu.Lock()
	d.poolTokens[fields.Pool] = PoolTokens{Token0: fields.Token0, Token1: fields.Token1}
	d.mu.Unlock()
	// Seed fires the persistence hooks outside the decoder lock, so the
	// mapping survives a restart after the cursor has passed this ledger.
	d.reg.SeedWithAttrs(fields.Pool, ev.ContractID, ev.Ledger, poolAttrs(fields))
	return nil
}

// Attribute keys of the persisted per-pool row.
const (
	AttrToken0      = "token0"
	AttrToken1      = "token1"
	AttrFeePips     = "fee_pips"
	AttrTickSpacing = "tick_spacing"
)

func poolAttrs(f PoolCreatedFields) contractid.Attrs {
	return contractid.Attrs{
		AttrToken0:      f.Token0.String(),
		AttrToken1:      f.Token1.String(),
		AttrFeePips:     strconv.FormatUint(uint64(f.FeePips), 10),
		AttrTickSpacing: strconv.FormatInt(int64(f.TickSpacing), 10),
	}
}

// emitTrade decodes one pool `swap` into a TradeEvent, or into a counted
// recognized no-op.
func (d *Decoder) emitTrade(ev events.Event) ([]consumer.Event, error) {
	closedAt, err := ev.EventClosedAt()
	if err != nil {
		return nil, err
	}
	fields, err := decodeSwapFields(ev.Value)
	if err != nil {
		return nil, err
	}

	tokens, known := d.poolTokensFor(ev.ContractID)
	if !known {
		// Gated in by the DB warm but no token mapping yet (creation not replayed since restart).
		// Fail closed, and return the sentinel so obs.SourceDecodeErrorsTotal surfaces the gap;
		// nil would make it indistinguishable from a protocol with nothing left to decode.
		d.bumpUnknownPool()
		return nil, ErrUnknownPool
	}

	trade, err := decodeSwap(
		fields, ev.Ledger, ev.TxHash, ev.OperationIndex, ev.EventIndex, closedAt,
		tokens.Token0, tokens.Token1,
	)
	if err != nil {
		// A swap with no cross-token exchange is not a trade: project zero rows with no error so
		// the ADR-0033 re-derive counts it as expected-zero, not a matched-but-undecodable event.
		if errors.Is(err, ErrNonDirectionalSwap) {
			d.bumpNonDirectional()
			return nil, nil
		}
		return nil, err
	}
	return []consumer.Event{TradeEvent{Trade: trade}}, nil
}

// emitPosition decodes one pool mint / burn / collect into a PositionEvent.
// A gated pool with no token mapping fails closed exactly like a swap does
// (counted ErrUnknownPool, no row), never a row with invented assets.
func (d *Decoder) emitPosition(ev events.Event, kind string) ([]consumer.Event, error) {
	closedAt, err := ev.EventClosedAt()
	if err != nil {
		return nil, err
	}
	fields, err := decodePositionFields(kind, ev.Value)
	if err != nil {
		return nil, err
	}
	tokens, known := d.poolTokensFor(ev.ContractID)
	if !known {
		d.bumpUnknownPool()
		return nil, ErrUnknownPool
	}
	return []consumer.Event{PositionEvent{
		ContractID: ev.ContractID,
		Ledger:     ev.Ledger,
		TxHash:     ev.TxHash,
		OpIndex:    uint32(ev.OperationIndex),
		//nolint:gosec // EventIndex is non-negative by Soroban spec.
		EventIndex: uint32(ev.EventIndex),
		ObservedAt: closedAt,
		Action:     kind,
		Owner:      fields.Owner,
		Sender:     fields.Sender,
		Recipient:  fields.Recipient,
		Token0:     tokens.Token0.String(),
		Token1:     tokens.Token1.String(),
		TickLower:  fields.TickLower,
		TickUpper:  fields.TickUpper,
		Liquidity:  fields.Liquidity,
		Amount0:    fields.Amount0,
		Amount1:    fields.Amount1,
	}}, nil
}

// poolTokensFor reads the token map under the read lock. One helper for every reader keeps the
// release a defer in one place: the dispatcher recovers a decoder panic, so an Unlock after a
// panicking statement would wedge the dispatch goroutine for the life of the process.
func (d *Decoder) poolTokensFor(contractID string) (PoolTokens, bool) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	tokens, ok := d.poolTokens[contractID]
	return tokens, ok
}

// SeedPool records a pool -> tokens mapping and registers the pool in the identity gate, for operator tooling admitting a pool found outside the live path.
func (d *Decoder) SeedPool(pool string, token0, token1 canonical.Asset, factoryID string, firstLedger uint32) {
	if pool == "" {
		return
	}
	d.mu.Lock()
	d.poolTokens[pool] = PoolTokens{Token0: token0, Token1: token1}
	d.mu.Unlock()
	d.reg.Seed(pool, factoryID, firstLedger)
}

func (d *Decoder) bumpUnknownPool() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.skippedUnknownPool++
}

func (d *Decoder) bumpNonDirectional() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.skippedNonDirectional++
}

// SkippedUnknownPool counts gated swaps dropped for want of a token mapping; non-zero means the curated table and the protocol_contracts warm drifted and a `pool_created` replay is owed.
func (d *Decoder) SkippedUnknownPool() int {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.skippedUnknownPool
}

// UnknownContractDrops implements the dispatcher's duck-typed reporter so a gated swap dropped for want of a token mapping reaches obs.SourceDecodeErrorsTotal.
func (d *Decoder) UnknownContractDrops() int {
	return d.SkippedUnknownPool()
}

// SkippedNonDirectional counts swaps carrying no cross-token exchange (recognized no-ops; see [ErrNonDirectionalSwap]).
func (d *Decoder) SkippedNonDirectional() int {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.skippedNonDirectional
}
