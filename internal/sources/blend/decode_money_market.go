package blend

import (
	"fmt"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/domain"
	"github.com/Stellar-Index/StellarIndex/internal/events"
	"github.com/Stellar-Index/StellarIndex/internal/scval"
)

// ─── Topic-arity expectations ──────────────────────────────────
//
// Per pool/src/events.rs in blend-contracts-v2 (c19abee5b9be4f49e0cda9057e87d343e5dcc095):
//
//	supply / withdraw / supply_collateral / withdraw_collateral /
//	borrow / repay:                3 topics  [Symbol, asset, from]
//	flash_loan:                    4 topics  [Symbol, asset, from, contract]
//	gulp:                          2 topics  [Symbol, asset]
//	claim:                         2 topics  [Symbol, from]
//	reserve_emission_update:       1 topic   [Symbol]
//	gulp_emissions:                1 topic   [Symbol]
//	bad_debt:                      3 topics  [Symbol, user, asset]
//	defaulted_debt:                2 topics  [Symbol, asset]
//	set_admin / update_pool /
//	queue_set_reserve / cancel_set_reserve: 2 topics [Symbol, admin]
//	set_reserve:                   1 topic   [Symbol]
//	set_status:                    1 or 2    [Symbol] (auto) | [Symbol, admin]
//	deploy (pool-factory):         1 topic   [Symbol]

// ─── Decoded event types ───────────────────────────────────────

// PositionEvent is the decoded shape of every money-market event changing a (user, asset, pool) position; EventKind discriminates, TokenAmount and BOrDAmount are i128 *big.Int (ADR-0003). Named type (not alias) over domain.BlendPositionEvent so it carries the consumer.Event methods.
type PositionEvent domain.BlendPositionEvent

// EmissionEvent is the decoded shape of the emission / credit-risk events (gulp, claim, reserve_emission_update, gulp_emissions, bad_debt, defaulted_debt); bodies are heterogeneous, so fields are nullable and EventKind says which are populated. Named type over domain.BlendEmissionEvent.
type EmissionEvent domain.BlendEmissionEvent

// AdminEvent is the decoded shape of every pool-config / admin / factory lifecycle event; ContractID is the EMITTING contract (the pool, or the factory for `deploy`). Named type over domain.BlendAdminEvent.
type AdminEvent domain.BlendAdminEvent

// classifyAny maps every topic the package declares to its Event* name; the dispatcher adapter uses it (classify covers only auctions).
func classifyAny(e *events.Event) string { //nolint:gocyclo,cyclop // one case per Blend topic; flattening makes the dispatch table easier to audit against pool/src/events.rs + pool-factory/src/events.rs.
	if len(e.Topic) == 0 {
		return ""
	}
	switch e.Topic[0] {
	// Auction events (handled by the auction-specific decoders).
	case TopicSymbolNewAuction:
		return EventNewAuction
	case TopicSymbolFillAuction:
		return EventFillAuction
	case TopicSymbolDeleteAuction:
		return EventDeleteAuction

	// Money-market events.
	case TopicSymbolSupply:
		return EventSupply
	case TopicSymbolWithdraw:
		return EventWithdraw
	case TopicSymbolSupplyCollateral:
		return EventSupplyCollateral
	case TopicSymbolWithdrawCollateral:
		return EventWithdrawCollateral
	case TopicSymbolBorrow:
		return EventBorrow
	case TopicSymbolRepay:
		return EventRepay
	case TopicSymbolFlashLoan:
		return EventFlashLoan

	// Emission + credit-risk events.
	case TopicSymbolGulp:
		return EventGulp
	case TopicSymbolClaim:
		return EventClaim
	case TopicSymbolReserveEmissions:
		return EventReserveEmissions
	case TopicSymbolGulpEmissions:
		return EventGulpEmissions
	case TopicSymbolBadDebt:
		return EventBadDebt
	case TopicSymbolDefaultedDebt:
		return EventDefaultedDebt

	// Admin / status.
	case TopicSymbolSetAdmin:
		return EventSetAdmin
	case TopicSymbolUpdatePool:
		return EventUpdatePool
	case TopicSymbolQueueSetReserve:
		return EventQueueSetReserve
	case TopicSymbolCancelSetReserve:
		return EventCancelSetReserve
	case TopicSymbolSetReserve:
		return EventSetReserve
	case TopicSymbolSetStatus:
		return EventSetStatus

	// Pool factory.
	case TopicSymbolDeploy:
		return EventDeploy

	// V1 pool-factory events.
	case TopicSymbolUpdateEmissions:
		return EventUpdateEmissions
	case TopicSymbolNewLiquidationAuction:
		return EventNewLiquidationAuction
	case TopicSymbolDeleteLiquidationAuction:
		return EventDeleteLiquidationAuction

	default:
		return ""
	}
}

