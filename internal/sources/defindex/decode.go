package defindex

import (
	"fmt"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/events"
	"github.com/Stellar-Index/StellarIndex/internal/scval"
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
// Per the EVERY-event policy: classifies all 12 vault-layer topic[1]
// symbols enumerated by the upstream contract (the WASM audit's §
// "Topic structure" + the n_wasm census). deposit + withdraw drive a
// VaultFlow, dfees drives per-entry DFees and the seven admin topics a
// VaultAdmin; rebalance and n_wasm have no decoder but recognising them
// satisfies the closed-set completeness requirement.
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
// (project_every_event_principle). Neither `create` nor `n_fee` has
// its BODY decoded: a `create` body is UNTRUSTED for registry fan-out
// because the factory is permissionless (anyone can create a vault — see
// Decoder.Decode), and `n_fee` is protocol-fee-recipient governance,
// not a creation announcement. Neither ever produces a consumer.Event:
// Decoder.Decode returns no Event on a factory match (drops cleanly
// without counting against ErrUnknownEvent).
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
// for deposit and withdraw; harvest carries the same two fields plus
// an unread price_per_share):
//
//	{ from: Address, amount: i128 }              (deposit/withdraw)
//	{ from: Address, amount: i128,
//	  price_per_share: i128 }                    (harvest)
//
// Fields are pulled by name from the top-level Map per
// docs/architecture/ingest-pipeline.md#contract-schema-evolution's decode-by-name
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
	case EventHarvest:
		flow.Direction = DirectionHarvest
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

// decodeVaultFlow converts one classified DeFindexVault event into a VaultFlow.
//
// Body shape (docs/operations/wasm-audits/defindex.md "Body shapes"):
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
// The `total_*_before` NAV-snapshot fields are ignored: not needed for flow
// attribution. Fields are pulled by name
// (ingest-pipeline.md#contract-schema-evolution), so the decoder survives the
// vault's mid-life WASM upgrade as long as the field names hold.
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

// decodeDFees converts one classified ("DeFindexVault","dfees") event into its
// per-asset [DFee] entries: one per distributed_fees Vec element, FeeIndex =
// position.
//
// Body shape (proven from lake blobs, see [DFee]):
//
//	Map{ distributed_fees: Vec[ (token Address<contract>, amount i128) ] }
//
// The tuple is Vec-encoded. Positions 0 (token) and 1 (amount) are read and
// trailing elements tolerated, so an additive per-entry field from a future
// vault upgrade does not error the event. FEWER than 2 elements is
// ErrMalformedPayload. An EMPTY distributed_fees Vec is a real shape (a
// distribution with nothing to distribute): ZERO entries, NO error, keeping
// live-decode and the completeness re-derive count-consistent. Any other body
// is ErrMalformedPayload: fail loud, never silent-drop.
func decodeDFees(e *events.Event) ([]DFee, error) {
	closedAt, err := e.EventClosedAt()
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrMalformedPayload, err)
	}

	body, err := scval.Parse(e.Value)
	if err != nil {
		return nil, fmt.Errorf("%w: parse dfees body: %w", ErrMalformedPayload, err)
	}
	entries, err := scval.AsMap(body)
	if err != nil {
		return nil, fmt.Errorf("%w: dfees body not a Map: %w", ErrMalformedPayload, err)
	}
	feesSv, err := scval.MustMapField(entries, "distributed_fees")
	if err != nil {
		return nil, fmt.Errorf("%w: dfees.distributed_fees: %w", ErrMalformedPayload, err)
	}
	elems, err := scval.AsVec(feesSv)
	if err != nil {
		return nil, fmt.Errorf("%w: dfees.distributed_fees not a Vec: %w", ErrMalformedPayload, err)
	}

	fees := make([]DFee, 0, len(elems))
	for i, sv := range elems {
		pair, err := scval.AsVec(sv)
		if err != nil {
			return nil, fmt.Errorf("%w: dfees.distributed_fees[%d]: %w", ErrMalformedPayload, i, err)
		}
		if len(pair) < 2 {
			return nil, fmt.Errorf("%w: dfees.distributed_fees[%d]: want tuple (Vec) of at least 2, got Vec of length %d", ErrMalformedPayload, i, len(pair))
		}
		token, err := scval.AsAddressStrkey(pair[0])
		if err != nil {
			return nil, fmt.Errorf("%w: dfees.distributed_fees[%d].token: %w", ErrMalformedPayload, i, err)
		}
		amount, err := scval.AsAmountFromI128(pair[1])
		if err != nil {
			return nil, fmt.Errorf("%w: dfees.distributed_fees[%d].amount: %w", ErrMalformedPayload, i, err)
		}
		fees = append(fees, DFee{
			Vault:    e.ContractID,
			Ledger:   e.Ledger,
			ClosedAt: closedAt,
			TxHash:   e.TxHash,
			OpIndex:  e.OperationIndex,
			FeeIndex: i,
			Token:    token,
			Amount:   amount,
		})
	}
	return fees, nil
}

