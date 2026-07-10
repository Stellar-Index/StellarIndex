package defindex

import (
	"fmt"

	"github.com/StellarIndex/stellar-index/internal/canonical"
	"github.com/StellarIndex/stellar-index/internal/events"
	"github.com/StellarIndex/stellar-index/internal/scval"
)

// classify decides whether this is a Blend strategy flow event we
// decode. Topics are 2-tuples:
//
//	topic[0] = String("BlendStrategy")     — pre-encoded, byte-equal
//	topic[1] = Symbol("deposit"|"withdraw"|"harvest"|…)
//
// Both positions are compared as byte-equal base64 against the
// constants computed at package init — no SCVal parsing on the
// reject path. Returns "" for non-strategy events.
//
// Per the EVERY-event policy (project_every_event_principle), this
// switch enumerates every topic[1] symbol the BlendStrategy contract
// emits — not just the ones we decode into a StrategyFlow today. The
// audit doc (docs/operations/wasm-audits/defindex.md) is the upstream
// reference for the topic set.
func classify(e *events.Event) string {
	if len(e.Topic) < 2 {
		return ""
	}
	if e.Topic[0] != TopicPrefixStrategy {
		return ""
	}
	switch e.Topic[1] {
	case TopicSymbolDeposit:
		return EventDeposit
	case TopicSymbolWithdraw:
		return EventWithdraw
	case TopicSymbolHarvest:
		return EventHarvest
	}
	return ""
}

// classifyVault is the vault-layer twin of classify. Topics are
// 2-tuples:
//
//	topic[0] = String("DeFindexVault")     — pre-encoded, byte-equal
//	topic[1] = Symbol("deposit"|"withdraw"|<governance/admin>)
//
// Per the EVERY-event policy: classifies all 11 vault-layer topic[1]
// symbols enumerated by the upstream contract (audit-2026-05-14 §
// "Topic structure"). Only deposit + withdraw drive a VaultFlow
// today; the other 9 are governance / admin / multiplexed-rebalance
// events with no decoder (yet) but recognising them satisfies the
// closed-set completeness requirement before flipping BackfillSafe.
func classifyVault(e *events.Event) string {
	if len(e.Topic) < 2 {
		return ""
	}
	if e.Topic[0] != TopicPrefixVault {
		return ""
	}
	switch e.Topic[1] {
	case TopicSymbolDeposit:
		return EventDeposit
	case TopicSymbolWithdraw:
		return EventWithdraw
	case TopicSymbolRescue:
		return EventRescue
	case TopicSymbolPaused:
		return EventPaused
	case TopicSymbolUnpaused:
		return EventUnpaused
	case TopicSymbolNReceiver:
		return EventNReceiver
	case TopicSymbolNManager:
		return EventNManager
	case TopicSymbolNEManager:
		return EventNEManager
	case TopicSymbolRBManager:
		return EventRBManager
	case TopicSymbolDFees:
		return EventDFees
	case TopicSymbolRebalance:
		return EventRebalance
	case TopicSymbolNWasm:
		return EventNWasm
	}
	return ""
}

// classifyFactory is the factory-layer twin of classify /
// classifyVault. Topics are 2-tuples:
//
//	topic[0] = String("DeFindexFactory")    — pre-encoded, byte-equal
//	topic[1] = Symbol("create"|"n_fee")
//
// We recognise factory events so the dispatcher's drop-counter
// doesn't file them as "unmatched topic" — EVERY-event policy
// (project_every_event_principle). A `create` match's BODY is now
// decoded (decodeFactoryCreateStrategies, ROADMAP #7 residual
// 2026-07-10) purely for its BlendStrategy fan-out side effect — see
// that function's doc for the evidence. `n_fee`'s body is still not
// decoded (no registry fan-out to do — protocol-fee-recipient
// governance, not a creation announcement). Neither ever produces a
// consumer.Event: Decoder.Decode returns no Event on a factory match
// (drops cleanly without counting against ErrUnknownEvent).
func classifyFactory(e *events.Event) string {
	if len(e.Topic) < 2 {
		return ""
	}
	if e.Topic[0] != TopicPrefixFactory {
		return ""
	}
	switch e.Topic[1] {
	case TopicSymbolCreate:
		return EventCreate
	case TopicSymbolNFee:
		return EventNFee
	}
	return ""
}

