package sushiswap_v3

import (
	"errors"
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

// Decoder is the dispatcher-facing view of SushiSwap V3 (ADR-0035
// factory-anchored gating). It owns two pieces of state that are seeded
// together and must never disagree:
//
//  1. reg — the contract-identity gate: the factory trust roots plus every
//     pool the factory has created. This is what Matches consults.
//  2. poolTokens — the pool → (token0, token1) money mapping. Token
//     identities exist ONLY in the factory's `pool_created` body, so a
//     swap that passes the gate still cannot be priced without this.
//
// Both are seeded from the same three places, in the same order the other
// gated sources use: the in-code curated table ([MainnetPools], covering
// history), the protocol_contracts DB warm (contractid.WithSeed, covering
// pools admitted by an operator or discovered after the table was frozen),
// and live factory `pool_created` events (covering everything from here
// on). The DB warm reaches the gate only — protocol_contracts stores a
// contract SET, not token identities — so a pool admitted purely that way
// is gated IN but has no token mapping until its creation event is
// replayed; its swaps then fail closed and are counted
// ([Decoder.SkippedUnknownPool]) instead of being written with invented
// assets.
//
// No correlation buffer: unlike Soroswap (swap+sync) or Phoenix (8 field
// events), a V3 `swap` body is self-contained — both amounts, the
// post-swap price and the tick all arrive in one event. There is nothing
// to hold across events, so there is no orphan class here.
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
			// Unreachable for the curated table (TestMainnetPools_AllTokensAreValidAssets
			// proves every entry converts). Skipping rather than panicking keeps a
			// hand-edit mistake from taking the whole indexer down; the pool stays
			// gated and its swaps fail closed into the counted ErrUnknownPool gap.
			continue
		}
		d.poolTokens[pool] = PoolTokens{Token0: tok0, Token1: tok1}
	}
	return d
}

// Name implements [dispatcher.Decoder].
func (*Decoder) Name() string { return SourceName }

// GatedContractSet returns the factory trust roots plus every registered
// pool — the tightest sound contract-id prefilter for a lake read over
// this decoder. The projector uses it as Source.ContractIDs so a
// far-behind catch-up window never streams the CAP-67 firehose: this
// protocol emits `mint` and `burn`, which are 33% and 12% of all pubnet
// contract events, so the topic-exclusion the other DEX sources use would
// have to drop this source's own events to be effective.
func (d *Decoder) GatedContractSet() []string { return d.reg.GatedSet() }

// Matches implements [dispatcher.Decoder]. Gates on CONTRACT IDENTITY, not
// topic bytes (ADR-0035).
//
// This gate is not a formality here, it is the whole safety story. Every
// event in this protocol has a ONE-element Symbol topic vector and the
// symbols are the most generic on the network: a bounded 10k-ledger census
// of pubnet puts `mint` at 33% and `burn` at 12% of ALL contract events,
// and any contract at all may emit a Map body under a `swap` symbol. A
// topic-only decoder would therefore attribute a large slice of the
// network's token traffic to SushiSwap and, worse, would let an arbitrary
// contract mint trades at prices of its choosing.
//
//   - `pool_created` matches ONLY from one of [MainnetFactories]. This is
//     the trust root: without it any contract could announce a pool with
//     token identities of its choosing, seed itself into the registry, and
//     have its own swaps recorded as real trades at fabricated prices.
//   - every other event matches ONLY from a REGISTERED pool.
//
// Coverage note (ADR-0035): an un-seeded real pool has its events dropped,
// so registry completeness is load-bearing. It is held by three
// independent seeds — the curated [MainnetPools] table, the
// protocol_contracts warm, and the factory's own `pool_created` events
// living in the lake from the factory's first ledger (substrate continuity,
// ADR-0033 Claim 1). A pool the curated table misses fails CLOSED into a
// visible recognition gap; it is never silently mis-attributed.
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

// Decode implements [dispatcher.Decoder].
//
// Only `swap` produces output. `mint` / `burn` / `collect` (the
// concentrated-liquidity position lifecycle) and `init` / `upgraded` /
// `migrated` (the pool lifecycle) are recognized, gated and deliberately
// projected as ZERO rows: they are real events with no trades in them and
// no table of their own yet, so counting them as expected-zero is what
// keeps the ADR-0033 re-derive honest rather than going blind on their
// ledgers. They are the natural next increment for this source — a
// positions/liquidity table, in the shape of soroswap_liquidity.
func (d *Decoder) Decode(ev events.Event) ([]consumer.Event, error) {
	switch classify(&ev) {
	case EventPoolCreated:
		return nil, d.seedFromCreation(ev)
	case EventSwap:
		return d.emitTrade(ev)
	default:
		return nil, nil
	}
}

// seedFromCreation registers a newly created pool in BOTH the identity
// gate and the token map. Matches has already proven the event came from a
// canonical factory, so the pool address in the body is trustworthy.
// Idempotent: the factory re-emits `pool_created` for two pools inside
// their own creation transaction, and a replay re-observes every event.
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
	// Seed fires the persistence hook outside the decoder lock, so the
	// mapping survives a restart after the cursor has passed this ledger.
	d.reg.Seed(fields.Pool, ev.ContractID, ev.Ledger)
	return nil
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
		d.bumpUnknownPool()
		return nil, nil
	}

	trade, err := decodeSwap(
		fields, ev.Ledger, ev.TxHash, ev.OperationIndex, ev.EventIndex, closedAt,
		tokens.Token0, tokens.Token1,
	)
	if err != nil {
		// A swap with no cross-token exchange decoded cleanly but is not a
		// trade. Project zero rows and report no error, so the ADR-0033
		// re-derive counts the ledger as expected-zero instead of going
		// blind on a matched-but-undecodable event.
		if errors.Is(err, ErrNonDirectionalSwap) {
			d.bumpNonDirectional()
			return nil, nil
		}
		return nil, err
	}
	return []consumer.Event{TradeEvent{Trade: trade}}, nil
}

// poolTokensFor reads the token map under the read lock. One helper for
// every reader so the release is a defer in exactly one place — the
// dispatcher RECOVERS a decoder panic and carries on, so an Unlock left on
// the line after a panicking statement would wedge the dispatch goroutine
// for the life of the process.
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

// SkippedUnknownPool counts gated swaps dropped for want of a token
// mapping. Non-zero means the curated table and the protocol_contracts
// warm have drifted apart and a `pool_created` replay is owed.
func (d *Decoder) SkippedUnknownPool() int {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.skippedUnknownPool
}

// SkippedNonDirectional counts swaps carrying no cross-token exchange
// (recognized no-ops; see [ErrNonDirectionalSwap]).
func (d *Decoder) SkippedNonDirectional() int {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.skippedNonDirectional
}
