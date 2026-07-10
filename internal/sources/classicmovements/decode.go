package classicmovements

import (
	"encoding/hex"
	"fmt"
	"math/big"
	"time"

	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/StellarIndex/stellar-index/internal/canonical"
	"github.com/StellarIndex/stellar-index/internal/xdrjson"
)

// SupportedOpTypes returns this package's op-only decode scope (the
// Decoder.Decode / matchesSupportedOp / decodeOp surface) in
// stellar.operations.op_type string form (xdr.OperationType.String())
// — the exact set clickhouse.StreamClassicOps should be called with
// for that surface. Grows with each phase: Phase 1 (CreateAccount,
// Payment), Phase 2 adds PathPaymentStrictReceive/Send, Phase 3 adds
// the ClaimableBalance trio + Clawback, Phase 4 adds AccountMerge —
// the LAST op-only-surface type; LiquidityPoolDeposit/Withdraw and
// the CAP-0038 AllowTrust/SetTrustLineFlags edge case are a SEPARATE
// entry-changes-correlated surface (see EntryChangeOpTypes in
// entrychanges.go). See recognition_test.go for the guard that pins
// this list.
func SupportedOpTypes() []string {
	return []string{
		xdr.OperationTypeCreateAccount.String(),
		xdr.OperationTypePayment.String(),
		xdr.OperationTypePathPaymentStrictReceive.String(),
		xdr.OperationTypePathPaymentStrictSend.String(),
		xdr.OperationTypeCreateClaimableBalance.String(),
		xdr.OperationTypeClaimClaimableBalance.String(),
		xdr.OperationTypeClawbackClaimableBalance.String(),
		xdr.OperationTypeClawback.String(),
		xdr.OperationTypeAccountMerge.String(),
	}
}

// matchesSupportedOp reports whether op is one of this package's
// op-only in-scope classic operation types (was matchesPhase1Op
// through Phase 1; renamed once Phase 2 made "Phase1" inaccurate —
// see doc.go). See recognition_test.go for the exhaustive-enum guard
// that pins this switch.
func matchesSupportedOp(op xdr.Operation) bool {
	switch op.Body.Type {
	case xdr.OperationTypeCreateAccount, xdr.OperationTypePayment,
		xdr.OperationTypePathPaymentStrictReceive, xdr.OperationTypePathPaymentStrictSend,
		xdr.OperationTypeCreateClaimableBalance, xdr.OperationTypeClaimClaimableBalance,
		xdr.OperationTypeClawbackClaimableBalance, xdr.OperationTypeClawback,
		xdr.OperationTypeAccountMerge:
		return true
	}
	return false
}

// decodeOp is the op-only-surface dispatch: given one classic op +
// its result + tx-level context, emit zero or more Movements. Zero
// movements is not an error — a failed op (result.Code != OpInner,
// or the operation's own result-union arm isn't the Success case)
// simply never happened and moved nothing (D1's "success-code-
// filtered" rule); a claim/clawback whose create can't (yet) be
// resolved is ALSO zero movements, not an error — see
// d.decodeClaimClaimableBalance / d.decodeClawbackClaimableBalance.
// An out-of-scope op type — one matchesSupportedOp would reject — is
// a loud ErrUnsupportedOpType instead of a silent zero-movements
// return; see that sentinel's doc comment for why. A method (not a
// free function, unlike Phase 1-2's arms) because the ClaimableBalance
// claim/clawback arms need read/write access to d's in-run BalanceId
// index.
func (d *Decoder) decodeOp(ledger uint32, closedAt time.Time, txHash string, opIndex uint32, fromAddr string, op xdr.Operation, result xdr.OperationResult) ([]Movement, error) {
	switch op.Body.Type {
	case xdr.OperationTypeCreateAccount:
		return decodeCreateAccount(ledger, closedAt, txHash, opIndex, fromAddr, op, result)
	case xdr.OperationTypePayment:
		return decodePayment(ledger, closedAt, txHash, opIndex, fromAddr, op, result)
	case xdr.OperationTypePathPaymentStrictReceive:
		return decodePathPaymentStrictReceive(ledger, closedAt, txHash, opIndex, fromAddr, op, result)
	case xdr.OperationTypePathPaymentStrictSend:
		return decodePathPaymentStrictSend(ledger, closedAt, txHash, opIndex, fromAddr, op, result)
	case xdr.OperationTypeCreateClaimableBalance:
		return decodeCreateClaimableBalance(ledger, closedAt, txHash, opIndex, fromAddr, op, result)
	case xdr.OperationTypeClaimClaimableBalance:
		return d.decodeClaimClaimableBalance(ledger, closedAt, txHash, opIndex, fromAddr, op, result)
	case xdr.OperationTypeClawbackClaimableBalance:
		return d.decodeClawbackClaimableBalance(ledger, closedAt, txHash, opIndex, fromAddr, op, result)
	case xdr.OperationTypeClawback:
		return decodeClawback(ledger, closedAt, txHash, opIndex, fromAddr, op, result)
	case xdr.OperationTypeAccountMerge:
		return decodeAccountMerge(ledger, closedAt, txHash, opIndex, fromAddr, op, result)
	default:
		return nil, fmt.Errorf("%w: %s", ErrUnsupportedOpType, op.Body.Type)
	}
}

