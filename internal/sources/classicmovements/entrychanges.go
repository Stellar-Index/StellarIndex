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
//
// ADR-0047 D3: LiquidityPoolDepositResult and LiquidityPoolWithdrawResult
// are bare result codes, so the amounts exchanged exist only as the pool's
// ReserveA/ReserveB before vs. after the op in ledger_entry_changes. The
// CAP-0038 case (a trustline revocation auto-redeeming LP shares into two
// ClaimableBalanceEntry rows, same op_index) is the same: neither
// AllowTrust nor SetTrustLineFlags carries the liquidated amounts.
//
// This is a SEPARATE surface from decode.go: dispatcher.OpContext has no
// room for a correlated change group, and dispatcher.LedgerEntryChangeDecoder
// delivers one change at a time, not a before/after pair per op. The
// functions below are plain calls made by classic-movements-backfill after
// it correlates clickhouse.StreamEntryChanges by (ledger, tx_hash, op_index).
//
// # Ledger_entry_changes fidelity: BOTH available and unavailable eras
//
// Per-op fidelity starts at ~ledger 61,996,000, past the P23 boundary
// (58,762,517) the backfill clamps to; ch-backfill over [38115806, 61999000]
// filled the gap. The functions stay correct for both eras: absent fidelity
// yields ErrEntryChangesUnavailable (or "no CAP-0038 liquidation"), counted
// by the caller — NEVER a guessed amount.
//
// An empty change set cannot tell "fidelity absent" from "no changes".
// Deposit/Withdraw always mutate the pool, so empty means unavailable.
// AllowTrust/SetTrustLineFlags usually liquidate nothing, so the caller MUST
// run clickhouse.CountOpScopedEntryChanges for the window before trusting an
// empty "no liquidation" from DecodeCAP0038Revocation, or it under-reports.

// EntryChangeOpTypes returns the entry-changes-correlated decode
// surface's op-type scope, in stellar.operations.op_type string form
// — the set clickhouse.StreamClassicOps should ALSO be called with
// (alongside SupportedOpTypes(), typically unioned into one CH read)
// so the caller has both the op bodies/results AND can correlate
// against clickhouse.StreamEntryChanges output for the same ops.
// AllowTrust/SetTrustLineFlags are here despite moving no value in
// the overwhelming majority of cases — they're in scope because they
// CAN trigger the CAP-0038 side effect, detected only by consulting
// entry changes; see recognition_test.go's
// TestRecognition_EntryChangeOpTypesIsExhaustiveAndDisjoint for the
// guard pinning this list disjoint from SupportedOpTypes().
func EntryChangeOpTypes() []string {
	return []string{
		xdr.OperationTypeLiquidityPoolDeposit.String(),
		xdr.OperationTypeLiquidityPoolWithdraw.String(),
		xdr.OperationTypeAllowTrust.String(),
		xdr.OperationTypeSetTrustLineFlags.String(),
	}
}

// ErrEntryChangesUnavailable is returned by DecodeLiquidityPoolOp
// when a successful LiquidityPoolDeposit/Withdraw has no correlated
// ledger_entry_changes to derive amounts from — either because
// ledger_entry_changes' per-op fidelity backfill (ADR-0047 Phase 0)
// hasn't reached this ledger range yet, or (far less likely, given a
// real deposit/withdraw always mutates the pool) a genuine data gap.
// The caller MUST count + log this and move on — never guess an
// amount by any other means.
var ErrEntryChangesUnavailable = errors.New("classicmovements: ledger_entry_changes unavailable for this op")

// EntryChangeXDR is one op-scoped ledger_entry_changes row, already
// correlated by the caller to a single op via
// (ledger, tx_hash, op_index) — decoupled from clickhouse.EntryChange
// so this package stays storage-agnostic (mirrors
// internal/sources/sdex never importing a storage package; the same
// design choice PendingClaimableBalanceRef made for Phase 3). Entry
// is nil for a 'removed' change (key only, no payload).
type EntryChangeXDR struct {
	ChangeType string // "state" | "created" | "updated" | "removed"
	Entry      *xdr.LedgerEntry
}

