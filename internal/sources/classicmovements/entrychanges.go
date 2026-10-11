package classicmovements

import (
	"errors"
	"fmt"
	"math/big"
	"slices"
	"strings"
	"time"

	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/xdrjson"
)

// ─── Entry-changes half: LiquidityPoolDeposit/Withdraw + ────────────
// ─── the CAP-0038 trustline-revocation auto-liquidation edge case ───
// LiquidityPoolDeposit/WithdrawResult are bare result codes, so the amounts exchanged exist only
// as the pool's ReserveA/ReserveB before vs. after the op in ledger_entry_changes. The CAP-0038
// case (a revocation auto-redeeming LP shares into two ClaimableBalanceEntry rows) is the same.
//
// This is a SEPARATE surface from decode.go because dispatcher.OpContext has no room for a
// correlated change group: the backfill correlates clickhouse.StreamEntryChanges by
// (ledger, tx_hash, op_index) and calls in here.
//
// # Ledger_entry_changes fidelity
// Per-op fidelity starts at ~ledger 61,996,000. Absent fidelity yields ErrEntryChangesUnavailable
// (or "no CAP-0038 liquidation"), counted by the caller, NEVER a guessed amount. Deposit/Withdraw
// always mutate the pool, so empty means unavailable. For AllowTrust/SetTrustLineFlags the caller
// MUST run clickhouse.CountOpScopedEntryChanges for the window first.

// EntryChangeOpTypes returns the entry-changes-correlated op-type scope in
// stellar.operations.op_type form, to be read alongside SupportedOpTypes(). AllowTrust and
// SetTrustLineFlags are included although they rarely move value, because they can trigger the
// CAP-0038 side effect, visible only in entry changes.
// TestRecognition_EntryChangeOpTypesIsExhaustiveAndDisjoint pins this list disjoint from
// SupportedOpTypes().
func EntryChangeOpTypes() []string {
	return []string{
		xdr.OperationTypeLiquidityPoolDeposit.String(),
		xdr.OperationTypeLiquidityPoolWithdraw.String(),
		xdr.OperationTypeAllowTrust.String(),
		xdr.OperationTypeSetTrustLineFlags.String(),
	}
}

// ErrEntryChangesUnavailable is returned by DecodeLiquidityPoolOp when a successful
// deposit/withdraw has no correlated ledger_entry_changes to derive amounts from: the per-op
// fidelity backfill has not reached this range, or (unlikely) a genuine data gap. The caller MUST
// count and log it, never guess an amount.
var ErrEntryChangesUnavailable = errors.New("classicmovements: ledger_entry_changes unavailable for this op")

// EntryChangeXDR is one op-scoped ledger_entry_changes row, already correlated by the caller to a
// single op via (ledger, tx_hash, op_index). It is decoupled from clickhouse.EntryChange to keep
// this package storage-agnostic. Entry is nil for a 'removed' change (key only).
type EntryChangeXDR struct {
	ChangeType string // "state" | "created" | "updated" | "removed"
	Entry      *xdr.LedgerEntry
}

// DecodeLiquidityPoolOp reconstructs the 'liquidity_pool_deposit'/'liquidity_pool_withdraw'
// rows (leg 0 = pool AssetA, leg 1 = pool AssetB) for a successful op from its correlated
// ledger_entry_changes group. A withdraw that rounds to zero on one leg emits only the paying
// leg. Returns ErrEntryChangesUnavailable, never a guessed amount, when changes has no usable
// liquidity_pool before/after pair.
//
// A deposit moves value from fromAddr into the pool (ToAddress empty: the pool has no G-address);
// a withdraw is the reverse. Attributes carries pool_id (hex, as sdex's LiquidityPool Maker field).
func DecodeLiquidityPoolOp(ledger uint32, closedAt time.Time, txHash string, opIndex uint32, fromAddr string, op xdr.Operation, result xdr.OperationResult, changes []EntryChangeXDR) ([]Movement, error) {
	switch op.Body.Type {
	case xdr.OperationTypeLiquidityPoolDeposit:
		return decodeLiquidityPoolDeposit(ledger, closedAt, txHash, opIndex, fromAddr, op, result, changes)
	case xdr.OperationTypeLiquidityPoolWithdraw:
		return decodeLiquidityPoolWithdraw(ledger, closedAt, txHash, opIndex, fromAddr, op, result, changes)
	default:
		return nil, fmt.Errorf("classicmovements: DecodeLiquidityPoolOp called with non-LP op type %s", op.Body.Type)
	}
}