// decodeCreateAccount reconstructs a 'create_account' movement:
// source -> new account, amount = StartingBalance, asset always
// native (CreateAccountOp carries no asset field — XLM is the only
// asset an account can be funded with at creation). Research §2
// path (a): the amount lives in the op BODY and
// CreateAccountResult is a bare success/failure code with no
// further data, so a success code alone is sufficient to trust the
// body's StartingBalance.
func decodeCreateAccount(ledger uint32, closedAt time.Time, txHash string, opIndex uint32, fromAddr string, op xdr.Operation, result xdr.OperationResult) ([]Movement, error) {
	if !opSucceeded(result) {
		return nil, nil
	}
	tr, ok := result.GetTr()
	if !ok {
		return nil, nil
	}
	r, ok := tr.GetCreateAccountResult()
	if !ok || r.Code != xdr.CreateAccountResultCodeCreateAccountSuccess {
		return nil, nil
	}

	body, ok := op.Body.GetCreateAccountOp()
	if !ok {
		return nil, fmt.Errorf("%w: op type CreateAccount but body has no CreateAccountOp (ledger %d tx %s op %d)",
			ErrMalformedMovement, ledger, txHash, opIndex)
	}
	if body.StartingBalance <= 0 {
		return nil, fmt.Errorf("%w: non-positive StartingBalance %d (ledger %d tx %s op %d)",
			ErrMalformedMovement, body.StartingBalance, ledger, txHash, opIndex)
	}

	return []Movement{{
		Kind:            KindCreateAccount,
		Provenance:      ProvenanceClassicDerived,
		Ledger:          ledger,
		LedgerCloseTime: closedAt,
		TxHash:          txHash,
		OpIndex:         opIndex,
		LegIndex:        0,
		Asset:           "native",
		Amount:          canonical.NewAmount(big.NewInt(int64(body.StartingBalance))),
		FromAddress:     fromAddr,
		ToAddress:       body.Destination.Address(),
	}}, nil
}

// decodePayment reconstructs a 'payment' movement: source -> dest,
// asset + amount straight from the op body. Research §2 path (a):
// PaymentResult is a bare success/failure code with no further
// data, so a success code alone is sufficient to trust the body's
// Asset/Amount.
func decodePayment(ledger uint32, closedAt time.Time, txHash string, opIndex uint32, fromAddr string, op xdr.Operation, result xdr.OperationResult) ([]Movement, error) {
	if !opSucceeded(result) {
		return nil, nil
	}
	tr, ok := result.GetTr()
	if !ok {
		return nil, nil
	}
	r, ok := tr.GetPaymentResult()
	if !ok || r.Code != xdr.PaymentResultCodePaymentSuccess {
		return nil, nil
	}

	body, ok := op.Body.GetPaymentOp()
	if !ok {
		return nil, fmt.Errorf("%w: op type Payment but body has no PaymentOp (ledger %d tx %s op %d)",
			ErrMalformedMovement, ledger, txHash, opIndex)
	}
	if body.Amount <= 0 {
		return nil, fmt.Errorf("%w: non-positive Amount %d (ledger %d tx %s op %d)",
			ErrMalformedMovement, body.Amount, ledger, txHash, opIndex)
	}
	dest, derr := body.Destination.GetAddress()
	if derr != nil {
		return nil, fmt.Errorf("%w: unresolvable destination: %w (ledger %d tx %s op %d)",
			ErrMalformedMovement, derr, ledger, txHash, opIndex)
	}

	return []Movement{{
		Kind:            KindPayment,
		Provenance:      ProvenanceClassicDerived,
		Ledger:          ledger,
		LedgerCloseTime: closedAt,
		TxHash:          txHash,
		OpIndex:         opIndex,
		LegIndex:        0,
		Asset:           xdrjson.AssetID(body.Asset),
		Amount:          canonical.NewAmount(big.NewInt(int64(body.Amount))),
		FromAddress:     fromAddr,
		ToAddress:       dest,
	}}, nil
}