// ─── Position-event decoder ────────────────────────────────────

// decodePositionEvent parses a money-market position-changing event using the topic table at the
// top of this file. One function serves all seven kinds; flash_loan alone carries an extra topic
// for the borrowing contract. Returns ErrMalformedPayload (wrapped) on schema drift.
func decodePositionEvent(e *events.Event, kind string, closedAt time.Time) (PositionEvent, error) {
	wantTopics := 3
	if kind == EventFlashLoan {
		wantTopics = 4
	}
	if len(e.Topic) != wantTopics {
		return PositionEvent{}, fmt.Errorf("%w: %s expected %d topics, got %d",
			ErrMalformedPayload, kind, wantTopics, len(e.Topic))
	}

	asset, err := decodeAddressTopic(e.Topic[1])
	if err != nil {
		return PositionEvent{}, fmt.Errorf("%w: asset: %w", ErrMalformedPayload, err)
	}
	user, err := decodeAddressTopic(e.Topic[2])
	if err != nil {
		return PositionEvent{}, fmt.Errorf("%w: from: %w", ErrMalformedPayload, err)
	}

	var counterparty string
	if kind == EventFlashLoan {
		counterparty, err = decodeAddressTopic(e.Topic[3])
		if err != nil {
			return PositionEvent{}, fmt.Errorf("%w: contract: %w", ErrMalformedPayload, err)
		}
	}

	body, err := scval.Parse(e.Value)
	if err != nil {
		return PositionEvent{}, fmt.Errorf("%w: parse body: %w", ErrMalformedPayload, err)
	}
	tuple, err := scval.AsTupleN(body, 2)
	if err != nil {
		return PositionEvent{}, fmt.Errorf("%w: body shape: %w", ErrMalformedPayload, err)
	}
	tokenAmt, err := scval.AsAmountFromI128(tuple[0])
	if err != nil {
		return PositionEvent{}, fmt.Errorf("%w: token amount: %w", ErrMalformedPayload, err)
	}
	bdAmt, err := scval.AsAmountFromI128(tuple[1])
	if err != nil {
		return PositionEvent{}, fmt.Errorf("%w: b/d-token amount: %w", ErrMalformedPayload, err)
	}

	return PositionEvent{
		Pool:         e.ContractID,
		Kind:         kind,
		Asset:        asset,
		User:         user,
		Counterparty: counterparty,
		TokenAmount:  tokenAmt.BigInt(),
		BOrDAmount:   bdAmt.BigInt(),
		Ledger:       e.Ledger,
		TxHash:       e.TxHash,
		OpIndex:      uint32(e.OperationIndex),
		Timestamp:    closedAt,
	}, nil
}

// ─── Emission-event decoder ────────────────────────────────────

// decodeGulp parses `gulp`: topics [Symbol, Address(asset)], body i128(token_delta).
func decodeGulp(e *events.Event, closedAt time.Time) (EmissionEvent, error) {
	if len(e.Topic) != 2 {
		return EmissionEvent{}, fmt.Errorf("%w: gulp expected 2 topics, got %d",
			ErrMalformedPayload, len(e.Topic))
	}
	asset, err := decodeAddressTopic(e.Topic[1])
	if err != nil {
		return EmissionEvent{}, fmt.Errorf("%w: asset: %w", ErrMalformedPayload, err)
	}
	body, err := scval.Parse(e.Value)
	if err != nil {
		return EmissionEvent{}, fmt.Errorf("%w: parse body: %w", ErrMalformedPayload, err)
	}
	amt, err := scval.AsAmountFromI128(body)
	if err != nil {
		return EmissionEvent{}, fmt.Errorf("%w: token_delta: %w", ErrMalformedPayload, err)
	}
	return EmissionEvent{
		Pool:      e.ContractID,
		Kind:      EventGulp,
		Asset:     asset,
		Amount:    amt.BigInt(),
		Ledger:    e.Ledger,
		TxHash:    e.TxHash,
		OpIndex:   uint32(e.OperationIndex),
		Timestamp: closedAt,
	}, nil
}

