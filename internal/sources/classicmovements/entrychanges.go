package classicmovements

import (
	"errors"
	"fmt"
	"math/big"
	"time"

	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/xdrjson"
)

// ─── Phase 4 (entry-changes half): LiquidityPoolDeposit/Withdraw + ──
// ─── the CAP-0038 trustline-revocation auto-liquidation edge case ───
//
// ADR-0047 D3 Phase 4 / research §2 path (c): LiquidityPoolDepositResult
// and LiquidityPoolWithdrawResult are BARE success/failure codes with
// zero data fields — the only ground truth for the two amounts
// exchanged is the pool's LiquidityPoolEntryConstantProduct
// ReserveA/ReserveB before vs. after the op, which lives ONLY in
// ledger_entry_changes. The CAP-0038 edge case (a trustline
// revocation that deauthorizes an account holding LP-share
// trustlines mixing the revoked asset auto-redeems those shares into
// two new ClaimableBalanceEntry rows, same op_index) is the same
// story: neither AllowTrust nor SetTrustLineFlags's own body/result
// carries the liquidated amounts.
//
// This is therefore a SEPARATE decode surface from decode.go's
// Matches/decodeOp/Decoder.Decode: dispatcher.OpContext (the
// op-only surface's input) has no room for a correlated
// ledger_entry_changes group, and — per D2 — this package is never
// live-wired anyway, so there's no dispatcher.LedgerEntryChangeDecoder
// registration to conform to either (that interface delivers ONE
// change at a time per LCM, not a pre-grouped before/after pair for
// one op — a mismatch for what before/after delta computation
// needs). EntryChangeOpTypes/DecodeLiquidityPoolOp/
// DecodeCAP0038Revocation below are plain functions the caller
// (classic-movements-backfill) invokes directly, after correlating
// clickhouse.StreamEntryChanges output by (ledger, tx_hash, op_index)
// itself.
//
// # Ledger_entry_changes fidelity: BOTH available and unavailable eras
//
// research §3.2: real per-op ledger_entry_changes fidelity currently
// starts at ~ledger 61,996,000 — ALREADY PAST the P23 boundary
// (58,762,517) this package's backfill command hard-clamps to. That
// means, as of this writing, EVERY op in this decode surface's
// addressable range will find zero usable entry changes — not
// because nothing happened, but because Phase 0 (a separate,
// operator-scheduled `ch-backfill` over [38115806, 61999000]) hasn't
// run yet. Once it does, ledger_entry_changes gains real fidelity for
// the entire P18-onward range these ops need (AMMs didn't exist
// before P18, so LP correctness needs nothing earlier). The functions
// below are written and tested to be correct for BOTH eras:
//   - Fidelity absent: ErrEntryChangesUnavailable / (nil, nil,
//     "no CAP-0038 liquidation" for AllowTrust/SetTrustLineFlags),
//     counted and logged by the caller — NEVER a guessed amount.
//   - Fidelity present: correct amounts derived from the actual
//     before/after reserve deltas.
//
// The one thing NEITHER function can do on its own is distinguish
// "fidelity absent for this window" from "op genuinely had no entry
// changes" — an empty StreamEntryChanges result looks identical
// either way at the SQL layer. LiquidityPoolDeposit/Withdraw don't
// need to distinguish these (a REAL deposit/withdraw ALWAYS mutates
// the pool, so empty changes always means unavailable fidelity —
// ErrEntryChangesUnavailable is correct either way). AllowTrust/
// SetTrustLineFlags CANNOT make this assumption (the overwhelming
// majority of these ops trigger NO liquidation at all, fidelity
// present or not) — the caller MUST run a window-level fidelity
// probe (clickhouse.CountOpScopedEntryChanges) BEFORE trusting an
// empty-changes "no liquidation" result from DecodeCAP0038Revocation,
// or it will silently under-report liquidations during the
// fidelity-absent era. See classic-movements-backfill's wiring for
// the exact probe-then-decide sequence.

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