// opSucceeded reports whether an operation reached its own
// type-specific result union (OperationResultCodeOpInner) — i.e.
// the op ran far enough to carry a Payment/CreateAccount/... result
// at all, success OR failure. false means the op failed at the
// transaction-validation layer (bad auth, missing source account,
// too many sub-entries, ...) before its own logic ever ran —
// result.GetTr() would report ok=false for the same reason, but
// checking the outer code explicitly documents the two distinct
// "no Tr()" causes for a reader.
func opSucceeded(result xdr.OperationResult) bool {
	return result.Code == xdr.OperationResultCodeOpInner
}

// ─── Phase 2: PathPaymentStrictReceive / PathPaymentStrictSend ────
//
// ADR-0047 D3 Phase 2 / research §2 path (b): both path-payment op
// types emit ONE 'path_payment' movement per op, keyed
// (ledger, tx_hash, op_index, leg_index=0) — never two rows, and
// never a row per hop (the per-hop ClaimAtoms already live in
// `trades` via internal/sources/sdex; duplicating them here would
// double-count the same on-chain event under a different table).
//
// The primary Asset/Amount columns hold the DESTINATION leg —
// result.Success.Last.{Asset,Amount} — for BOTH op types uniformly,
// never a body field: PathPaymentStrictReceiveOp.DestAmount and
// PathPaymentStrictSendOp.DestMin are exact/floor respectively in
// the body, but reading the amount that ACTUALLY reached the
// destination from the result's SimplePaymentResult is correct
// either way and needs no per-type branching (research §2's
// PathPaymentStrictReceive/Send table rows). Attributes carries the
// SOURCE leg (send_asset / send_amount) since the schema's Asset/
// Amount columns hold exactly one asset per row (migration 0105).
//
// Source-leg amount derivation is the one place the two op types
// genuinely differ:
//   - StrictSend: body.SendAmount is exact by protocol definition
//     ("sends an exact amount") — no result inspection needed.
//   - StrictReceive: body.SendMax is only a ceiling; the amount
//     actually consumed is derived from the result's Offers —
//     pathPaymentStrictReceiveSourceAmount below, verified against
//     real pre-P23 mainnet multi-hop data (see real_bytes_test.go's
//     TestRealBytes_pathPaymentStrictReceive_twoHop, ledger 40000003
//     tx 49203432…, which round-trips a genuine 2-hop
//     native→SHIB→native path exactly).

// decodePathPaymentStrictReceive reconstructs a 'path_payment'
// movement for a successful PathPaymentStrictReceive op.
func decodePathPaymentStrictReceive(ledger uint32, closedAt time.Time, txHash string, opIndex uint32, fromAddr string, op xdr.Operation, result xdr.OperationResult) ([]Movement, error) {
	if !opSucceeded(result) {
		return nil, nil
	}
	tr, ok := result.GetTr()
	if !ok {
		return nil, nil
	}
	r, ok := tr.GetPathPaymentStrictReceiveResult()
	if !ok || r.Code != xdr.PathPaymentStrictReceiveResultCodePathPaymentStrictReceiveSuccess {
		return nil, nil
	}
	succ := r.MustSuccess()

	body, ok := op.Body.GetPathPaymentStrictReceiveOp()
	if !ok {
		return nil, fmt.Errorf("%w: op type PathPaymentStrictReceive but body has no PathPaymentStrictReceiveOp (ledger %d tx %s op %d)",
			ErrMalformedMovement, ledger, txHash, opIndex)
	}

	sourceAmount, err := pathPaymentStrictReceiveSourceAmount(body.SendAsset, succ.Offers, succ.Last.Amount)
	if err != nil {
		return nil, fmt.Errorf("%w: ledger %d tx %s op %d: %w", ErrMalformedMovement, ledger, txHash, opIndex, err)
	}

	return buildPathPaymentMovement(ledger, closedAt, txHash, opIndex, fromAddr,
		body.SendAsset, sourceAmount, succ.Last)
}

