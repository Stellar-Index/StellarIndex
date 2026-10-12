package defindex

import (
	"fmt"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/events"
	"github.com/Stellar-Index/StellarIndex/internal/scval"
)

// classify decides whether this is a Blend strategy flow event we decode: topic[0] = String("BlendStrategy"), topic[1] =
// Symbol("deposit"|"withdraw"|"harvest"|...), compared as pre-encoded base64 so rejects skip SCVal parsing. Returns "" otherwise.
// The switch enumerates every topic[1] symbol the contract emits (EVERY-event policy; docs/operations/wasm-audits/defindex.md).
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

// classifyVault is the vault-layer twin of classify: topic[0] = String("DeFindexVault"), topic[1] = Symbol. It classifies all 12
// vault-layer symbols (EVERY-event policy). deposit and withdraw drive a VaultFlow, dfees per-entry DFees, the seven admin topics a
// VaultAdmin; rebalance and n_wasm have no decoder but are recognised for closed-set completeness.
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

// classifyFactory is the factory-layer twin of classify: topic[0] = String("DeFindexFactory"), topic[1] = "create"|"n_fee". Recognised
// so the drop counter does not file them as "unmatched topic". Neither body is decoded: a `create` body is untrusted for registry
// fan-out (permissionless factory, see Decoder.Decode) and `n_fee` is fee-recipient governance.
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

// decodeFlow converts one classified strategy event into a StrategyFlow. Body: { from: Address, amount: i128 } (harvest adds an
// unread price_per_share: i128). Fields are pulled by name (decode-by-name rule,
// docs/architecture/ingest-pipeline.md#contract-schema-evolution); positional decoding breaks across contract upgrades.
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

// decodeVaultFlow converts one classified DeFindexVault event into a VaultFlow (body shapes: docs/operations/wasm-audits/defindex.md).
// deposit: { depositor, amounts: Vec<i128>, df_tokens_minted: i128 }; withdraw: { withdrawer, amounts_withdrawn: Vec<i128>,
// df_tokens_burned: i128 }. The `total_*_before` NAV fields are ignored. Fields are pulled by name, so the decoder survives the
// vault's mid-life WASM upgrade.
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

// decodeDFees converts one ("DeFindexVault","dfees") event into per-asset [DFee] entries, one per distributed_fees Vec element
// (FeeIndex = position). Body: Map{ distributed_fees: Vec[ (token Address, amount i128) ] }. Positions 0 and 1 are read and trailing
// elements tolerated (additive upgrades); fewer than 2 is ErrMalformedPayload. An empty Vec is a real shape (zero entries, no error),
// keeping live decode and the completeness re-derive count-consistent. Any other body fails loud.
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

// DecodeRebalanceMethod extracts the `rebalance_method` Symbol from a ("DeFindexVault","rebalance") body as a [RebalanceMethod]; per-method payloads are not decoded, it returns ErrMalformedPayload if the body is not a Map or the field is absent or not a Symbol, and Decode() emits nothing for rebalance until a real sample exists.
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