// decodeClaim parses `claim`: topics [Symbol, Address(from)], body (reserve_token_ids: Vec<u32>, amount_claimed: i128).
func decodeClaim(e *events.Event, closedAt time.Time) (EmissionEvent, error) {
	if len(e.Topic) != 2 {
		return EmissionEvent{}, fmt.Errorf("%w: claim expected 2 topics, got %d",
			ErrMalformedPayload, len(e.Topic))
	}
	from, err := decodeAddressTopic(e.Topic[1])
	if err != nil {
		return EmissionEvent{}, fmt.Errorf("%w: from: %w", ErrMalformedPayload, err)
	}
	body, err := scval.Parse(e.Value)
	if err != nil {
		return EmissionEvent{}, fmt.Errorf("%w: parse body: %w", ErrMalformedPayload, err)
	}
	tuple, err := scval.AsTupleN(body, 2)
	if err != nil {
		return EmissionEvent{}, fmt.Errorf("%w: body shape: %w", ErrMalformedPayload, err)
	}
	idsVec, err := scval.AsVec(tuple[0])
	if err != nil {
		return EmissionEvent{}, fmt.Errorf("%w: reserve_token_ids: %w", ErrMalformedPayload, err)
	}
	ids := make([]uint32, 0, len(idsVec))
	for i, sv := range idsVec {
		id, err := scval.AsU32(sv)
		if err != nil {
			return EmissionEvent{}, fmt.Errorf("%w: reserve_token_ids[%d]: %w", ErrMalformedPayload, i, err)
		}
		ids = append(ids, id)
	}
	amt, err := scval.AsAmountFromI128(tuple[1])
	if err != nil {
		return EmissionEvent{}, fmt.Errorf("%w: amount_claimed: %w", ErrMalformedPayload, err)
	}
	return EmissionEvent{
		Pool:            e.ContractID,
		Kind:            EventClaim,
		User:            from,
		Amount:          amt.BigInt(),
		ReserveTokenIDs: ids,
		Ledger:          e.Ledger,
		TxHash:          e.TxHash,
		OpIndex:         uint32(e.OperationIndex),
		Timestamp:       closedAt,
	}, nil
}

// decodeReserveEmissionUpdate parses `reserve_emission_update`: topics [Symbol], body (res_token_id: u32, eps: u64, expiration: u64).
func decodeReserveEmissionUpdate(e *events.Event, closedAt time.Time) (EmissionEvent, error) {
	if len(e.Topic) != 1 {
		return EmissionEvent{}, fmt.Errorf("%w: reserve_emission_update expected 1 topic, got %d",
			ErrMalformedPayload, len(e.Topic))
	}
	body, err := scval.Parse(e.Value)
	if err != nil {
		return EmissionEvent{}, fmt.Errorf("%w: parse body: %w", ErrMalformedPayload, err)
	}
	tuple, err := scval.AsTupleN(body, 3)
	if err != nil {
		return EmissionEvent{}, fmt.Errorf("%w: body shape: %w", ErrMalformedPayload, err)
	}
	resID, err := scval.AsU32(tuple[0])
	if err != nil {
		return EmissionEvent{}, fmt.Errorf("%w: res_token_id: %w", ErrMalformedPayload, err)
	}
	eps, err := scval.AsU64(tuple[1])
	if err != nil {
		return EmissionEvent{}, fmt.Errorf("%w: eps: %w", ErrMalformedPayload, err)
	}
	exp, err := scval.AsU64(tuple[2])
	if err != nil {
		return EmissionEvent{}, fmt.Errorf("%w: expiration: %w", ErrMalformedPayload, err)
	}
	return EmissionEvent{
		Pool:            e.ContractID,
		Kind:            EventReserveEmissions,
		ResTokenID:      resID,
		EmissionsPerSec: eps,
		Expiration:      exp,
		Ledger:          e.Ledger,
		TxHash:          e.TxHash,
		OpIndex:         uint32(e.OperationIndex),
		Timestamp:       closedAt,
	}, nil
}

// decodeGulpEmissions parses `gulp_emissions`: topics [Symbol], body i128(emissions).
func decodeGulpEmissions(e *events.Event, closedAt time.Time) (EmissionEvent, error) {
	if len(e.Topic) != 1 {
		return EmissionEvent{}, fmt.Errorf("%w: gulp_emissions expected 1 topic, got %d",
			ErrMalformedPayload, len(e.Topic))
	}
	body, err := scval.Parse(e.Value)
	if err != nil {
		return EmissionEvent{}, fmt.Errorf("%w: parse body: %w", ErrMalformedPayload, err)
	}
	amt, err := scval.AsAmountFromI128(body)
	if err != nil {
		return EmissionEvent{}, fmt.Errorf("%w: emissions: %w", ErrMalformedPayload, err)
	}
	return EmissionEvent{
		Pool:      e.ContractID,
		Kind:      EventGulpEmissions,
		Amount:    amt.BigInt(),
		Ledger:    e.Ledger,
		TxHash:    e.TxHash,
		OpIndex:   uint32(e.OperationIndex),
		Timestamp: closedAt,
	}, nil
}