// decodePathPaymentStrictSend reconstructs a 'path_payment' movement
// for a successful PathPaymentStrictSend op. Unlike StrictReceive,
// the source amount is exact in the body (SendAmount) — no Offers
// derivation needed.
func decodePathPaymentStrictSend(ledger uint32, closedAt time.Time, txHash string, opIndex uint32, fromAddr string, op xdr.Operation, result xdr.OperationResult) ([]Movement, error) {
	if !opSucceeded(result) {
		return nil, nil
	}
	tr, ok := result.GetTr()
	if !ok {
		return nil, nil
	}
	r, ok := tr.GetPathPaymentStrictSendResult()
	if !ok || r.Code != xdr.PathPaymentStrictSendResultCodePathPaymentStrictSendSuccess {
		return nil, nil
	}
	succ := r.MustSuccess()

	body, ok := op.Body.GetPathPaymentStrictSendOp()
	if !ok {
		return nil, fmt.Errorf("%w: op type PathPaymentStrictSend but body has no PathPaymentStrictSendOp (ledger %d tx %s op %d)",
			ErrMalformedMovement, ledger, txHash, opIndex)
	}
	if body.SendAmount <= 0 {
		return nil, fmt.Errorf("%w: non-positive SendAmount %d (ledger %d tx %s op %d)",
			ErrMalformedMovement, body.SendAmount, ledger, txHash, opIndex)
	}

	return buildPathPaymentMovement(ledger, closedAt, txHash, opIndex, fromAddr,
		body.SendAsset, body.SendAmount, succ.Last)
}

// buildPathPaymentMovement assembles the single 'path_payment' row
// shared by both StrictReceive and StrictSend once each has resolved
// its own source amount: primary Asset/Amount = the destination leg
// (last.Asset / last.Amount, exact from the result for both types);
// Attributes.send_asset / send_amount = the source leg.
func buildPathPaymentMovement(ledger uint32, closedAt time.Time, txHash string, opIndex uint32, fromAddr string, sendAsset xdr.Asset, sendAmount xdr.Int64, last xdr.SimplePaymentResult) ([]Movement, error) {
	if last.Amount <= 0 {
		return nil, fmt.Errorf("%w: non-positive dest Amount %d (ledger %d tx %s op %d)",
			ErrMalformedMovement, last.Amount, ledger, txHash, opIndex)
	}
	if sendAmount <= 0 {
		return nil, fmt.Errorf("%w: non-positive derived source amount %d (ledger %d tx %s op %d)",
			ErrMalformedMovement, sendAmount, ledger, txHash, opIndex)
	}
	destMuxed := last.Destination.ToMuxedAccount()
	destAddr, derr := destMuxed.GetAddress()
	if derr != nil {
		return nil, fmt.Errorf("%w: unresolvable destination: %w (ledger %d tx %s op %d)",
			ErrMalformedMovement, derr, ledger, txHash, opIndex)
	}

	return []Movement{{
		Kind:            KindPathPayment,
		Provenance:      ProvenanceClassicDerived,
		Ledger:          ledger,
		LedgerCloseTime: closedAt,
		TxHash:          txHash,
		OpIndex:         opIndex,
		LegIndex:        0,
		Asset:           xdrjson.AssetID(last.Asset),
		Amount:          canonical.NewAmount(big.NewInt(int64(last.Amount))),
		FromAddress:     fromAddr,
		ToAddress:       destAddr,
		Attributes: map[string]any{
			"send_asset":  xdrjson.AssetID(sendAsset),
			"send_amount": canonical.NewAmount(big.NewInt(int64(sendAmount))).String(),
		},
	}}, nil
}

// pathPaymentStrictReceiveSourceAmount derives the exact amount of
// sendAsset the taker actually paid for a successful
// PathPaymentStrictReceive — NOT available directly in the body
// (SendMax is only a ceiling) or as a single result field the way
// StrictSend's SendAmount is.
//
// Verified against real multi-hop mainnet data (real_bytes_test.go):
// stellar-core appends offers to Success.Offers in STRICT hop order,
// first hop first — a real 2-hop native→SHIB→native path payment
// showed Offers[0] = {AssetSold: SHIB, AssetBought: native} (hop 0:
// pay native, receive SHIB) followed by Offers[1] =
// {AssetSold: native, AssetBought: SHIB} (hop 1: pay SHIB, receive
// native) — i.e. Offers[0].AmountBought is exactly the taker's
// SendAsset spend, and later offers' AssetBought no longer matches
// SendAsset once the path has moved past the first hop. A single hop
// filled across multiple offers (order-book depth) keeps ALL of its
// claims at the front of Offers with AssetBought == SendAsset — this
// sums the full contiguous prefix, not just Offers[0], to cover that
// case too.
//
// Empty Offers means the op crossed no order book / pool at all —
// only possible when SendAsset == DestAsset (a path payment used as
// a plain, same-asset transfer with slippage-protection semantics).
// In that case nothing was converted, so the amount consumed equals
// exactly what was delivered: lastAmount.
func pathPaymentStrictReceiveSourceAmount(sendAsset xdr.Asset, offers []xdr.ClaimAtom, lastAmount xdr.Int64) (xdr.Int64, error) {
	if len(offers) == 0 {
		return lastAmount, nil
	}
	var total xdr.Int64
	for i, atom := range offers {
		boughtAsset, boughtAmount, ok := claimAtomBoughtSide(atom)
		if !ok {
			return 0, fmt.Errorf("claim atom %d: unrecognized ClaimAtomType %d", i, atom.Type)
		}
		if !boughtAsset.Equals(sendAsset) {
			if i == 0 {
				return 0, fmt.Errorf("first offer's AssetBought does not match SendAsset — hop-order assumption violated")
			}
			break // past the first hop; later offers convert a different asset pair
		}
		total += boughtAmount
	}
	if total <= 0 {
		return 0, fmt.Errorf("derived source amount is non-positive: %d", total)
	}
	return total, nil
}

