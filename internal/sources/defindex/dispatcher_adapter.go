package defindex

import (
	"github.com/Stellar-Index/StellarIndex/internal/consumer"
	"github.com/Stellar-Index/StellarIndex/internal/contractid"
	"github.com/Stellar-Index/StellarIndex/internal/events"
)

// Decoder implements dispatcher.Decoder (the event-based variant —
// not ContractCallDecoder). DeFindex contracts publish Soroban
// contract events on every capital flow at both layers:
//
//   - Vault wrappers: `("DeFindexVault","deposit"|"withdraw")` —
//     end-user (G-strkey) attribution.
//   - Blend strategies: `("BlendStrategy","deposit"|"withdraw"|…)` —
//     vault↔strategy capital movement (`from` = vault C-strkey).
//
// We match both, gated on CONTRACT IDENTITY (ADR-0035/0040, CS-026):
// the namespaced topic strings are still just strings any pubnet
// contract can emit, and the lake contains emitters that carry the
// topic shape but none of the four independent DeFindex-provenance
// proofs (see MainnetVaults). Dispatch is therefore topic shape AND
// registry membership — the curated evidence-verified set plus any
// operator-seeded protocol_contracts rows.
type Decoder struct {
	// reg gates Matches() on contract identity. The factory trust
	// roots (MainnetFactories) gate factory events; children (vaults
	// AND strategies) come from the curated, evidence-verified in-code
	// seed + the protocol_contracts operator warm.
	//
	// There is NO live fan-out from factory `create` bodies. The
	// DeFindex factory is PERMISSIONLESS — anyone can create a vault —
	// so the BlendStrategy addresses a create body NAMES are
	// attacker-controlled: a canonical-factory emitter proves only that
	// the real factory announced SOME (possibly attacker-owned) vault,
	// never that a named address is a genuine DeFindex strategy. The
	// old ROADMAP #7 fan-out Seeded them anyway, which let an attacker
	// register arbitrary contracts as "strategies" merely by naming
	// them in a create they triggered, contaminating flow/TVL stats
	// (task #34, W8 recon 6c). Both layers now behave the same: neither
	// the VAULT's own address (never carried by any observed create
	// body) nor a strategy address self-registers — each fail-closes
	// into an ADR-0033 recognition gap until an operator verifies
	// provenance and seeds it (docs/protocols/defindex.md).
	reg *contractid.Registry
}

// NewDecoder constructs a defindex Decoder. Contract-identity gating
// (ADR-0035/0040): the curated mainnet set (vault wrappers +
// strategies, docs/protocols/defindex.md) is ALWAYS seeded — it is the
// trust root for BOTH layers, because neither a vault's nor a
// strategy's identity can be safely reconstructed from the
// PERMISSIONLESS factory's creation events (the create omits the vault
// address entirely and only NAMES attacker-controlled strategy
// addresses — task #34, W8 recon 6c). Caller opts layer the
// protocol_contracts DB warm + live-upsert hook on top; that operator
// seam (verify provenance, then seed protocol_contracts / extend the
// in-code set) is how a future BlendStrategy deployment is admitted —
// there is no automatic create-body fan-out to keep the strategy half
// current on its own.
func NewDecoder(opts ...contractid.Option) *Decoder {
	base := []contractid.Option{
		contractid.WithFactories(MainnetFactories),
		contractid.WithSeed(MainnetGatedSet()),
	}
	return &Decoder{reg: contractid.New(append(base, opts...)...)}
}

// Name implements [dispatcher.Decoder].
func (d *Decoder) Name() string { return SourceName }

// Matches implements [dispatcher.Decoder]. Gates on CONTRACT
// IDENTITY, not topic bytes (ADR-0035/0040):
//
//   - vault / strategy flow events match ONLY when emitted by a
//     REGISTERED child (curated seed + protocol_contracts warm);
//   - factory events (`create` / `n_fee`) match ONLY when emitted
//     by one of the canonical MainnetFactories. Decode returns
//     ([], nil) for them — recognised for EVERY-event-policy
//     completeness, not decoded into a flow.
//
// COVERAGE NOTE (ADR-0035): an un-seeded real VAULT *or* STRATEGY
// fail-closes into an ADR-0033 recognition gap — visible, never
// silently mis-attributed. Closing such a gap is an operator step
// (verify provenance, then seed protocol_contracts / extend the
// in-code set) for BOTH layers. The factory `create` body is NOT
// trusted to self-register children: it omits the vault address
// entirely and only NAMES attacker-controlled strategy addresses
// (the factory is permissionless — task #34, W8 recon 6c), so a
// canonical-factory emitter cannot vouch for them.
func (d *Decoder) Matches(ev events.Event) bool {
	if classify(&ev) != "" || classifyVault(&ev) != "" {
		return d.reg.Has(ev.ContractID)
	}
	return classifyFactory(&ev) != "" && d.reg.IsFactory(ev.ContractID)
}