// DecodeLiquidityPoolOp reconstructs the TWO
// 'liquidity_pool_deposit'/'liquidity_pool_withdraw' movement rows
// (leg 0 = pool AssetA, leg 1 = pool AssetB) for a successful
// LiquidityPoolDeposit/Withdraw op, given its correlated
// ledger_entry_changes group. Returns ErrEntryChangesUnavailable
// (never a guessed amount) when changes has no usable liquidity_pool
// before/after pair for this op.
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

	before, haveBefore, after, haveAfter := liquidityPoolBeforeAfter(changes)
	if !haveAfter {
		return nil, fmt.Errorf("%w: ledger %d tx %s op %d", ErrEntryChangesUnavailable, ledger, txHash, opIndex)
	}
	if !haveBefore {
		// A brand-new pool (this deposit created it) — implicit
		// zero-reserve "before" is valid, not a fidelity gap.
		before = xdr.LiquidityPoolEntryConstantProduct{}
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

	before, haveBefore, after, haveAfter := liquidityPoolBeforeAfter(changes)
	// A withdraw ALWAYS acts on a pre-existing pool — unlike deposit,
	// a missing "before" here is itself a fidelity gap, not a valid
	// "new pool" case.
	if !haveBefore || !haveAfter {
		return nil, fmt.Errorf("%w: ledger %d tx %s op %d", ErrEntryChangesUnavailable, ledger, txHash, opIndex)
	}
	deltaA := before.ReserveA - after.ReserveA
	deltaB := before.ReserveB - after.ReserveB
	if deltaA <= 0 || deltaB <= 0 {
		return nil, fmt.Errorf("%w: non-positive reserve delta A=%d B=%d (ledger %d tx %s op %d)",
			ErrMalformedMovement, deltaA, deltaB, ledger, txHash, opIndex)
	}

	poolIDHex := fmt.Sprintf("%x", body.LiquidityPoolId)
	return []Movement{
		liquidityPoolLeg(KindLiquidityPoolWithdraw, ledger, closedAt, txHash, opIndex, 0,
			before.Params.AssetA, deltaA, "", fromAddr, poolIDHex),
		liquidityPoolLeg(KindLiquidityPoolWithdraw, ledger, closedAt, txHash, opIndex, 1,
			before.Params.AssetB, deltaB, "", fromAddr, poolIDHex),
	}, nil
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

// liquidityPoolBeforeAfter walks an op's correlated liquidity_pool
// entry-changes group (already in change_index order — the same
// order stellar-core's own Changes list uses, see
// clickhouse.StreamEntryChanges' doc) and extracts the before/after
// LiquidityPoolEntryConstantProduct. "before" comes from a 'state'
// row if present (haveBefore=false, not an error, for a brand-new
// pool with no prior state — 'created' rows have no preceding
// state); "after" comes from the LAST 'created'/'updated' row.
func liquidityPoolBeforeAfter(changes []EntryChangeXDR) (before xdr.LiquidityPoolEntryConstantProduct, haveBefore bool, after xdr.LiquidityPoolEntryConstantProduct, haveAfter bool) {
	for _, c := range changes {
		cp, ok := liquidityPoolConstantProduct(c.Entry)
		if !ok {
			continue
		}
		switch c.ChangeType {
		case "state":
			before = cp
			haveBefore = true
		case "created", "updated":
			after = cp
			haveAfter = true
		}
	}
	return before, haveBefore, after, haveAfter
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
// Emits movement_kind='liquidity_pool_withdraw' rows (one per created
// ClaimableBalanceEntry, i.e. one per pool asset — always two for a
// real CAP-0038 event, since it's a two-asset pool) rather than a
// dedicated kind: functionally this IS a forced LP withdrawal, just
// routed through escrow instead of directly to the trustor. Attributes
// marks provenance explicitly (revocation=true, trigger_op_type,
// claimable_balance_id) so a reader can distinguish this from an
// ordinary voluntary withdrawal.
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

	movements := make([]Movement, 0, len(created))
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
				"revocation":           true,
				"trigger_op_type":      triggerType,
				"claimable_balance_id": cb.BalanceIDHex,
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
// from changes — the CAP-0038 liquidation signal. Any non-'created'
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
	return out
}