// claimAtomBoughtSide extracts (AssetBought, AmountBought) from a
// ClaimAtom regardless of its concrete variant (OrderBook,
// LiquidityPool, or the legacy pre-CAP-27 V0 shape) — the "what did
// the taker pay into this offer" side, mirroring
// internal/sources/sdex/decode.go's decodeClaimAtom field mapping
// (AssetSold/AmountSold is what the taker RECEIVED; AssetBought/
// AmountBought is what the taker PAID).
func claimAtomBoughtSide(atom xdr.ClaimAtom) (xdr.Asset, xdr.Int64, bool) {
	switch atom.Type {
	case xdr.ClaimAtomTypeClaimAtomTypeOrderBook:
		ob := atom.MustOrderBook()
		return ob.AssetBought, ob.AmountBought, true
	case xdr.ClaimAtomTypeClaimAtomTypeLiquidityPool:
		lp := atom.MustLiquidityPool()
		return lp.AssetBought, lp.AmountBought, true
	case xdr.ClaimAtomTypeClaimAtomTypeV0:
		v0 := atom.MustV0()
		return v0.AssetBought, v0.AmountBought, true
	default:
		return xdr.Asset{}, 0, false
	}
}

// ─── Phase 3: ClaimableBalance create/claim/clawback + Clawback ───
//
// ADR-0047 D3 Phase 3. Four op types, three reconstruction paths:
//   - CreateClaimableBalance: path (a) — the CREATED BalanceId comes
//     back in the result (research §2), everything else from the
//     body. No correlation needed for this op itself.
//   - ClaimClaimableBalance / ClawbackClaimableBalance: path
//     (b+own-index) — neither op's body nor result carries an
//     asset/amount, only a BalanceId. Resolved against the create
//     row's own previously-derived Asset/Amount via Decoder's in-run
//     index (dispatcher_adapter.go), falling back to a Postgres
//     lookup for creates outside this run — see that file's Decoder
//     doc for the full design + memory-scaling caveat.
//   - Clawback (the plain op, not the claimable-balance variant):
//     path (a) — asset/amount/holder all in the body.
//
// Row cardinality: every Phase 3 kind is exactly one row per op
// (leg_index always 0) — none of these ops have a second asset leg.