// decodeFlow converts one classified strategy event into a
// StrategyFlow.
//
// Body shape (verified on-chain via scan-soroban-events — identical
// for both deposit and withdraw):
//
//	{ from: Address, amount: i128 }
//
// Fields are pulled by name from the top-level Map per
// docs/architecture/contract-schema-evolution.md's decode-by-name
// rule — positional decoding would silently break across upgrades.
func decodeFlow(e *events.Event, kind string) (StrategyFlow, error) {
	closedAt, err := e.EventClosedAt()
	if err != nil {
		return StrategyFlow{}, fmt.Errorf("%w: %w", ErrMalformedPayload, err)
	}

	flow := StrategyFlow{
		Source:     SourceName,
		Ledger:     e.Ledger,
		ClosedAt:   closedAt,
		TxHash:     e.TxHash,
		OpIndex:    e.OperationIndex,
		ContractID: e.ContractID,
	}

	switch kind {
	case EventDeposit:
		flow.Direction = DirectionDeposit
	case EventWithdraw:
		flow.Direction = DirectionWithdraw
	default:
		// Defensive — classify() should have filtered.
		return StrategyFlow{}, fmt.Errorf("%w: %s", ErrUnknownEvent, kind)
	}

	body, err := scval.Parse(e.Value)
	if err != nil {
		return StrategyFlow{}, fmt.Errorf("%w: parse body: %w", ErrMalformedPayload, err)
	}
	entries, err := scval.AsMap(body)
	if err != nil {
		return StrategyFlow{}, fmt.Errorf("%w: body not a Map: %w", ErrMalformedPayload, err)
	}

	fromSv, err := scval.MustMapField(entries, "from")
	if err != nil {
		return StrategyFlow{}, fmt.Errorf("%w: %s.from: %w", ErrMalformedPayload, kind, err)
	}
	flow.From, err = scval.AsAddressStrkey(fromSv)
	if err != nil {
		return StrategyFlow{}, fmt.Errorf("%w: %s.from: %w", ErrMalformedPayload, kind, err)
	}

	amountSv, err := scval.MustMapField(entries, "amount")
	if err != nil {
		return StrategyFlow{}, fmt.Errorf("%w: %s.amount: %w", ErrMalformedPayload, kind, err)
	}
	flow.Amount, err = scval.AsAmountFromI128(amountSv)
	if err != nil {
		return StrategyFlow{}, fmt.Errorf("%w: %s.amount: %w", ErrMalformedPayload, kind, err)
	}

	return flow, nil
}