// decodeBadDebt parses `bad_debt`: topics [Symbol, Address(user), Address(asset)], body i128(d_tokens).
func decodeBadDebt(e *events.Event, closedAt time.Time) (EmissionEvent, error) {
	if len(e.Topic) != 3 {
		return EmissionEvent{}, fmt.Errorf("%w: bad_debt expected 3 topics, got %d",
			ErrMalformedPayload, len(e.Topic))
	}
	user, err := decodeAddressTopic(e.Topic[1])
	if err != nil {
		return EmissionEvent{}, fmt.Errorf("%w: user: %w", ErrMalformedPayload, err)
	}
	asset, err := decodeAddressTopic(e.Topic[2])
	if err != nil {
		return EmissionEvent{}, fmt.Errorf("%w: asset: %w", ErrMalformedPayload, err)
	}
	body, err := scval.Parse(e.Value)
	if err != nil {
		return EmissionEvent{}, fmt.Errorf("%w: parse body: %w", ErrMalformedPayload, err)
	}
	amt, err := scval.AsAmountFromI128(body)
	if err != nil {
		return EmissionEvent{}, fmt.Errorf("%w: d_tokens: %w", ErrMalformedPayload, err)
	}
	return EmissionEvent{
		Pool:      e.ContractID,
		Kind:      EventBadDebt,
		User:      user,
		Asset:     asset,
		Amount:    amt.BigInt(),
		Ledger:    e.Ledger,
		TxHash:    e.TxHash,
		OpIndex:   uint32(e.OperationIndex),
		Timestamp: closedAt,
	}, nil
}

// decodeDefaultedDebt parses `defaulted_debt`: topics [Symbol, Address(asset)], body i128(d_tokens_burnt).
func decodeDefaultedDebt(e *events.Event, closedAt time.Time) (EmissionEvent, error) {
	if len(e.Topic) != 2 {
		return EmissionEvent{}, fmt.Errorf("%w: defaulted_debt expected 2 topics, got %d",
			ErrMalformedPayload, len(e.Topic))
	}
	asset, err := decodeAddressTopic(e.Topic[1])
	if err != nil {
		return EmissionEvent{}, fmt.Errorf("%w: asset: %w", ErrMalformedPayload, err)
	}
	body, err := scval.Parse(e.Value)
	if err != nil {
		return EmissionEvent{}, fmt.Errorf("%w: parse body: %w", ErrMalformedPayload, err)
	}
	amt, err := scval.AsAmountFromI128(body)
	if err != nil {
		return EmissionEvent{}, fmt.Errorf("%w: d_tokens_burnt: %w", ErrMalformedPayload, err)
	}
	return EmissionEvent{
		Pool:      e.ContractID,
		Kind:      EventDefaultedDebt,
		Asset:     asset,
		Amount:    amt.BigInt(),
		Ledger:    e.Ledger,
		TxHash:    e.TxHash,
		OpIndex:   uint32(e.OperationIndex),
		Timestamp: closedAt,
	}, nil
}

// ─── Admin-event decoders ──────────────────────────────────────

// decodeSetAdmin parses `set_admin`: topics [Symbol, Address(admin)], body Address(new_admin).
func decodeSetAdmin(e *events.Event, closedAt time.Time) (AdminEvent, error) {
	if len(e.Topic) != 2 {
		return AdminEvent{}, fmt.Errorf("%w: set_admin expected 2 topics, got %d",
			ErrMalformedPayload, len(e.Topic))
	}
	admin, err := decodeAddressTopic(e.Topic[1])
	if err != nil {
		return AdminEvent{}, fmt.Errorf("%w: admin: %w", ErrMalformedPayload, err)
	}
	body, err := scval.Parse(e.Value)
	if err != nil {
		return AdminEvent{}, fmt.Errorf("%w: parse body: %w", ErrMalformedPayload, err)
	}
	newAdmin, err := scval.AsAddressStrkey(body)
	if err != nil {
		return AdminEvent{}, fmt.Errorf("%w: new_admin: %w", ErrMalformedPayload, err)
	}
	return AdminEvent{
		ContractID: e.ContractID,
		Kind:       EventSetAdmin,
		Admin:      admin,
		Target:     newAdmin,
		Ledger:     e.Ledger,
		TxHash:     e.TxHash,
		OpIndex:    uint32(e.OperationIndex),
		Timestamp:  closedAt,
	}, nil
}

