package xdrjson

import (
	"fmt"

	"github.com/stellar/go-stellar-sdk/xdr"
)

// This file turns the raw XDR result-code integers the lake stores
// (stellar.transactions.result_code and stellar.operation_results.result_code)
// into stable, human-readable slugs, so a FAILED transaction is self-explaining
// on the API and in the explorer UI — never a bare, unexplained integer. It
// lives in xdrjson (the network-explorer classic-XDR→human decoder, ADR-0038)
// alongside the op-body/key/entry decoders — the same category of non-SCVal
// classic-XDR semantics, so the explorer serving layer stays xdr-free. The
// slugs are Horizon-aligned where a precedent exists (e.g. tx_no_source_account,
// op_no_source_account) so they read familiarly to Stellar developers.
//
// Design note (transparency, not suppression): failed transactions ARE indexed
// and ARE served (an on-chain, fee-charged, permanent record — many explorers
// show them). The honest contract is to mark them clearly. The authoritative
// "why" is the TRANSACTION-level result code: successful=false + the tx result
// (e.g. "tx_failed", "tx_insufficient_fee") states, unambiguously, that the
// whole transaction did not apply and why. The per-operation code is structural
// detail: for a txFAILED, an operation that itself failed structurally carries
// op_bad_auth / op_no_source_account / …; an operation whose outcome is in its
// inner (op-type-specific) result carries op_inner, with the transaction-level
// reason remaining the authoritative headline.

// txResultNames maps every transaction result code to its slug. Keyed by the
// xdr typed constant so the compiler pins each entry to a real enum member;
// TestTxResultNamesExhaustive asserts none is missing as the SDK adds codes.
var txResultNames = map[xdr.TransactionResultCode]string{
	xdr.TransactionResultCodeTxSuccess:             "tx_success",
	xdr.TransactionResultCodeTxFeeBumpInnerSuccess: "tx_fee_bump_inner_success",
	xdr.TransactionResultCodeTxFailed:              "tx_failed",
	xdr.TransactionResultCodeTxTooEarly:            "tx_too_early",
	xdr.TransactionResultCodeTxTooLate:             "tx_too_late",
	xdr.TransactionResultCodeTxMissingOperation:    "tx_missing_operation",
	xdr.TransactionResultCodeTxBadSeq:              "tx_bad_seq",
	xdr.TransactionResultCodeTxBadAuth:             "tx_bad_auth",
	xdr.TransactionResultCodeTxInsufficientBalance: "tx_insufficient_balance",
	xdr.TransactionResultCodeTxNoAccount:           "tx_no_source_account",
	xdr.TransactionResultCodeTxInsufficientFee:     "tx_insufficient_fee",
	xdr.TransactionResultCodeTxBadAuthExtra:        "tx_bad_auth_extra",
	xdr.TransactionResultCodeTxInternalError:       "tx_internal_error",
	xdr.TransactionResultCodeTxNotSupported:        "tx_not_supported",
	xdr.TransactionResultCodeTxFeeBumpInnerFailed:  "tx_fee_bump_inner_failed",
	xdr.TransactionResultCodeTxBadSponsorship:      "tx_bad_sponsorship",
	xdr.TransactionResultCodeTxBadMinSeqAgeOrGap:   "tx_bad_minseq_age_or_gap",
	xdr.TransactionResultCodeTxMalformed:           "tx_malformed",
	xdr.TransactionResultCodeTxSorobanInvalid:      "tx_soroban_invalid",
	xdr.TransactionResultCodeTxFrozenKeyAccessed:   "tx_frozen_key_accessed",
}

// opResultNames maps every OUTER operation result code to its slug. When the
// code is op_inner the operation was applied and its outcome lives in the
// op-type-specific inner result; the transaction-level reason is authoritative.
var opResultNames = map[xdr.OperationResultCode]string{
	xdr.OperationResultCodeOpInner:             "op_inner",
	xdr.OperationResultCodeOpBadAuth:           "op_bad_auth",
	xdr.OperationResultCodeOpNoAccount:         "op_no_source_account",
	xdr.OperationResultCodeOpNotSupported:      "op_not_supported",
	xdr.OperationResultCodeOpTooManySubentries: "op_too_many_subentries",
	xdr.OperationResultCodeOpExceededWorkLimit: "op_exceeded_work_limit",
	xdr.OperationResultCodeOpTooManySponsoring: "op_too_many_sponsoring",
}

// TxResultName returns the human-readable slug for a transaction result code
// (the value in stellar.transactions.result_code). An unmapped/newer code
// falls back to a truthful numeric form rather than a blank — a slug is never
// silently empty, so the wire never implies "success" for an unknown code.
func TxResultName(code int32) string {
	if s, ok := txResultNames[xdr.TransactionResultCode(code)]; ok {
		return s
	}
	return fmt.Sprintf("tx_unknown(%d)", code)
}

// OpResultName returns the human-readable slug for an operation result code
// (the value in stellar.operation_results.result_code). Same fallback
// discipline as TxResultName.
func OpResultName(code int32) string {
	if s, ok := opResultNames[xdr.OperationResultCode(code)]; ok {
		return s
	}
	return fmt.Sprintf("op_unknown(%d)", code)
}