func decodeLiquidityPoolDeposit(ledger uint32, closedAt time.Time, txHash string, opIndex uint32, fromAddr string, op xdr.Operation, result xdr.OperationResult, changes []EntryChangeXDR) ([]Movement, error) {
	if !opSucceeded(result) {
		return nil, nil
	}
	tr, ok := result.GetTr()
	if !ok {
		return nil, nil
	}
	r, ok := tr.GetLiquidityPoolDepositResult()
	if !ok || r.Code != xdr.LiquidityPoolDepositResultCodeLiquidityPoolDepositSuccess {
		return nil, nil
	}
	body, ok := op.Body.GetLiquidityPoolDepositOp()
	if !ok {
		return nil, fmt.Errorf("%w: op type LiquidityPoolDeposit but body has no LiquidityPoolDepositOp (ledger %d tx %s op %d)",
			ErrMalformedMovement, ledger, txHash, opIndex)
	}

	v := liquidityPoolBeforeAfter(changes)
	if !v.haveAfter {
		return nil, fmt.Errorf("%w: ledger %d tx %s op %d", ErrEntryChangesUnavailable, ledger, txHash, opIndex)
	}
	before, after := v.before, v.after
	if !v.haveBefore {
		// A brand-new pool: an implicit zero-reserve "before" is valid, not a fidelity gap.
		before = xdr.LiquidityPoolEntryConstantProduct{}
	}
	if negativeReserve(before, after) {
		return nil, fmt.Errorf("%w: negative pool reserve (ledger %d tx %s op %d)",
			ErrMalformedMovement, ledger, txHash, opIndex)
	}
	deltaA := after.ReserveA - before.ReserveA
	deltaB := after.ReserveB - before.ReserveB
	if deltaA <= 0 || deltaB <= 0 {
		return nil, fmt.Errorf("%w: non-positive reserve delta A=%d B=%d (ledger %d tx %s op %d)",
			ErrMalformedMovement, deltaA, deltaB, ledger, txHash, opIndex)
	}

	poolIDHex := fmt.Sprintf("%x", body.LiquidityPoolId)
	return []Movement{
		liquidityPoolLeg(KindLiquidityPoolDeposit, ledger, closedAt, txHash, opIndex, 0,
			after.Params.AssetA, deltaA, fromAddr, "", poolIDHex),
		liquidityPoolLeg(KindLiquidityPoolDeposit, ledger, closedAt, txHash, opIndex, 1,
			after.Params.AssetB, deltaB, fromAddr, "", poolIDHex),
	}, nil
}

