package classicmovements

import (
	"fmt"
	"math/big"
	"time"

	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/StellarIndex/stellar-index/internal/canonical"
	"github.com/StellarIndex/stellar-index/internal/xdrjson"
)

// SupportedOpTypes returns this phase's op-type scope in
// stellar.operations.op_type string form (xdr.OperationType.String())
// — the exact set clickhouse.StreamClassicOps should be called with,
// and the exact set matchesPhase1Op / decodeOp's switches cover.
// Phase 2 adds PathPaymentStrictReceive/Send here (and to
// matchesPhase1Op and decodeOp) when it lands; see recognition_test.go
// for the guard that pins this list.
func SupportedOpTypes() []string {
	return []string{
		xdr.OperationTypeCreateAccount.String(),
		xdr.OperationTypePayment.String(),
	}
}

// matchesPhase1Op reports whether op is one of Phase 1's two
// in-scope classic operation types. See recognition_test.go for the
// exhaustive-enum guard that pins this switch to exactly
// {CreateAccount, Payment} — the ADR-0047 D4.2 recognition check.
func matchesPhase1Op(op xdr.Operation) bool {
	switch op.Body.Type {
	case xdr.OperationTypeCreateAccount, xdr.OperationTypePayment:
		return true
	}
	return false
}

// decodeOp is the phase op-type dispatch: given one classic op + its
// result + tx-level context, emit zero or one Movement. Zero
// movements is not an error — a failed op (result.Code != OpInner,
// or the operation's own result-union arm isn't the Success case)
// simply never happened and moved nothing (D1's "success-code-
// filtered" rule). An out-of-scope op type — one matchesPhase1Op
// would reject — is a loud ErrUnsupportedOpType instead of a silent
// zero-movements return; see that sentinel's doc comment for why.
func decodeOp(ledger uint32, closedAt time.Time, txHash string, opIndex uint32, fromAddr string, op xdr.Operation, result xdr.OperationResult) ([]Movement, error) {
	switch op.Body.Type {
	case xdr.OperationTypeCreateAccount:
		return decodeCreateAccount(ledger, closedAt, txHash, opIndex, fromAddr, op, result)
	case xdr.OperationTypePayment:
		return decodePayment(ledger, closedAt, txHash, opIndex, fromAddr, op, result)
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