// decodeUpdatePool parses `update_pool`: topics [Symbol, Address(admin)],
// body (backstop_take_rate: u32, max_positions: u32, min_collateral: i128).
func decodeUpdatePool(e *events.Event, closedAt time.Time) (AdminEvent, error) {
	if len(e.Topic) != 2 {
		return AdminEvent{}, fmt.Errorf("%w: update_pool expected 2 topics, got %d",
			ErrMalformedPayload, len(e.Topic))
	}
	admin, err := decodeAddressTopic(e.Topic[1])
	if err != nil {
		return AdminEvent{}, fmt.Errorf("%w: admin: %w", ErrMalformedPayload, err)
	}
	body, err := scval.Parse(e.Value)
	if err != nil {
		return AdminEvent{}, fmt.Errorf("%w: parse body: %w", ErrMalformedPayload, err)
	}
	tuple, err := scval.AsTupleN(body, 3)
	if err != nil {
		return AdminEvent{}, fmt.Errorf("%w: body shape: %w", ErrMalformedPayload, err)
	}
	rate, err := scval.AsU32(tuple[0])
	if err != nil {
		return AdminEvent{}, fmt.Errorf("%w: backstop_take_rate: %w", ErrMalformedPayload, err)
	}
	maxPos, err := scval.AsU32(tuple[1])
	if err != nil {
		return AdminEvent{}, fmt.Errorf("%w: max_positions: %w", ErrMalformedPayload, err)
	}
	minCol, err := scval.AsAmountFromI128(tuple[2])
	if err != nil {
		return AdminEvent{}, fmt.Errorf("%w: min_collateral: %w", ErrMalformedPayload, err)
	}
	return AdminEvent{
		ContractID:       e.ContractID,
		Kind:             EventUpdatePool,
		Admin:            admin,
		BackstopTakeRate: rate,
		MaxPositions:     maxPos,
		MinCollateral:    minCol.BigInt(),
		Ledger:           e.Ledger,
		TxHash:           e.TxHash,
		OpIndex:          uint32(e.OperationIndex),
		Timestamp:        closedAt,
	}, nil
}

// decodeQueueSetReserve parses `queue_set_reserve`: topics [Symbol, Address(admin)], body (asset: Address, metadata: ReserveConfig).
// The full name-sorted ScvMap ReserveConfig goes into AdminEvent.ReserveConfig so storage can marshal it to jsonb losslessly.
func decodeQueueSetReserve(e *events.Event, closedAt time.Time) (AdminEvent, error) {
	if len(e.Topic) != 2 {
		return AdminEvent{}, fmt.Errorf("%w: queue_set_reserve expected 2 topics, got %d",
			ErrMalformedPayload, len(e.Topic))
	}
	admin, err := decodeAddressTopic(e.Topic[1])
	if err != nil {
		return AdminEvent{}, fmt.Errorf("%w: admin: %w", ErrMalformedPayload, err)
	}
	body, err := scval.Parse(e.Value)
	if err != nil {
		return AdminEvent{}, fmt.Errorf("%w: parse body: %w", ErrMalformedPayload, err)
	}
	tuple, err := scval.AsTupleN(body, 2)
	if err != nil {
		return AdminEvent{}, fmt.Errorf("%w: body shape: %w", ErrMalformedPayload, err)
	}
	asset, err := scval.AsAddressStrkey(tuple[0])
	if err != nil {
		return AdminEvent{}, fmt.Errorf("%w: asset: %w", ErrMalformedPayload, err)
	}
	cfg, missing, err := decodeReserveConfig(tuple[1])
	if err != nil {
		return AdminEvent{}, fmt.Errorf("%w: metadata: %w", ErrMalformedPayload, err)
	}
	return AdminEvent{
		ContractID:           e.ContractID,
		Kind:                 EventQueueSetReserve,
		Admin:                admin,
		Asset:                asset,
		ReserveConfig:        cfg,
		ReserveConfigMissing: missing,
		Ledger:               e.Ledger,
		TxHash:               e.TxHash,
		OpIndex:              uint32(e.OperationIndex),
		Timestamp:            closedAt,
	}, nil
}