func decodeLiquidityPoolWithdraw(ledger uint32, closedAt time.Time, txHash string, opIndex uint32, fromAddr string, op xdr.Operation, result xdr.OperationResult, changes []EntryChangeXDR) ([]Movement, error) {
	if !opSucceeded(result) {
		return nil, nil
	}
	tr, ok := result.GetTr()
	if !ok {
		return nil, nil
	}
	r, ok := tr.GetLiquidityPoolWithdrawResult()
	if !ok || r.Code != xdr.LiquidityPoolWithdrawResultCodeLiquidityPoolWithdrawSuccess {
		return nil, nil
	}
	body, ok := op.Body.GetLiquidityPoolWithdrawOp()
	if !ok {
		return nil, fmt.Errorf("%w: op type LiquidityPoolWithdraw but body has no LiquidityPoolWithdrawOp (ledger %d tx %s op %d)",
			ErrMalformedMovement, ledger, txHash, opIndex)
	}

	v := liquidityPoolBeforeAfter(changes)
	// A withdraw always acts on an existing pool, so a missing "before" is a fidelity gap.
	if !v.haveBefore {
		return nil, fmt.Errorf("%w: ledger %d tx %s op %d", ErrEntryChangesUnavailable, ledger, txHash, opIndex)
	}
	before, after := v.before, v.after
	if !v.haveAfter {
		if !v.removed {
			return nil, fmt.Errorf("%w: ledger %d tx %s op %d", ErrEntryChangesUnavailable, ledger, txHash, opIndex)
		}
		// Full drain: core erased the LiquidityPoolEntry, leaving 'state' + 'removed' with no
		// 'updated' row. After-reserves are zero by construction, so this is a complete
		// observation; calling it unavailable would drop a real withdrawal and raise a false
		// fidelity alarm.
		after = xdr.LiquidityPoolEntryConstantProduct{}
	}
	if negativeReserve(before, after) {
		return nil, fmt.Errorf("%w: negative pool reserve (ledger %d tx %s op %d)",
			ErrMalformedMovement, ledger, txHash, opIndex)
	}
	deltaA := before.ReserveA - after.ReserveA
	deltaB := before.ReserveB - after.ReserveB
	if deltaA < 0 || deltaB < 0 || (deltaA == 0 && deltaB == 0) {
		return nil, fmt.Errorf("%w: non-positive reserve delta A=%d B=%d (ledger %d tx %s op %d)",
			ErrMalformedMovement, deltaA, deltaB, ledger, txHash, opIndex)
	}

	// A single zero leg is a valid one-sided withdrawal: core floors each leg, so a small
	// withdrawal from a lopsided pool rounds one asset to zero. Drop only the zero leg; the
	// surviving leg keeps its true LegIndex (0=AssetA, 1=AssetB).
	poolIDHex := fmt.Sprintf("%x", body.LiquidityPoolId)
	movements := make([]Movement, 0, 2)
	if deltaA > 0 {
		movements = append(movements, liquidityPoolLeg(KindLiquidityPoolWithdraw, ledger, closedAt, txHash, opIndex, 0,
			before.Params.AssetA, deltaA, "", fromAddr, poolIDHex))
	}
	if deltaB > 0 {
		movements = append(movements, liquidityPoolLeg(KindLiquidityPoolWithdraw, ledger, closedAt, txHash, opIndex, 1,
			before.Params.AssetB, deltaB, "", fromAddr, poolIDHex))
	}
	return movements, nil
}

// negativeReserve reports a reserve core can never produce; rejecting it keeps the int64 deltas
// from wrapping into a positive leg.
func negativeReserve(sides ...xdr.LiquidityPoolEntryConstantProduct) bool {
	for _, s := range sides {
		if s.ReserveA < 0 || s.ReserveB < 0 {
			return true
		}
	}
	return false
}

// liquidityPoolLeg builds one leg of a LiquidityPoolDeposit/Withdraw movement (leg 0 = AssetA,
// leg 1 = AssetB). DecodeCAP0038Revocation does not use it: its rows come from created
// ClaimableBalanceEntry data and carry revocation attributes instead of pool_id.
func liquidityPoolLeg(kind Kind, ledger uint32, closedAt time.Time, txHash string, opIndex, legIndex uint32, asset xdr.Asset, amount xdr.Int64, fromAddr, toAddr, poolIDHex string) Movement {
	return Movement{
		Kind:            kind,
		Provenance:      ProvenanceClassicDerived,
		Ledger:          ledger,
		LedgerCloseTime: closedAt,
		TxHash:          txHash,
		OpIndex:         opIndex,
		LegIndex:        legIndex,
		Asset:           xdrjson.AssetID(asset),
		Amount:          canonical.NewAmount(big.NewInt(int64(amount))),
		FromAddress:     fromAddr,
		ToAddress:       toAddr,
		Attributes:      map[string]any{"pool_id": poolIDHex},
	}
}

// lpEntryChangeView reduces one op's liquidity_pool entry-changes group to the pool state before
// and after the op, and whether the op ERASED the pool entry outright.
type lpEntryChangeView struct {
	before     xdr.LiquidityPoolEntryConstantProduct
	haveBefore bool
	after      xdr.LiquidityPoolEntryConstantProduct
	haveAfter  bool
	removed    bool
}

// liquidityPoolBeforeAfter walks an op's liquidity_pool entry-changes group (in change_index
// order, as stellar-core emits it) and extracts the before/after constant-product state. "before"
// comes from a 'state' row (absent for a brand-new pool); "after" from the LAST 'created' or
// 'updated' row.
//
// removed reports a 'removed' row: core erases the entry when the last shares are withdrawn
// ('state' + 'removed', no 'updated'). It has a key only (Entry nil), so it is not an "after", but
// the caller reads it as a zero-reserve after. The group is scoped to one op and
// entry_type='liquidity_pool', so any 'removed' row is this pool's.
func liquidityPoolBeforeAfter(changes []EntryChangeXDR) lpEntryChangeView {
	var v lpEntryChangeView
	for _, c := range changes {
		if c.ChangeType == "removed" {
			v.removed = true
			continue
		}
		cp, ok := liquidityPoolConstantProduct(c.Entry)
		if !ok {
			continue
		}
		switch c.ChangeType {
		case "state":
			v.before = cp
			v.haveBefore = true
		case "created", "updated":
			v.after = cp
			v.haveAfter = true
		}
	}
	return v
}

