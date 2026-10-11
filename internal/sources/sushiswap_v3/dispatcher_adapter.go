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

// PoolTokens is a pool's (token0, token1) identity pair, as canonical
// assets. Exported so operator tooling can build a seed map.
type PoolTokens struct {
	Token0 canonical.Asset
	Token1 canonical.Asset
}

// Decoder is the dispatcher-facing view of SushiSwap V3 (ADR-0035 factory-anchored gating). It
// owns two pieces of state that must never disagree:
//
//  1. reg: the contract-identity gate (factory roots plus every factory-created pool).
//  2. poolTokens: pool -> (token0, token1). Token identities exist only in the factory's
//     `pool_created` body, so a gated swap cannot be priced without it.
//
// Both are seeded from [MainnetPools], the protocol_contracts DB warm (contractid.WithSeed) and
// live `pool_created` events. The warm reaches the gate only; a pool admitted that way has no
// token mapping unless its sushiswap_v3_pools row (contractid.WithAttrSeed) carries it, and its
// swaps fail closed and are counted ([Decoder.SkippedUnknownPool]) rather than written with
// invented assets.
//
// No correlation buffer: a V3 `swap` body is self-contained.
type Decoder struct {
	reg *contractid.Registry

	mu         sync.RWMutex
	poolTokens map[string]PoolTokens

	skippedUnknownPool    int
	skippedNonDirectional int
}

// NewDecoder constructs a SushiSwap V3 Decoder. Options layer the
// protocol_contracts warm and the live-upsert persistence hook
// (contractid.WithSeed / WithHook) on top of the intrinsic trust roots.
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

// GatedContractSet returns the factory trust roots plus every registered pool, the tightest
// sound contract-id prefilter for a lake read. The projector reads it every cycle as
// Source.ContractIDsFunc so a far-behind catch-up never streams the CAP-67 firehose: `mint` and
// `burn` are so common that the topic exclusion other DEX sources use would drop this source's
// own events.
func (d *Decoder) GatedContractSet() []string { return d.reg.GatedSet() }

// Matches implements [dispatcher.Decoder]. Gates on contract identity, not topic bytes
// (ADR-0035). The gate is the whole safety story: every topic is a one-element generic Symbol
// and any contract may emit a Map body under `swap`, so a topic-only decoder would misattribute
// foreign traffic and let an arbitrary contract mint trades at prices of its choosing.
//
//   - `pool_created` matches only from one of [MainnetFactories], the trust root; otherwise any
//     contract could seed itself into the registry with token identities of its choosing.
//   - every other event matches only from a registered pool.
//
// Registry completeness is load-bearing: an un-seeded real pool has its events dropped. Three
// seeds hold it ([MainnetPools], the protocol_contracts warm, and the factory's own
// `pool_created` events in the lake, ADR-0033 Claim 1). A missed pool fails closed into a
// visible gap, never a mis-attribution.
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

// Decode implements [dispatcher.Decoder]: `swap` yields a trade, `mint` / `burn` / `collect` a
// PositionEvent. Pool lifecycle events (`init` / `upgraded` / `migrated`) are gated and projected
// as zero rows so the ADR-0033 re-derive counts them as expected-zero.
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

// SeedPool records a pool → tokens mapping and registers the pool in the
// identity gate. Operator tooling calls this to admit a pool discovered
// outside the live event path.
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

// SkippedUnknownPool counts gated swaps dropped for want of a token mapping; non-zero means the
// curated table and the protocol_contracts warm drifted and a `pool_created` replay is owed.
func (d *Decoder) SkippedUnknownPool() int {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.skippedUnknownPool
}

// UnknownContractDrops implements the dispatcher's duck-typed reporter interface (like
// soroswap.Decoder.EvictedOrphans) so a gated swap dropped for want of a token mapping reaches
// obs.SourceDecodeErrorsTotal instead of vanishing.
func (d *Decoder) UnknownContractDrops() int {
	return d.SkippedUnknownPool()
}

// SkippedNonDirectional counts swaps carrying no cross-token exchange
// (recognized no-ops; see [ErrNonDirectionalSwap]).
func (d *Decoder) SkippedNonDirectional() int {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.skippedNonDirectional
}