// DecodeLiquidityPoolOp reconstructs the
// 'liquidity_pool_deposit'/'liquidity_pool_withdraw' movement rows
// (leg 0 = pool AssetA, leg 1 = pool AssetB) for a successful
// LiquidityPoolDeposit/Withdraw op, given its correlated
// ledger_entry_changes group. Two rows for the ordinary case; a
// withdraw whose payout rounds to zero on one leg emits only the
// paying leg (see decodeLiquidityPoolWithdraw). Returns
// ErrEntryChangesUnavailable (never a guessed amount) when changes has
// no usable liquidity_pool before/after pair for this op.
//
// from/to framing: a deposit moves value FROM the depositor
// (fromAddr) INTO the pool (no G-address — ToAddress left empty, the
// same "no single resolvable address" convention
// claimable_balance_create's escrow leg uses); a withdraw is the
// reverse (FromAddress empty, ToAddress = fromAddr). Attributes
// always carries pool_id (hex of the PoolId, same convention
// internal/sources/sdex's LiquidityPool ClaimAtom Maker field uses)
// for cross-referencing against SDEX's trade-side rows for the same
// pool.
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
		// A brand-new pool (this deposit created it) — implicit
		// zero-reserve "before" is valid, not a fidelity gap.
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
	// A withdraw ALWAYS acts on a pre-existing pool — unlike deposit,
	// a missing "before" here is itself a fidelity gap, not a valid
	// "new pool" case.
	if !v.haveBefore {
		return nil, fmt.Errorf("%w: ledger %d tx %s op %d", ErrEntryChangesUnavailable, ledger, txHash, opIndex)
	}
	before, after := v.before, v.after
	if !v.haveAfter {
		if !v.removed {
			return nil, fmt.Errorf("%w: ledger %d tx %s op %d", ErrEntryChangesUnavailable, ledger, txHash, opIndex)
		}
		// Full drain: the last pool shares were withdrawn, so core
		// ERASED the LiquidityPoolEntry — the group is 'state' +
		// 'removed' with no 'updated' row, and 'removed' carries a key
		// only (Entry nil). The after-reserves are zero BY
		// CONSTRUCTION, so this is a complete observation, not a
		// fidelity gap: treating it as ErrEntryChangesUnavailable both
		// dropped a real two-leg withdrawal and fired a false
		// "entry-changes unavailable" fidelity alarm at the caller.
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

	// A single zero leg is a VALID one-sided withdrawal, not a
	// malformed op: core computes each leg as
	// floor(shares * reserve / totalShares) with ROUND_DOWN and only
	// rejects it against the caller's minAmountA/minAmountB, so a small
	// withdrawal from a lopsided pool legitimately pays out on one
	// asset and rounds the other to zero. Emit the paying leg(s) and
	// drop only the zero one — LegIndex still identifies the pool asset
	// (0=AssetA, 1=AssetB), so the surviving leg keeps its true index.
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

// negativeReserve reports a reserve core can never produce; rejecting it
// also keeps the int64 reserve deltas from wrapping into a positive leg.
func negativeReserve(sides ...xdr.LiquidityPoolEntryConstantProduct) bool {
	for _, s := range sides {
		if s.ReserveA < 0 || s.ReserveB < 0 {
			return true
		}
	}
	return false
}

// liquidityPoolLeg builds one leg of a two-leg LiquidityPoolDeposit/
// Withdraw movement (leg 0 = pool AssetA, leg 1 = pool AssetB). The
// CAP-0038 revocation path (DecodeCAP0038Revocation) does NOT use
// this helper — its rows come from created ClaimableBalanceEntry
// data, not a pool reserve delta, so it builds its own Movement
// literal with a different Attributes shape (revocation provenance
// instead of pool_id).
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

// lpEntryChangeView is one op's correlated liquidity_pool
// entry-changes group reduced to the three facts the deposit/withdraw
// decoders need: the pool state before the op, the pool state after
// it, and whether the op ERASED the pool entry outright.
type lpEntryChangeView struct {
	before     xdr.LiquidityPoolEntryConstantProduct
	haveBefore bool
	after      xdr.LiquidityPoolEntryConstantProduct
	haveAfter  bool
	removed    bool
}

// liquidityPoolBeforeAfter walks an op's correlated liquidity_pool
// entry-changes group (already in change_index order — the same
// order stellar-core's own Changes list uses, see
// clickhouse.StreamEntryChanges' doc) and extracts the before/after
// LiquidityPoolEntryConstantProduct. "before" comes from a 'state'
// row if present (haveBefore=false, not an error, for a brand-new
// pool with no prior state — 'created' rows have no preceding
// state); "after" comes from the LAST 'created'/'updated' row.
//
// removed reports a 'removed' row in the group: core erases the
// LiquidityPoolEntry when the last shares are withdrawn, emitting
// 'state' + 'removed' and NO 'updated' row. That row carries a
// LedgerKey only (Entry nil), so it can't be read as an "after" — but
// it is not a missing observation either: the caller reads it as a
// zero-reserve after (see decodeLiquidityPoolWithdraw). The group is
// already scoped to one op and filtered to entry_type='liquidity_pool'
// by the caller, so any 'removed' row here is this pool's.
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

// liquidityPoolConstantProduct extracts the ConstantProduct body
// from a decoded LedgerEntry, false for anything else (nil entry,
// wrong entry type, or a future non-ConstantProduct pool type — none
// exist in current XDR, but this fails closed rather than panicking
// if one is ever added).
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

// DecodeCAP0038Revocation checks whether a successful AllowTrust /
// SetTrustLineFlags op triggered CAP-0038's automatic
// liquidity-pool-share liquidation side effect, detected PURELY from
// correlated entry changes (created claimable_balance rows at this
// op's index — the op body alone can't tell us whether the targeted
// account actually held a matching LP-share trustline at the time).
// Returns ZERO movements for the overwhelmingly common case (no
// liquidation) — this is NOT an error and NOT
// ErrEntryChangesUnavailable, unlike LiquidityPoolDeposit/Withdraw:
// an empty changes group here is the EXPECTED steady state, so
// callers must run their own window-level fidelity probe
// (clickhouse.CountOpScopedEntryChanges) before trusting "zero
// movements" as "definitely no liquidation happened" rather than
// "can't tell, fidelity is absent" — see this file's package-level
// doc comment.
//
// Emits TWO rows per created ClaimableBalanceEntry (one per pool asset,
// so four for a real two-asset CAP-0038 event): a
// movement_kind='liquidity_pool_withdraw' row (functionally a forced LP
// withdrawal, routed through escrow) at leg_index 0..n-1, and a
// 'claimable_balance_create' row for the same balance at leg_index
// n..2n-1 so a later claim/clawback resolves. Both carry
// revocation=true, trigger_op_type and balance_id so a reader can
// distinguish them from voluntary withdrawals and explicit creates.
//
// FromAddress is the Trustor (the account whose position was
// liquidated) — NOT ctx.TxSource (typically the issuer submitting
// the revocation, a different account). ToAddress is left empty:
// funds land in a claimable balance, not directly deliverable, same
// convention as claimable_balance_create's escrow leg.
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

	// TWO movements per liquidated asset, because two things genuinely
	// happened on-chain: the trustor exited the pool, AND a claimable
	// balance was created to hold the proceeds.
	//
	// Emitting only the pool-exit leg (the original shape) made the
	// created balance UNRESOLVABLE, and not for one reason but three:
	// the movement was tagged KindLiquidityPoolWithdraw
	// so the resolver's `Kind == KindClaimableBalanceCreate` index gate
	// skipped it; the id lived under `claimable_balance_id` while every
	// other site — and the ClickHouse lookup's external table — keys on
	// `balance_id`; and cbLookupCreatesQuery filters
	// `movement_kind = 'claimable_balance_create'`, structurally
	// excluding it. A later legitimate claim or clawback against that
	// balance therefore resolved to nothing and was dropped, silently and
	// permanently, even though the asset/amount/id were all known here.
	//
	// Fixing it by broadening the ClickHouse predicate was the obvious
	// route and is the wrong one: that `movement_kind` filter is a
	// LowCardinality PREWHERE doing real work — it scopes the semijoin to
	// the cb-create rows (~2.5 min over 695M rows at 4 threads, per its
	// own measurement). Widening it to include every liquidity-pool
	// withdraw would regress the hot path to fix a rare case. Emitting the
	// row that actually describes reality costs nothing on the read side
	// and needs no query change at all.
	//
	// LegIndex: the pool-exit legs take [0, len(created)) and the create
	// legs follow above that range rather than interleaved. Both are numbered
	// in balance-id order (createdClaimableBalances), never change order.
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
				// Canonical key, matching every other emitter and the
				// ClickHouse external table. `claimable_balance_id` is kept
				// alongside it because older rows carry
				// only that spelling.
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
				// Marks this create as protocol-derived rather than the
				// result of an explicit CreateClaimableBalance op, so the
				// two are distinguishable downstream.
				"revocation":      true,
				"trigger_op_type": triggerType,
			},
		})
	}
	return movements, nil
}

// trustFlagOpSuccess reports whether op (AllowTrust or
// SetTrustLineFlags) succeeded and, if so, returns its Trustor's
// address and a stable "trigger_op_type" label. ok=false + err=nil
// means the op failed (routine, not an error); a non-nil err means
// op.Body's own type-specific field was missing despite a success
// result (a genuine malformed-data signal).
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

// createdClaimableBalanceRef is one CAP-0038-liquidated leg, decoded
// from a 'created' claimable_balance entry change.
type createdClaimableBalanceRef struct {
	Asset        string
	Amount       canonical.Amount
	BalanceIDHex string
}

// createdClaimableBalances extracts every 'created' ClaimableBalanceEntry
// from changes — the CAP-0038 liquidation signal — sorted by balance id.
// The order is leg_index, part of account_movements' key, and change order
// within an op differs between exports of the same ledger. Any non-'created'
// claimable_balance change (a genuine ClaimClaimableBalance/
// ClawbackClaimableBalance at the SAME op_index would be a protocol
// impossibility — AllowTrust/SetTrustLineFlags never claim/clawback)
// is ignored rather than erroring, since only 'created' rows are ever
// expected here.
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