// liquidityPoolConstantProduct extracts the ConstantProduct body from a LedgerEntry, false for
// anything else. It fails closed rather than panicking if a non-ConstantProduct pool type is ever
// added.
func liquidityPoolConstantProduct(e *xdr.LedgerEntry) (xdr.LiquidityPoolEntryConstantProduct, bool) {
	if e == nil {
		return xdr.LiquidityPoolEntryConstantProduct{}, false
	}
	lp, ok := e.Data.GetLiquidityPool()
	if !ok {
		return xdr.LiquidityPoolEntryConstantProduct{}, false
	}
	if lp.Body.Type != xdr.LiquidityPoolTypeLiquidityPoolConstantProduct || lp.Body.ConstantProduct == nil {
		return xdr.LiquidityPoolEntryConstantProduct{}, false
	}
	return *lp.Body.ConstantProduct, true
}

// DecodeCAP0038Revocation checks whether a successful AllowTrust / SetTrustLineFlags op triggered
// CAP-0038's automatic LP-share liquidation, detected only from created claimable_balance rows at
// this op's index (the op body cannot say whether the account held an LP-share trustline). The
// common no-liquidation case returns zero movements, not an error and not
// ErrEntryChangesUnavailable. Callers must run the window-level fidelity probe
// (clickhouse.CountOpScopedEntryChanges) before reading zero as "no liquidation".
//
// Emits TWO rows per created ClaimableBalanceEntry: a 'liquidity_pool_withdraw' row at leg_index
// 0..n-1 and a 'claimable_balance_create' row for the same balance at n..2n-1 so a later
// claim/clawback resolves. Both carry revocation=true, trigger_op_type and balance_id to tell them
// from voluntary withdrawals and creates.
//
// FromAddress is the Trustor (the liquidated account), NOT the tx source (typically the issuer).
// ToAddress is empty because funds land in a claimable balance.
func DecodeCAP0038Revocation(ledger uint32, closedAt time.Time, txHash string, opIndex uint32, op xdr.Operation, result xdr.OperationResult, changes []EntryChangeXDR) ([]Movement, error) {
	if !opSucceeded(result) {
		return nil, nil
	}
	trustor, triggerType, ok, err := trustFlagOpSuccess(op, result)
	if err != nil {
		return nil, fmt.Errorf("%w: %w (ledger %d tx %s op %d)", ErrMalformedMovement, err, ledger, txHash, opIndex)
	}
	if !ok {
		return nil, nil
	}

	created := createdClaimableBalances(changes)
	if len(created) == 0 {
		return nil, nil // the common case: no CAP-0038 liquidation triggered here
	}

	// TWO movements per liquidated asset: the trustor exited the pool AND a claimable balance
	// was created to hold the proceeds.
	//
	// A pool-exit leg alone leaves the created balance unresolvable: the resolver's
	// `Kind == KindClaimableBalanceCreate` index gate skips it and cbLookupCreatesQuery filters
	// `movement_kind = 'claimable_balance_create'`, so a later claim or clawback would be dropped
	// silently. Broadening the ClickHouse predicate is the wrong fix: the `movement_kind` filter is
	// a LowCardinality PREWHERE that keeps the semijoin small (~2.5 min over 695M rows).
	//
	// LegIndex: pool-exit legs take [0, len(created)) and the create legs follow, both in
	// balance-id order (createdClaimableBalances), never change order.
	movements := make([]Movement, 0, 2*len(created))
	for i, cb := range created {
		movements = append(movements, Movement{
			Kind:            KindLiquidityPoolWithdraw,
			Provenance:      ProvenanceClassicDerived,
			Ledger:          ledger,
			LedgerCloseTime: closedAt,
			TxHash:          txHash,
			OpIndex:         opIndex,
			LegIndex:        uint32(i), //nolint:gosec // len(created) is at most a handful of pool assets, never near uint32 overflow.
			Asset:           cb.Asset,
			Amount:          cb.Amount,
			FromAddress:     trustor,
			ToAddress:       "",
			Attributes: map[string]any{
				"revocation":      true,
				"trigger_op_type": triggerType,
				// Canonical key, matching every other emitter and the ClickHouse external
				// table; `claimable_balance_id` stays because older rows carry only that.
				"balance_id":           cb.BalanceIDHex,
				"claimable_balance_id": cb.BalanceIDHex,
			},
		})
	}
	for i, cb := range created {
		movements = append(movements, Movement{
			Kind:            KindClaimableBalanceCreate,
			Provenance:      ProvenanceClassicDerived,
			Ledger:          ledger,
			LedgerCloseTime: closedAt,
			TxHash:          txHash,
			OpIndex:         opIndex,
			LegIndex:        uint32(len(created) + i), //nolint:gosec // a handful of pool assets; never near uint32 overflow.
			Asset:           cb.Asset,
			Amount:          cb.Amount,
			FromAddress:     trustor,
			ToAddress:       "",
			Attributes: map[string]any{
				"balance_id": cb.BalanceIDHex,
				// revocation marks this create as protocol-derived, not an explicit
				// CreateClaimableBalance op.
				"revocation":      true,
				"trigger_op_type": triggerType,
			},
		})
	}
	return movements, nil
}