// decodeCreateClaimableBalance reconstructs a
// 'claimable_balance_create' movement: source account -> escrow.
// ToAddress is deliberately empty (NULL) — the funds go to a
// Claimants list, not one address, until claimed (migration 0105's
// column comment). Attributes carries the generated balance_id (hex,
// same convention as internal/sources/claimable_balances'
// claimableIDHex: hex of just the V0 hash bytes, no type-discriminant
// prefix) and a claimants list (destination addresses only — the
// full ClaimPredicate tree, e.g. time-bound/AND/OR conditions, is
// not modelled here; a reader needing exact claim conditions should
// consult the raw op body via stellar.operations, not this table).
func decodeCreateClaimableBalance(ledger uint32, closedAt time.Time, txHash string, opIndex uint32, fromAddr string, op xdr.Operation, result xdr.OperationResult) ([]Movement, error) {
	if !opSucceeded(result) {
		return nil, nil
	}
	tr, ok := result.GetTr()
	if !ok {
		return nil, nil
	}
	r, ok := tr.GetCreateClaimableBalanceResult()
	if !ok || r.Code != xdr.CreateClaimableBalanceResultCodeCreateClaimableBalanceSuccess {
		return nil, nil
	}
	if r.BalanceId == nil {
		return nil, fmt.Errorf("%w: success result but nil BalanceId (ledger %d tx %s op %d)",
			ErrMalformedMovement, ledger, txHash, opIndex)
	}
	balanceIDHex, err := claimableBalanceIDHex(*r.BalanceId)
	if err != nil {
		return nil, fmt.Errorf("%w: %w (ledger %d tx %s op %d)", ErrMalformedMovement, err, ledger, txHash, opIndex)
	}

	body, ok := op.Body.GetCreateClaimableBalanceOp()
	if !ok {
		return nil, fmt.Errorf("%w: op type CreateClaimableBalance but body has no CreateClaimableBalanceOp (ledger %d tx %s op %d)",
			ErrMalformedMovement, ledger, txHash, opIndex)
	}
	if body.Amount <= 0 {
		return nil, fmt.Errorf("%w: non-positive Amount %d (ledger %d tx %s op %d)",
			ErrMalformedMovement, body.Amount, ledger, txHash, opIndex)
	}

	claimants := make([]string, 0, len(body.Claimants))
	for _, c := range body.Claimants {
		if c.V0 != nil {
			claimants = append(claimants, c.V0.Destination.Address())
		}
	}

	return []Movement{{
		Kind:            KindClaimableBalanceCreate,
		Provenance:      ProvenanceClassicDerived,
		Ledger:          ledger,
		LedgerCloseTime: closedAt,
		TxHash:          txHash,
		OpIndex:         opIndex,
		LegIndex:        0,
		Asset:           xdrjson.AssetID(body.Asset),
		Amount:          canonical.NewAmount(big.NewInt(int64(body.Amount))),
		FromAddress:     fromAddr,
		ToAddress:       "",
		Attributes: map[string]any{
			"balance_id": balanceIDHex,
			"claimants":  claimants,
		},
	}}, nil
}

// decodeClaimClaimableBalance reconstructs a
// 'claimable_balance_claim' movement: escrow -> claiming account
// (fromAddr, i.e. ctx.TxSource — the op has no separate claimant
// field, the claimer IS the op's source). FromAddress is
// deliberately empty (NULL): the escrow has no G-address. Asset/
// Amount are resolved via d's BalanceId index; if unresolved, this
// returns zero movements (not an error) and records a
// PendingClaimableBalanceRef for the caller's second pass.
func (d *Decoder) decodeClaimClaimableBalance(ledger uint32, closedAt time.Time, txHash string, opIndex uint32, fromAddr string, op xdr.Operation, result xdr.OperationResult) ([]Movement, error) {
	if !opSucceeded(result) {
		return nil, nil
	}
	tr, ok := result.GetTr()
	if !ok {
		return nil, nil
	}
	r, ok := tr.GetClaimClaimableBalanceResult()
	if !ok || r.Code != xdr.ClaimClaimableBalanceResultCodeClaimClaimableBalanceSuccess {
		return nil, nil
	}

	body, ok := op.Body.GetClaimClaimableBalanceOp()
	if !ok {
		return nil, fmt.Errorf("%w: op type ClaimClaimableBalance but body has no ClaimClaimableBalanceOp (ledger %d tx %s op %d)",
			ErrMalformedMovement, ledger, txHash, opIndex)
	}
	balanceIDHex, err := claimableBalanceIDHex(body.BalanceId)
	if err != nil {
		return nil, fmt.Errorf("%w: %w (ledger %d tx %s op %d)", ErrMalformedMovement, err, ledger, txHash, opIndex)
	}

	info, found := d.lookupClaimableBalance(balanceIDHex)
	if !found {
		d.recordPending(PendingClaimableBalanceRef{
			Kind:            KindClaimableBalanceClaim,
			BalanceIDHex:    balanceIDHex,
			Ledger:          ledger,
			LedgerCloseTime: closedAt,
			TxHash:          txHash,
			OpIndex:         opIndex,
			FromAddress:     "",
			ToAddress:       fromAddr,
		})
		return nil, nil
	}

	return []Movement{{
		Kind:            KindClaimableBalanceClaim,
		Provenance:      ProvenanceClassicDerived,
		Ledger:          ledger,
		LedgerCloseTime: closedAt,
		TxHash:          txHash,
		OpIndex:         opIndex,
		LegIndex:        0,
		Asset:           info.Asset,
		Amount:          info.Amount,
		FromAddress:     "",
		ToAddress:       fromAddr,
		Attributes: map[string]any{
			"balance_id": balanceIDHex,
			"created_by": info.CreatedBy,
		},
	}}, nil
}