// decodeCancelSetReserve parses `cancel_set_reserve`: topics [Symbol, Address(admin)], body Address(asset).
func decodeCancelSetReserve(e *events.Event, closedAt time.Time) (AdminEvent, error) {
	if len(e.Topic) != 2 {
		return AdminEvent{}, fmt.Errorf("%w: cancel_set_reserve expected 2 topics, got %d",
			ErrMalformedPayload, len(e.Topic))
	}
	admin, err := decodeAddressTopic(e.Topic[1])
	if err != nil {
		return AdminEvent{}, fmt.Errorf("%w: admin: %w", ErrMalformedPayload, err)
	}
	body, err := scval.Parse(e.Value)
	if err != nil {
		return AdminEvent{}, fmt.Errorf("%w: parse body: %w", ErrMalformedPayload, err)
	}
	asset, err := scval.AsAddressStrkey(body)
	if err != nil {
		return AdminEvent{}, fmt.Errorf("%w: asset: %w", ErrMalformedPayload, err)
	}
	return AdminEvent{
		ContractID: e.ContractID,
		Kind:       EventCancelSetReserve,
		Admin:      admin,
		Asset:      asset,
		Ledger:     e.Ledger,
		TxHash:     e.TxHash,
		OpIndex:    uint32(e.OperationIndex),
		Timestamp:  closedAt,
	}, nil
}

// decodeSetReserve parses `set_reserve`: topics [Symbol], body (asset: Address, index: u32).
func decodeSetReserve(e *events.Event, closedAt time.Time) (AdminEvent, error) {
	if len(e.Topic) != 1 {
		return AdminEvent{}, fmt.Errorf("%w: set_reserve expected 1 topic, got %d",
			ErrMalformedPayload, len(e.Topic))
	}
	body, err := scval.Parse(e.Value)
	if err != nil {
		return AdminEvent{}, fmt.Errorf("%w: parse body: %w", ErrMalformedPayload, err)
	}
	tuple, err := scval.AsTupleN(body, 2)
	if err != nil {
		return AdminEvent{}, fmt.Errorf("%w: body shape: %w", ErrMalformedPayload, err)
	}
	asset, err := scval.AsAddressStrkey(tuple[0])
	if err != nil {
		return AdminEvent{}, fmt.Errorf("%w: asset: %w", ErrMalformedPayload, err)
	}
	idx, err := scval.AsU32(tuple[1])
	if err != nil {
		return AdminEvent{}, fmt.Errorf("%w: index: %w", ErrMalformedPayload, err)
	}
	return AdminEvent{
		ContractID:   e.ContractID,
		Kind:         EventSetReserve,
		Asset:        asset,
		ReserveIndex: idx,
		Ledger:       e.Ledger,
		TxHash:       e.TxHash,
		OpIndex:      uint32(e.OperationIndex),
		Timestamp:    closedAt,
	}, nil
}

// decodeSetStatus parses `set_status`: body u32, topics [Symbol] (non-admin) or [Symbol, Address(admin)] (admin).
// Both arities are accepted; the ByAdmin flag distinguishes them.
func decodeSetStatus(e *events.Event, closedAt time.Time) (AdminEvent, error) {
	if len(e.Topic) != 1 && len(e.Topic) != 2 {
		return AdminEvent{}, fmt.Errorf("%w: set_status expected 1 or 2 topics, got %d",
			ErrMalformedPayload, len(e.Topic))
	}
	out := AdminEvent{
		ContractID: e.ContractID,
		Kind:       EventSetStatus,
		Ledger:     e.Ledger,
		TxHash:     e.TxHash,
		OpIndex:    uint32(e.OperationIndex),
		Timestamp:  closedAt,
	}
	if len(e.Topic) == 2 {
		admin, err := decodeAddressTopic(e.Topic[1])
		if err != nil {
			return AdminEvent{}, fmt.Errorf("%w: admin: %w", ErrMalformedPayload, err)
		}
		out.Admin = admin
		out.ByAdmin = true
	}
	body, err := scval.Parse(e.Value)
	if err != nil {
		return AdminEvent{}, fmt.Errorf("%w: parse body: %w", ErrMalformedPayload, err)
	}
	status, err := scval.AsU32(body)
	if err != nil {
		return AdminEvent{}, fmt.Errorf("%w: new_status: %w", ErrMalformedPayload, err)
	}
	out.NewStatus = status
	return out, nil
}