// vaultAdminFields names the body fields each admin topic carries, as
// observed in the r1-lake samples; "" means the body has no such field.
var vaultAdminFields = map[string]struct{ caller, strategy, newAddress, amount string }{
	EventRescue:    {caller: "caller", strategy: "strategy_address", amount: "amount_withdrawn"},
	EventPaused:    {caller: "caller", strategy: "strategy_address"},
	EventUnpaused:  {caller: "caller", strategy: "strategy_address"},
	EventNReceiver: {caller: "caller", newAddress: "new_fee_receiver"},
	EventNManager:  {newAddress: "new_manager"},
	EventNEManager: {newAddress: "new_emergency_manager"},
	EventRBManager: {newAddress: "new_rebalance_manager"},
}

// decodeVaultAdmin converts one classified vault admin event into a
// [VaultAdmin]. Every field [vaultAdminFields] names for the kind is
// required; extra body fields are ignored (decode-by-name).
func decodeVaultAdmin(e *events.Event, kind string) (VaultAdmin, error) {
	spec, ok := vaultAdminFields[kind]
	if !ok {
		return VaultAdmin{}, fmt.Errorf("%w: %s", ErrUnknownEvent, kind)
	}
	closedAt, err := e.EventClosedAt()
	if err != nil {
		return VaultAdmin{}, fmt.Errorf("%w: %w", ErrMalformedPayload, err)
	}
	body, err := scval.Parse(e.Value)
	if err != nil {
		return VaultAdmin{}, fmt.Errorf("%w: parse %s body: %w", ErrMalformedPayload, kind, err)
	}
	entries, err := scval.AsMap(body)
	if err != nil {
		return VaultAdmin{}, fmt.Errorf("%w: %s body not a Map: %w", ErrMalformedPayload, kind, err)
	}

	out := VaultAdmin{
		Vault:    e.ContractID,
		Ledger:   e.Ledger,
		ClosedAt: closedAt,
		TxHash:   e.TxHash,
		OpIndex:  e.OperationIndex,
		Kind:     kind,
	}
	for _, f := range []struct {
		name string
		dst  *string
	}{
		{spec.caller, &out.Caller},
		{spec.strategy, &out.Strategy},
		{spec.newAddress, &out.NewAddress},
	} {
		if f.name == "" {
			continue
		}
		sv, err := scval.MustMapField(entries, f.name)
		if err != nil {
			return VaultAdmin{}, fmt.Errorf("%w: vault.%s.%s: %w", ErrMalformedPayload, kind, f.name, err)
		}
		if *f.dst, err = scval.AsAddressStrkey(sv); err != nil {
			return VaultAdmin{}, fmt.Errorf("%w: vault.%s.%s: %w", ErrMalformedPayload, kind, f.name, err)
		}
	}
	if spec.amount != "" {
		sv, err := scval.MustMapField(entries, spec.amount)
		if err != nil {
			return VaultAdmin{}, fmt.Errorf("%w: vault.%s.%s: %w", ErrMalformedPayload, kind, spec.amount, err)
		}
		amt, err := scval.AsAmountFromI128(sv)
		if err != nil {
			return VaultAdmin{}, fmt.Errorf("%w: vault.%s.%s: %w", ErrMalformedPayload, kind, spec.amount, err)
		}
		// A negative withdrawal would fail the table's CHECK and stall the
		// projector on one row; reject it at decode instead.
		if amt.Sign() < 0 {
			return VaultAdmin{}, fmt.Errorf("%w: vault.%s.%s negative: %s", ErrMalformedPayload, kind, spec.amount, amt)
		}
		out.Amount = &amt
	}
	return out, nil
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
// event: production Decode() does not yet emit a
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