// decodeClawbackClaimableBalance reconstructs a
// 'claimable_balance_clawback' movement: escrow -> destroyed. Mirrors
// decodeClaimClaimableBalance's correlation shape exactly (same
// BalanceId-only body, same index/pending split) but the "receiving"
// side is the issuer performing the clawback (fromAddr, i.e.
// ctx.TxSource — a Clawback-family op's effective source must be the
// asset's issuer) rather than an arbitrary claimer, matching plain
// Clawback's from-holder/to-issuer framing below.
func (d *Decoder) decodeClawbackClaimableBalance(ledger uint32, closedAt time.Time, txHash string, opIndex uint32, fromAddr string, op xdr.Operation, result xdr.OperationResult) ([]Movement, error) {
	if !opSucceeded(result) {
		return nil, nil
	}
	tr, ok := result.GetTr()
	if !ok {
		return nil, nil
	}
	r, ok := tr.GetClawbackClaimableBalanceResult()
	if !ok || r.Code != xdr.ClawbackClaimableBalanceResultCodeClawbackClaimableBalanceSuccess {
		return nil, nil
	}

	body, ok := op.Body.GetClawbackClaimableBalanceOp()
	if !ok {
		return nil, fmt.Errorf("%w: op type ClawbackClaimableBalance but body has no ClawbackClaimableBalanceOp (ledger %d tx %s op %d)",
			ErrMalformedMovement, ledger, txHash, opIndex)
	}
	balanceIDHex, err := claimableBalanceIDHex(body.BalanceId)
	if err != nil {
		return nil, fmt.Errorf("%w: %w (ledger %d tx %s op %d)", ErrMalformedMovement, err, ledger, txHash, opIndex)
	}

	info, found := d.lookupClaimableBalance(balanceIDHex)
	if !found {
		d.recordPending(PendingClaimableBalanceRef{
			Kind:            KindClaimableBalanceClawback,
			BalanceIDHex:    balanceIDHex,
			Ledger:          ledger,
			LedgerCloseTime: closedAt,
			TxHash:          txHash,
			OpIndex:         opIndex,
			FromAddress:     "",
			ToAddress:       fromAddr,
		})
		return nil, nil
	}

	return []Movement{{
		Kind:            KindClaimableBalanceClawback,
		Provenance:      ProvenanceClassicDerived,
		Ledger:          ledger,
		LedgerCloseTime: closedAt,
		TxHash:          txHash,
		OpIndex:         opIndex,
		LegIndex:        0,
		Asset:           info.Asset,
		Amount:          info.Amount,
		FromAddress:     "",
		ToAddress:       fromAddr,
		Attributes: map[string]any{
			"balance_id": balanceIDHex,
			"created_by": info.CreatedBy,
		},
	}}, nil
}

// decodeClawback reconstructs a 'clawback' movement for the plain
// Clawback op (NOT ClawbackClaimableBalance, handled above): holder
// -> issuer, asset destroyed (research §2 path (a) — Asset/From/
// Amount are all in the body; ClawbackResult is a bare code).
//
// FromAddress is body.From (the holder whose asset is clawed back) —
// NOT fromAddr/ctx.TxSource. ToAddress is fromAddr/ctx.TxSource (the
// clawback MUST be submitted by the asset's issuer; protocol enforces
// this via AUTH_CLAWBACK_ENABLED). Confirmed against real mainnet
// data: real_bytes_test.go's TestRealBytes_clawback_success has
// From=GCFROB3…(the holder) while the op's own resolved source
// account is GBBKUUT…(the issuer) — a DIFFERENT address, the reverse
// of every other op-only-surface kind (including AccountMerge) where
// fromAddr IS the movement's FromAddress.
func decodeClawback(ledger uint32, closedAt time.Time, txHash string, opIndex uint32, fromAddr string, op xdr.Operation, result xdr.OperationResult) ([]Movement, error) {
	if !opSucceeded(result) {
		return nil, nil
	}
	tr, ok := result.GetTr()
	if !ok {
		return nil, nil
	}
	r, ok := tr.GetClawbackResult()
	if !ok || r.Code != xdr.ClawbackResultCodeClawbackSuccess {
		return nil, nil
	}

	body, ok := op.Body.GetClawbackOp()
	if !ok {
		return nil, fmt.Errorf("%w: op type Clawback but body has no ClawbackOp (ledger %d tx %s op %d)",
			ErrMalformedMovement, ledger, txHash, opIndex)
	}
	if body.Amount <= 0 {
		return nil, fmt.Errorf("%w: non-positive Amount %d (ledger %d tx %s op %d)",
			ErrMalformedMovement, body.Amount, ledger, txHash, opIndex)
	}
	holderMuxed := body.From
	holderAddr, herr := holderMuxed.GetAddress()
	if herr != nil {
		return nil, fmt.Errorf("%w: unresolvable From: %w (ledger %d tx %s op %d)",
			ErrMalformedMovement, herr, ledger, txHash, opIndex)
	}

	return []Movement{{
		Kind:            KindClawback,
		Provenance:      ProvenanceClassicDerived,
		Ledger:          ledger,
		LedgerCloseTime: closedAt,
		TxHash:          txHash,
		OpIndex:         opIndex,
		LegIndex:        0,
		Asset:           xdrjson.AssetID(body.Asset),
		Amount:          canonical.NewAmount(big.NewInt(int64(body.Amount))),
		FromAddress:     holderAddr,
		ToAddress:       fromAddr,
	}}, nil
}