// trustFlagOpSuccess reports whether an AllowTrust or SetTrustLineFlags op succeeded and returns
// its Trustor address and a stable "trigger_op_type" label. ok=false with nil err is a routine
// failed op; a non-nil err means the body was missing despite a success result (malformed data).
func trustFlagOpSuccess(op xdr.Operation, result xdr.OperationResult) (trustor, triggerType string, ok bool, err error) {
	tr, hasTr := result.GetTr()
	if !hasTr {
		return "", "", false, nil
	}
	switch op.Body.Type {
	case xdr.OperationTypeAllowTrust:
		r, rok := tr.GetAllowTrustResult()
		if !rok || r.Code != xdr.AllowTrustResultCodeAllowTrustSuccess {
			return "", "", false, nil
		}
		body, bok := op.Body.GetAllowTrustOp()
		if !bok {
			return "", "", false, errors.New("op type AllowTrust but body has no AllowTrustOp")
		}
		return body.Trustor.Address(), "allow_trust", true, nil
	case xdr.OperationTypeSetTrustLineFlags:
		r, rok := tr.GetSetTrustLineFlagsResult()
		if !rok || r.Code != xdr.SetTrustLineFlagsResultCodeSetTrustLineFlagsSuccess {
			return "", "", false, nil
		}
		body, bok := op.Body.GetSetTrustLineFlagsOp()
		if !bok {
			return "", "", false, errors.New("op type SetTrustLineFlags but body has no SetTrustLineFlagsOp")
		}
		return body.Trustor.Address(), "set_trustline_flags", true, nil
	default:
		return "", "", false, fmt.Errorf("trustFlagOpSuccess called with unsupported op type %s", op.Body.Type)
	}
}

// createdClaimableBalanceRef is one CAP-0038-liquidated leg from a 'created' claimable_balance change.
type createdClaimableBalanceRef struct {
	Asset        string
	Amount       canonical.Amount
	BalanceIDHex string
}

// createdClaimableBalances extracts every 'created' ClaimableBalanceEntry (the CAP-0038
// liquidation signal), sorted by balance id. The order becomes leg_index, part of
// account_movements' key, and change order within an op differs between exports of the same
// ledger. Non-'created' changes are ignored: AllowTrust/SetTrustLineFlags never claim or claw
// back, so only 'created' rows are expected.
func createdClaimableBalances(changes []EntryChangeXDR) []createdClaimableBalanceRef {
	var out []createdClaimableBalanceRef
	for _, c := range changes {
		if c.ChangeType != "created" || c.Entry == nil {
			continue
		}
		cb, ok := c.Entry.Data.GetClaimableBalance()
		if !ok {
			continue
		}
		idHex, err := claimableBalanceIDHex(cb.BalanceId)
		if err != nil {
			continue
		}
		out = append(out, createdClaimableBalanceRef{
			Asset:        xdrjson.AssetID(cb.Asset),
			Amount:       canonical.NewAmount(big.NewInt(int64(cb.Amount))),
			BalanceIDHex: idHex,
		})
	}
	slices.SortFunc(out, func(a, b createdClaimableBalanceRef) int { return strings.Compare(a.BalanceIDHex, b.BalanceIDHex) })
	return out
}