// decodeVaultFlow converts one classified DeFindexVault event into
// a VaultFlow.
//
// Body shape (per docs/operations/wasm-audits/defindex.md "Body
// shapes", confirmed on-chain via Soroban-RPC getEvents against an
// active wrapper):
//
//	deposit:  { depositor:  Address,
//	            amounts:           Vec<i128>,
//	            df_tokens_minted:  i128,
//	            total_managed_funds_before, total_supply_before }
//	withdraw: { withdrawer: Address,
//	            amounts_withdrawn: Vec<i128>,
//	            df_tokens_burned:  i128,
//	            total_managed_funds_before, total_supply_before }
//
// We ignore the `total_*_before` NAV-snapshot fields at Phase B —
// they're useful for NAV reconstruction but not for flow
// attribution. Fields are pulled by name (decode-by-name per
// contract-schema-evolution.md), so the decoder is robust against
// the vault contract's known mid-life WASM upgrade
// (`ae3409a4…468b` → `07097f83…84b0`) provided the field names
// don't change — and they haven't.
func decodeVaultFlow(e *events.Event, kind string) (VaultFlow, error) {
	closedAt, err := e.EventClosedAt()
	if err != nil {
		return VaultFlow{}, fmt.Errorf("%w: %w", ErrMalformedPayload, err)
	}

	flow := VaultFlow{
		Source:     SourceName,
		Ledger:     e.Ledger,
		ClosedAt:   closedAt,
		TxHash:     e.TxHash,
		OpIndex:    e.OperationIndex,
		ContractID: e.ContractID,
	}

	// Pick the per-direction field names. The vault layer uses
	// distinct names per direction (depositor vs withdrawer,
	// amounts vs amounts_withdrawn, df_tokens_minted vs df_tokens_burned),
	// unlike the strategy layer which shares names across directions.
	var userField, amountsField, tokensField string
	switch kind {
	case EventDeposit:
		flow.Direction = DirectionDeposit
		userField, amountsField, tokensField = "depositor", "amounts", "df_tokens_minted"
	case EventWithdraw:
		flow.Direction = DirectionWithdraw
		userField, amountsField, tokensField = "withdrawer", "amounts_withdrawn", "df_tokens_burned"
	default:
		return VaultFlow{}, fmt.Errorf("%w: %s", ErrUnknownEvent, kind)
	}

	body, err := scval.Parse(e.Value)
	if err != nil {
		return VaultFlow{}, fmt.Errorf("%w: parse body: %w", ErrMalformedPayload, err)
	}
	entries, err := scval.AsMap(body)
	if err != nil {
		return VaultFlow{}, fmt.Errorf("%w: body not a Map: %w", ErrMalformedPayload, err)
	}

	// User address (G-strkey for direct deposit, occasionally
	// C-strkey if a router/aggregator deposited on their behalf).
	userSv, err := scval.MustMapField(entries, userField)
	if err != nil {
		return VaultFlow{}, fmt.Errorf("%w: vault.%s.%s: %w", ErrMalformedPayload, kind, userField, err)
	}
	flow.User, err = scval.AsAddressStrkey(userSv)
	if err != nil {
		return VaultFlow{}, fmt.Errorf("%w: vault.%s.%s: %w", ErrMalformedPayload, kind, userField, err)
	}

	// Multi-asset amounts vector — Vec<i128>.
	amountsSv, err := scval.MustMapField(entries, amountsField)
	if err != nil {
		return VaultFlow{}, fmt.Errorf("%w: vault.%s.%s: %w", ErrMalformedPayload, kind, amountsField, err)
	}
	elems, err := scval.AsVec(amountsSv)
	if err != nil {
		return VaultFlow{}, fmt.Errorf("%w: vault.%s.%s not a Vec: %w", ErrMalformedPayload, kind, amountsField, err)
	}
	flow.Amounts = make([]canonical.Amount, 0, len(elems))
	for i, sv := range elems {
		amt, err := scval.AsAmountFromI128(sv)
		if err != nil {
			return VaultFlow{}, fmt.Errorf("%w: vault.%s.%s[%d]: %w", ErrMalformedPayload, kind, amountsField, i, err)
		}
		flow.Amounts = append(flow.Amounts, amt)
	}

	// Share-token delta (df_tokens_minted on deposit, df_tokens_burned on withdraw).
	tokensSv, err := scval.MustMapField(entries, tokensField)
	if err != nil {
		return VaultFlow{}, fmt.Errorf("%w: vault.%s.%s: %w", ErrMalformedPayload, kind, tokensField, err)
	}
	flow.DfTokens, err = scval.AsAmountFromI128(tokensSv)
	if err != nil {
		return VaultFlow{}, fmt.Errorf("%w: vault.%s.%s: %w", ErrMalformedPayload, kind, tokensField, err)
	}

	return flow, nil
}

// DecodeRebalanceMethod extracts the `rebalance_method` discriminator
// Symbol from a ("DeFindexVault","rebalance") event body. It reads
// ONLY that one documented field and returns the raw Symbol verbatim
// as a [RebalanceMethod]; the per-method payload is deliberately NOT
// decoded — see the [RebalanceMethod] godoc for the do-not-invent
// scope caveats. Returns ErrMalformedPayload if the body is not a Map
// or the discriminator field is absent / not a Symbol.
//
// This is the multiplexer scaffolding for the four-way rebalance
// event (BACKLOG #58): production Decode() does not yet emit a
// consumer.Event for rebalance (the payload is unmodelled pending a
// real on-chain sample), so this decoder is exercised by the golden
// tests + available to operator tooling that inspects raw rebalance
// bodies from the lake once samples exist.
func DecodeRebalanceMethod(e *events.Event) (RebalanceMethod, error) {
	body, err := scval.Parse(e.Value)
	if err != nil {
		return "", fmt.Errorf("%w: parse rebalance body: %w", ErrMalformedPayload, err)
	}
	entries, err := scval.AsMap(body)
	if err != nil {
		return "", fmt.Errorf("%w: rebalance body not a Map: %w", ErrMalformedPayload, err)
	}
	sv, err := scval.MustMapField(entries, RebalanceMethodField)
	if err != nil {
		return "", fmt.Errorf("%w: rebalance.%s: %w", ErrMalformedPayload, RebalanceMethodField, err)
	}
	sym, err := scval.AsSymbol(sv)
	if err != nil {
		return "", fmt.Errorf("%w: rebalance.%s: %w", ErrMalformedPayload, RebalanceMethodField, err)
	}
	return RebalanceMethod(sym), nil
}