// claimableBalanceIDHex encodes a ClaimableBalanceId to a hex
// string. Only the V0 variant is defined in current XDR; a future
// variant would extend this switch. Same convention as
// internal/sources/claimable_balances' claimableIDHex (hex of just
// the V0 hash bytes, no type-discriminant prefix) — kept as an
// independent copy rather than an import since that package's helper
// is unexported and the two packages have no other coupling.
func claimableBalanceIDHex(id xdr.ClaimableBalanceId) (string, error) {
	if id.V0 == nil {
		return "", fmt.Errorf("ClaimableBalanceId variant %d has no V0 hash", id.Type)
	}
	return hex.EncodeToString(id.V0[:]), nil
}

// ─── Phase 4 (op-only half): AccountMerge ──────────────────────────
//
// ADR-0047 D3 Phase 4. AccountMerge doesn't need ledger_entry_changes
// (research §2 path (b)): the exact amount merged is in the RESULT,
// not the body — AccountMergeOp carries no amount field at all, only
// the destination; core computes and reports the source account's
// full native balance at merge time via
// AccountMergeResult.SourceAccountBalance. One row per op (leg_index
// always 0), asset always native (only XLM balance is transferable
// this way — any non-native trustlines must be removed/zero before
// merge succeeds).

// decodeAccountMerge reconstructs an 'account_merge' movement: the
// merging (source) account -> the destination account, for the
// account's FULL native balance (research §2 path (b): the amount is
// in AccountMergeResult.SourceAccountBalance, never derivable from
// the body).
func decodeAccountMerge(ledger uint32, closedAt time.Time, txHash string, opIndex uint32, fromAddr string, op xdr.Operation, result xdr.OperationResult) ([]Movement, error) {
	if !opSucceeded(result) {
		return nil, nil
	}
	tr, ok := result.GetTr()
	if !ok {
		return nil, nil
	}
	r, ok := tr.GetAccountMergeResult()
	if !ok || r.Code != xdr.AccountMergeResultCodeAccountMergeSuccess {
		return nil, nil
	}
	bal, ok := r.GetSourceAccountBalance()
	if !ok {
		return nil, fmt.Errorf("%w: success result but no SourceAccountBalance (ledger %d tx %s op %d)",
			ErrMalformedMovement, ledger, txHash, opIndex)
	}
	if bal <= 0 {
		return nil, fmt.Errorf("%w: non-positive SourceAccountBalance %d (ledger %d tx %s op %d)",
			ErrMalformedMovement, bal, ledger, txHash, opIndex)
	}

	destMuxed, ok := op.Body.GetDestination()
	if !ok {
		return nil, fmt.Errorf("%w: op type AccountMerge but body has no Destination (ledger %d tx %s op %d)",
			ErrMalformedMovement, ledger, txHash, opIndex)
	}
	destAddr, derr := destMuxed.GetAddress()
	if derr != nil {
		return nil, fmt.Errorf("%w: unresolvable destination: %w (ledger %d tx %s op %d)",
			ErrMalformedMovement, derr, ledger, txHash, opIndex)
	}

	return []Movement{{
		Kind:            KindAccountMerge,
		Provenance:      ProvenanceClassicDerived,
		Ledger:          ledger,
		LedgerCloseTime: closedAt,
		TxHash:          txHash,
		OpIndex:         opIndex,
		LegIndex:        0,
		Asset:           "native",
		Amount:          canonical.NewAmount(big.NewInt(int64(bal))),
		FromAddress:     fromAddr,
		ToAddress:       destAddr,
	}}, nil
}