// decodeDeploy parses the pool-factory `deploy`: topics [Symbol], body Address(pool_address).
func decodeDeploy(e *events.Event, closedAt time.Time) (AdminEvent, error) {
	if len(e.Topic) != 1 {
		return AdminEvent{}, fmt.Errorf("%w: deploy expected 1 topic, got %d",
			ErrMalformedPayload, len(e.Topic))
	}
	body, err := scval.Parse(e.Value)
	if err != nil {
		return AdminEvent{}, fmt.Errorf("%w: parse body: %w", ErrMalformedPayload, err)
	}
	addr, err := scval.AsAddressStrkey(body)
	if err != nil {
		return AdminEvent{}, fmt.Errorf("%w: pool_address: %w", ErrMalformedPayload, err)
	}
	return AdminEvent{
		ContractID: e.ContractID,
		Kind:       EventDeploy,
		Target:     addr,
		Ledger:     e.Ledger,
		TxHash:     e.TxHash,
		OpIndex:    uint32(e.OperationIndex),
		Timestamp:  closedAt,
	}, nil
}

// ─── V1 pool-factory decoders ────────────────────────────────
//
// The V1 pool-factory (CCZD6ESM…) emits three topics absent from blend-contracts-v2's
// pool/src/events.rs, verified against real lake bytes (see README.md "Known gap"):
//
//	update_emissions:           1 topic  [Symbol]           body i128 (bare)
//	new_liquidation_auction:    2 topics [Symbol, Address]   body Map{bid,lot,block}
//	delete_liquidation_auction: 2 topics [Symbol, Address]   body ScvVoid
//
// update_emissions lands in blend_emissions (a pool-wide total, unlike V2's per-reserve
// reserve_emission_update). The two liquidation-auction events land in blend_admin, NOT
// blend_auctions: V1 has no auction_type topic and that column is NOT NULL, so inventing one would
// put unverified provenance in a verified-data table. bid/lot/block ride in blend_admin's jsonb
// attributes instead.

// decodeUpdateEmissions parses V1 `update_emissions`: topics [Symbol], body bare i128 (pool-wide emissions total).
func decodeUpdateEmissions(e *events.Event, closedAt time.Time) (EmissionEvent, error) {
	if len(e.Topic) != 1 {
		return EmissionEvent{}, fmt.Errorf("%w: update_emissions expected 1 topic, got %d",
			ErrMalformedPayload, len(e.Topic))
	}
	body, err := scval.Parse(e.Value)
	if err != nil {
		return EmissionEvent{}, fmt.Errorf("%w: parse body: %w", ErrMalformedPayload, err)
	}
	amt, err := scval.AsAmountFromI128(body)
	if err != nil {
		return EmissionEvent{}, fmt.Errorf("%w: update_emissions amount: %w", ErrMalformedPayload, err)
	}
	return EmissionEvent{
		Pool:      e.ContractID,
		Kind:      EventUpdateEmissions,
		Amount:    amt.BigInt(),
		Ledger:    e.Ledger,
		TxHash:    e.TxHash,
		OpIndex:   uint32(e.OperationIndex),
		Timestamp: closedAt,
	}, nil
}

// decodeNewLiquidationAuctionV1 parses V1 `new_liquidation_auction`: topics [Symbol, Address(user)], body AuctionData Map{bid, lot, block}
// as in V2 but with no auction_type topic and no percent field.
func decodeNewLiquidationAuctionV1(e *events.Event, closedAt time.Time) (AdminEvent, error) {
	if len(e.Topic) != 2 {
		return AdminEvent{}, fmt.Errorf("%w: new_liquidation_auction expected 2 topics, got %d",
			ErrMalformedPayload, len(e.Topic))
	}
	user, err := decodeAddressTopic(e.Topic[1])
	if err != nil {
		return AdminEvent{}, fmt.Errorf("%w: user: %w", ErrMalformedPayload, err)
	}
	body, err := scval.Parse(e.Value)
	if err != nil {
		return AdminEvent{}, fmt.Errorf("%w: parse body: %w", ErrMalformedPayload, err)
	}
	data, err := decodeAuctionData(body)
	if err != nil {
		return AdminEvent{}, fmt.Errorf("%w: auction_data: %w", ErrMalformedPayload, err)
	}
	return AdminEvent{
		ContractID:   e.ContractID,
		Kind:         EventNewLiquidationAuction,
		Target:       user,
		AuctionBid:   toDomainAssetAmounts(data.Bid),
		AuctionLot:   toDomainAssetAmounts(data.Lot),
		AuctionBlock: data.Block,
		Ledger:       e.Ledger,
		TxHash:       e.TxHash,
		OpIndex:      uint32(e.OperationIndex),
		Timestamp:    closedAt,
	}, nil
}