// Decode implements [dispatcher.Decoder]. Emits one Event per
// matched flow — Event (strategy layer), VaultEvent (vault wrapper
// layer), or DFeesEvent (one per dfees distributed_fees entry, W5.2)
// — for the events we model. Every OTHER recognised topic (vault
// rebalance + the seven remaining admin events; factory `create` /
// `n_fee`) drops cleanly with (nil, nil): "match, nothing to emit".
// Returning an ERROR is a "skip + count-as-decode-error" signal,
// reserved for genuinely malformed deposit/withdraw bodies — NOT for
// topics we recognise but intentionally don't model yet (BACKLOG #58),
// and NOT for factory bodies (their contents are untrusted and never
// decoded — task #34, W8 recon 6c). Filing those as decode errors
// would inflate the source's decode-error counter for normal upstream
// traffic.
func (d *Decoder) Decode(ev events.Event) ([]consumer.Event, error) {
	if kind := classify(&ev); kind != "" {
		return d.decodeStrategy(&ev, kind)
	}
	if kind := classifyVault(&ev); kind != "" {
		return d.decodeVault(&ev, kind)
	}
	if classifyFactory(&ev) != "" {
		// create / n_fee — recognised so the dispatcher's drop-counter
		// doesn't file them as "unmatched topic"; neither is itself a
		// consumer.Event, and neither's BODY is decoded.
		//
		// SECURITY (task #34, W8 recon 6c): the DeFindex factory is
		// PERMISSIONLESS — anyone can create a vault — and a `create`
		// body's `assets[].strategies[].address` fields are
		// attacker-controlled. Matches() proving ev.ContractID is a
		// canonical factory does NOT make those NAMED addresses genuine
		// DeFindex strategies; it only proves the real factory announced
		// SOME (possibly attacker-owned) vault. The old ROADMAP #7
		// fan-out Seeded them anyway, so an attacker could register
		// arbitrary contracts as "strategies" merely by naming them, and
		// their subsequent ("BlendStrategy",…) events then decoded as
		// recognised DeFindex flows, contaminating flow/TVL attribution.
		//
		// No execution-corroboration signal (the dispatcher's
		// ExecutionCorroborated — an ACTUAL invocation of the strategy)
		// is available on this event decode path, so we fail closed: a
		// strategy is recognised ONLY when it is in the curated,
		// evidence-verified trust root (MainnetStrategies) or an operator
		// has verified its provenance and seeded protocol_contracts — the
		// SAME posture vaults already have. A named-but-unverified
		// strategy fail-closes into an ADR-0033 recognition gap.
		return nil, nil
	}
	// Defensive — Matches should have filtered.
	return nil, ErrUnknownEvent
}

// decodeStrategy handles a classified BlendStrategy event.
// deposit/withdraw/harvest each model a StrategyFlow. The old premise
// that harvest's body "has never been observed on-chain" was disproved
// by the lake (audit 2026-08-04 finding 4: 1,018 harvests; body
// `{amount: i128, from: Address, price_per_share: i128}` — the exact
// {from, amount} shape decodeFlow reads by name, the extra field
// unread), so dropping them under-counted vault NAV by the full
// harvested yield. Recovery: projector-replay -source defindex.
func (d *Decoder) decodeStrategy(ev *events.Event, kind string) ([]consumer.Event, error) {
	if kind != EventDeposit && kind != EventWithdraw && kind != EventHarvest {
		return nil, nil // any future unmodelled strategy topic
	}
	flow, err := decodeFlow(ev, kind)
	if err != nil {
		return nil, err
	}
	flow.EventIndex = uint32(ev.EventIndex) //nolint:gosec // event index is small, non-negative
	return []consumer.Event{Event{Flow: flow}}, nil
}

// decodeVault handles a classified DeFindexVault event.
// deposit/withdraw model a VaultFlow; dfees models per-asset [DFee]
// entries (W5.2 — body shape proven from real lake blobs, one
// DFeesEvent per distributed_fees Vec entry so reconcile
// expected-counts equal served rows). `rebalance` and the seven other
// admin topics (rescue / paused / unpaused / nreceiver / nmanager /
// nemanager / rbmanager, plus n_wasm) remain recognised but NOT
// modelled — their bodies have never been observed on-chain, so they
// drop cleanly. The rebalance discriminator scaffolding lives in
// [DecodeRebalanceMethod]; the per-method payload decode is blocked on
// real samples (BACKLOG #58).
func (d *Decoder) decodeVault(ev *events.Event, kind string) ([]consumer.Event, error) {
	if kind == EventDFees {
		fees, err := decodeDFees(ev)
		if err != nil {
			return nil, err
		}
		if len(fees) == 0 {
			// Empty distributed_fees Vec — a real observed shape (a
			// distribution ran with nothing to distribute): recognised,
			// zero rows, NOT an error and NOT ErrMalformedPayload.
			return nil, nil
		}
		out := make([]consumer.Event, 0, len(fees))
		for i := range fees {
			fees[i].EventIndex = uint32(ev.EventIndex) //nolint:gosec // event index is small, non-negative
			out = append(out, DFeesEvent{Fee: fees[i]})
		}
		return out, nil
	}
	if kind != EventDeposit && kind != EventWithdraw {
		return nil, nil // rebalance / admin (unmodelled)
	}
	flow, err := decodeVaultFlow(ev, kind)
	if err != nil {
		return nil, err
	}
	flow.EventIndex = uint32(ev.EventIndex) //nolint:gosec // event index is small, non-negative
	return []consumer.Event{VaultEvent{Flow: flow}}, nil
}