// decodeFactoryCreateStrategies extracts every BlendStrategy contract
// address a DeFindexFactory `create` event announces — the
// factory-anchored fan-out seam for the STRATEGY layer (ADR-0035/0040,
// ROADMAP #7 residual 2026-07-10).
//
// Unlike the new VAULT's own address — which the create body has
// never been observed to carry (docs/protocols/defindex.md's
// structural gap; vaults remain curated-set only) — the body's
// top-level `assets` Vec carries, per asset, a `strategies` Vec of
// `{address, name, paused}` entries. Verified against every
// create/n_fee-shaped body from all 3 create-emitting factories in
// the r1 lake (2026-07-10, 117 bodies): mechanically extracting this
// field reproduces the evidence-verified MainnetStrategies set
// EXACTLY — 16/16, zero extra, zero missing. The 6 flagged
// no-proof-B strategy emitters are correctly absent (2 predate the
// earliest factory create event — pre-factory dev/rehearsal
// deployments that cannot carry this proof by definition; the other 4
// were never referenced by any vault config in any create body — orphaned
// deployments a vault never adopted). Schema is stable across the
// full factory-era history (55,484,403 → current): confirmed
// byte-identical field names on the earliest (CAVP2QLP…) and current
// (CDKFHFJI…) factories.
//
// A `create` event that names ZERO strategies for an asset is a
// legitimate, observed shape (e.g. lake ledger 57,147,588) — it
// yields no address for that asset, not an error. Only a
// missing/malformed top-level `assets` field is ErrMalformedPayload;
// a malformed per-asset `strategies` field is skipped rather than
// failing the whole event, so one odd asset entry can't block
// registering strategies announced by OTHER assets in the same
// create.
//
// Callers Seed() each returned address into the contractid.Registry
// (Decoder.Decode) — the SAME live-upsert path Blend/Soroswap/
// Aquarius use for their fan-out, so a restart resumes with a
// complete strategy set via the protocol_contracts warm. The vault
// side of the gate is unaffected: this function never touches
// vault identity.
func decodeFactoryCreateStrategies(e *events.Event) ([]string, error) {
	body, err := scval.Parse(e.Value)
	if err != nil {
		return nil, fmt.Errorf("%w: create body: %w", ErrMalformedPayload, err)
	}
	entries, err := scval.AsMap(body)
	if err != nil {
		return nil, fmt.Errorf("%w: create body not a Map: %w", ErrMalformedPayload, err)
	}
	assetsSv, err := scval.MustMapField(entries, "assets")
	if err != nil {
		return nil, fmt.Errorf("%w: create.assets: %w", ErrMalformedPayload, err)
	}
	assets, err := scval.AsVec(assetsSv)
	if err != nil {
		return nil, fmt.Errorf("%w: create.assets not a Vec: %w", ErrMalformedPayload, err)
	}

	var strategies []string
	for _, asset := range assets {
		strategies = append(strategies, assetStrategyAddresses(asset)...)
	}
	return strategies, nil
}

// assetStrategyAddresses extracts the `address` field of every entry
// in one `assets[i].strategies` Vec (see decodeFactoryCreateStrategies
// for the shape). Lenient by design: any malformed shape at this
// level (missing/wrong-typed `strategies` field, a malformed
// strategy entry) yields fewer addresses for THIS asset, not an
// error — a single odd asset entry must not block registering
// strategies announced by other assets in the same create event.
func assetStrategyAddresses(asset scval.ScVal) []string {
	assetEntries, err := scval.AsMap(asset)
	if err != nil {
		return nil
	}
	stratsSv, err := scval.MustMapField(assetEntries, "strategies")
	if err != nil {
		return nil // no strategies field on this asset — nothing to register
	}
	strats, err := scval.AsVec(stratsSv)
	if err != nil {
		return nil
	}

	var out []string
	for _, s := range strats {
		if addr, ok := strategyAddress(s); ok {
			out = append(out, addr)
		}
	}
	return out
}

// strategyAddress extracts the `address` field from one
// `{address, name, paused}` strategy entry. Returns ok=false (not an
// error) for any malformed entry — see assetStrategyAddresses.
func strategyAddress(s scval.ScVal) (string, bool) {
	entries, err := scval.AsMap(s)
	if err != nil {
		return "", false
	}
	addrSv, err := scval.MustMapField(entries, "address")
	if err != nil {
		return "", false
	}
	addr, err := scval.AsAddressStrkey(addrSv)
	if err != nil {
		return "", false
	}
	return addr, true
}