// decodeDeleteLiquidationAuctionV1 parses V1 `delete_liquidation_auction`: topics [Symbol, Address(user)], body ScvVoid (not parsed;
// only topic-derived fields matter, as with V2 delete_auction).
func decodeDeleteLiquidationAuctionV1(e *events.Event, closedAt time.Time) (AdminEvent, error) {
	if len(e.Topic) != 2 {
		return AdminEvent{}, fmt.Errorf("%w: delete_liquidation_auction expected 2 topics, got %d",
			ErrMalformedPayload, len(e.Topic))
	}
	user, err := decodeAddressTopic(e.Topic[1])
	if err != nil {
		return AdminEvent{}, fmt.Errorf("%w: user: %w", ErrMalformedPayload, err)
	}
	return AdminEvent{
		ContractID: e.ContractID,
		Kind:       EventDeleteLiquidationAuction,
		Target:     user,
		Ledger:     e.Ledger,
		TxHash:     e.TxHash,
		OpIndex:    uint32(e.OperationIndex),
		Timestamp:  closedAt,
	}, nil
}

// toDomainAssetAmounts converts to primitive-only domain types, since domain cannot import this package.
func toDomainAssetAmounts(in []AssetAmount) []domain.BlendAssetAmount {
	if len(in) == 0 {
		return nil
	}
	out := make([]domain.BlendAssetAmount, len(in))
	for i, a := range in {
		out[i] = domain.BlendAssetAmount{Asset: a.Asset.String(), Amount: a.Amount}
	}
	return out
}

// ─── ReserveConfig decoder helper ──────────────────────────────

// reserveConfigKeys mirrors pool/src/storage.rs::ReserveConfig, decoded by name. i128 becomes a decimal string (full precision),
// u32 becomes uint64 (jsonb-safe). V2Only marks fields V2 added; a V1 ReserveConfig has the other eleven.
var reserveConfigKeys = []struct {
	Name   string
	Type   string // "u32" | "i128" | "bool"
	V2Only bool
}{
	{"index", "u32", false},
	{"decimals", "u32", false},
	{"c_factor", "u32", false},
	{"l_factor", "u32", false},
	{"util", "u32", false},
	{"max_util", "u32", false},
	{"r_base", "u32", false},
	{"r_one", "u32", false},
	{"r_two", "u32", false},
	{"r_three", "u32", false},
	{"reactivity", "u32", false},
	{reserveConfigSupplyCap, "i128", true},
	{reserveConfigEnabled, "bool", true},
}

// decodeReserveConfig decodes an ScvMap ReserveConfig. A missing field both pool generations carry is an error (fail loud on upgrade);
// a missing V2-only field is omitted and named in missing so V1 events land.
func decodeReserveConfig(sv scval.ScVal) (out map[string]any, missing []string, err error) {
	entries, err := scval.AsMap(sv)
	if err != nil {
		return nil, nil, fmt.Errorf("ReserveConfig shape: %w", err)
	}
	out = make(map[string]any, len(reserveConfigKeys))
	for _, k := range reserveConfigKeys {
		val, ok := scval.MapField(entries, k.Name)
		if !ok {
			if k.V2Only {
				missing = append(missing, k.Name)
				continue
			}
			return nil, nil, fmt.Errorf("ReserveConfig missing %q", k.Name)
		}
		v, err := decodeReserveConfigField(val, k.Type)
		if err != nil {
			return nil, nil, fmt.Errorf("ReserveConfig.%s: %w", k.Name, err)
		}
		out[k.Name] = v
	}
	return out, missing, nil
}

// decodeReserveConfigField decodes one ReserveConfig value per the
// reserveConfigKeys type rules.
func decodeReserveConfigField(val scval.ScVal, typ string) (any, error) {
	switch typ {
	case "u32":
		n, err := scval.AsU32(val)
		if err != nil {
			return nil, err
		}
		return uint64(n), nil
	case "i128":
		amt, err := scval.AsAmountFromI128(val)
		if err != nil {
			return nil, err
		}
		return amt.String(), nil // i128 as decimal string
	case "bool":
		return scval.AsBool(val)
	}
	return nil, fmt.Errorf("unknown field type %q", typ)
}
