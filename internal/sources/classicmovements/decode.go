package classicmovements

import (
	"encoding/hex"
	"fmt"
	"math"
	"math/big"
	"time"

	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/sdexclaim"
	"github.com/Stellar-Index/StellarIndex/internal/xdrjson"
)

// SupportedOpTypes returns the op-only decode scope in stellar.operations.op_type form, the exact set clickhouse.StreamClassicOps should be called with; liquidity-pool and CAP-0038 trust-flag ops are the separate EntryChangeOpTypes surface, and recognition_test.go pins this list.
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

// matchesSupportedOp reports whether op is one of this package's op-only in-scope classic operation types; recognition_test.go's
// exhaustive-enum guard pins this switch.
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

// decodeOp dispatches one classic op to its decoder. Zero movements is not an error (failed op, or a claim/clawback whose create is
// unresolved); an out-of-scope op type is a loud ErrUnsupportedOpType. A method because claim/clawback use d's BalanceId index.
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

// decodeCreateAccount reconstructs a 'create_account' movement: source -> new account for
// StartingBalance, always native. The result is a bare code, so success alone makes the body's
// amount trustworthy.
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
	// StartingBalance == 0 is legal under CAP-33 sponsored reserves: the sponsor covers the
	// reserve. Rejecting it would drop every sponsored creation, so zero emits a real movement
	// and only a negative balance is malformed.
	if body.StartingBalance < 0 {
		return nil, fmt.Errorf("%w: negative StartingBalance %d (ledger %d tx %s op %d)",
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

// decodePayment reconstructs a 'payment' movement: source -> dest, asset and amount from the op
// body. PaymentResult is a bare code, so success alone makes the body trustworthy.
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
	dest, derr := baseAccountAddress(body.Destination)
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
		Attributes:      muxedDestinationAttrs(body.Destination),
	}}, nil
}

// muxedDestinationAttrs keeps the M-strkey a deposit was addressed to, since ToAddress is
// resolved to the base G for lookup. Without it /v1/accounts/{M...}/movements cannot tell which
// rows were meant for that sub-account. Returns nil for a non-muxed destination.
func muxedDestinationAttrs(m xdr.MuxedAccount) map[string]any {
	if m.Type != xdr.CryptoKeyTypeKeyTypeMuxedEd25519 {
		return nil
	}
	addr, err := m.GetAddress()
	if err != nil {
		return nil
	}
	return map[string]any{"destination_muxed": addr}
}

// opSucceeded reports whether the op reached its own type-specific result union
// (OperationResultCodeOpInner), success or failure. False means it failed at the
// transaction-validation layer before its own logic ran.
func opSucceeded(result xdr.OperationResult) bool {
	return result.Code == xdr.OperationResultCodeOpInner
}

// baseAccountAddress resolves a MUXED counterparty to the G-strkey holding the balance: account_movements is queried by G-strkey
// equality, so a row under an M-address would be missing from the real account's feed (matches xdrjson.ParticipantAccounts and the lake
// extractor). The memo id has no balance effect and is dropped.
func baseAccountAddress(m xdr.MuxedAccount) (string, error) {
	switch m.Type {
	case xdr.CryptoKeyTypeKeyTypeEd25519, xdr.CryptoKeyTypeKeyTypeMuxedEd25519:
		// AccountId.Address() panics on an encode error; GetAddress() is the
		// same encode with the error returned instead, which is what every
		// caller here already unwraps via the derr/herr pattern.
		aid := m.ToAccountId()
		return aid.GetAddress()
	default:
		return "", fmt.Errorf("unknown muxed account type %v", m.Type)
	}
}

// ─── PathPaymentStrictReceive / PathPaymentStrictSend ────
//
// A path payment moves TWO assets, so each op emits two 'path_payment' legs: leg_index 0, the
// SOURCE leg (FromAddress only, what left the sender), and leg_index 1, the DESTINATION leg
// (ToAddress only, result.Success.Last). One row per op would put one asset on both participants'
// rows, booking the sender an outflow in an asset it never held. No row per hop: per-hop
// ClaimAtoms already live in `trades` (sdex).
//
// The destination amount is always result.Success.Last.Amount (what landed). StrictSend
// body.SendAmount is exact, but StrictReceive body.SendMax is a ceiling, so that source amount is
// derived from the result's Offers (pathPaymentStrictReceiveSourceAmount).

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

// decodePathPaymentStrictSend reconstructs a 'path_payment' movement for a successful op; unlike StrictReceive the source amount is exact
// in the body (SendAmount).
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

// buildPathPaymentMovement assembles the two 'path_payment' legs shared by both variants: leg 0 is the sender's outflow in sendAsset, leg
// 1 the destination's inflow in last.Asset.
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

	sendAssetID, sendAmt := xdrjson.AssetID(sendAsset), canonical.NewAmount(big.NewInt(int64(sendAmount)))
	destAssetID, destAmt := xdrjson.AssetID(last.Asset), canonical.NewAmount(big.NewInt(int64(last.Amount)))
	attrs := func() map[string]any {
		return map[string]any{
			"send_asset":  sendAssetID,
			"send_amount": sendAmt.String(),
			"dest_asset":  destAssetID,
			"dest_amount": destAmt.String(),
			"from":        fromAddr,
			"to":          destAddr,
		}
	}
	leg := func(legIndex uint32, asset string, amount canonical.Amount, from, to string) Movement {
		return Movement{
			Kind:            KindPathPayment,
			Provenance:      ProvenanceClassicDerived,
			Ledger:          ledger,
			LedgerCloseTime: closedAt,
			TxHash:          txHash,
			OpIndex:         opIndex,
			LegIndex:        legIndex,
			Asset:           asset,
			Amount:          amount,
			FromAddress:     from,
			ToAddress:       to,
			Attributes:      attrs(),
		}
	}
	return []Movement{
		leg(0, sendAssetID, sendAmt, fromAddr, ""),
		leg(1, destAssetID, destAmt, "", destAddr),
	}, nil
}

// pathPaymentStrictReceiveSourceAmount derives the exact sendAsset the taker paid (the body has only the SendMax ceiling). stellar-core
// appends Success.Offers in strict hop order (real_bytes_test.go), so it sums the first hop's full contiguous prefix with AssetBought ==
// SendAsset, not just Offers[0]; empty Offers means SendAsset == DestAsset, so the amount is lastAmount.
func pathPaymentStrictReceiveSourceAmount(sendAsset xdr.Asset, offers []xdr.ClaimAtom, lastAmount xdr.Int64) (xdr.Int64, error) {
	if len(offers) == 0 {
		return lastAmount, nil
	}
	var total xdr.Int64
	for i, atom := range offers {
		boughtAsset, boughtAmount, ok := sdexclaim.BoughtSide(atom)
		if !ok {
			return 0, fmt.Errorf("claim atom %d: unrecognized ClaimAtomType %d", i, atom.Type)
		}
		if !boughtAsset.Equals(sendAsset) {
			if i == 0 {
				return 0, fmt.Errorf("first offer's AssetBought does not match SendAsset — hop-order assumption violated")
			}
			break // past the first hop; later offers convert a different asset pair
		}
		// Core bounds this sum by SendMax, so a negative atom or an int64
		// overflow is corrupt input; reject it rather than net or wrap it.
		if boughtAmount < 0 {
			return 0, fmt.Errorf("claim atom %d: negative AmountBought %d", i, boughtAmount)
		}
		if total > math.MaxInt64-boughtAmount {
			return 0, fmt.Errorf("claim atom %d: derived source amount overflows int64", i)
		}
		total += boughtAmount
	}
	if total <= 0 {
		return 0, fmt.Errorf("derived source amount is non-positive: %d", total)
	}
	return total, nil
}

// ─── ClaimableBalance create/claim/clawback + Clawback ───
//
// Claim and ClawbackClaimableBalance carry neither asset nor amount, only a BalanceId: it is
// resolved against the create row via Decoder's in-run index, falling back to a ClickHouse lookup
// for creates outside this run (see the Decoder doc in dispatcher_adapter.go). Create takes its
// BalanceId from the result; plain Clawback has asset, amount and holder in the body. Every kind
// is exactly one row per op (leg_index 0).

// decodeCreateClaimableBalance reconstructs a 'claimable_balance_create' movement, source account -> escrow with ToAddress NULL.
// Attributes carry balance_id (hex of the V0 hash bytes) and claimant addresses only; the ClaimPredicate tree is not modelled, so exact
// conditions come from the raw op body in stellar.operations.
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

// decodeClaimClaimableBalance reconstructs a 'claimable_balance_claim' movement, escrow -> claimer with FromAddress NULL (escrow has no
// G-address). An unresolved BalanceId returns zero movements, not an error, and records a PendingClaimableBalanceRef for the second pass.
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

// decodeClawbackClaimableBalance reconstructs a 'claimable_balance_clawback' movement: escrow ->
// destroyed. It correlates exactly like decodeClaimClaimableBalance, but the receiving side is
// the issuer (fromAddr), since a clawback op's source must be the asset's issuer.
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

// decodeClawback reconstructs a 'clawback' movement, holder -> issuer, asset destroyed. FromAddress is body.From (the holder), NOT
// fromAddr; ToAddress is fromAddr, the issuer who must submit it. This reverses every other op-only kind
// (TestRealBytes_clawback_success).
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
	holderAddr, herr := baseAccountAddress(body.From)
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

// claimableBalanceIDHex hex-encodes a ClaimableBalanceId (V0 hash bytes, no type prefix), the
// same convention as claimable_balances' claimableIDHex. It is a copy because that helper is
// unexported and the packages are otherwise uncoupled.
func claimableBalanceIDHex(id xdr.ClaimableBalanceId) (string, error) {
	if id.V0 == nil {
		return "", fmt.Errorf("ClaimableBalanceId variant %d has no V0 hash", id.Type)
	}
	return hex.EncodeToString(id.V0[:]), nil
}

// ─── AccountMerge ──────────────────────────
//
// AccountMerge needs no ledger_entry_changes: the merged amount is in the RESULT
// (SourceAccountBalance), since the op body carries only the destination. One row per op
// (leg_index 0), always native, because non-native trustlines must be gone before a merge succeeds.

// decodeAccountMerge reconstructs an 'account_merge' movement: merging account -> destination
// for its full native balance, which comes from AccountMergeResult.SourceAccountBalance.
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
	// bal == 0 is legal under CAP-33: a fully sponsored account merges with zero native balance.
	// Mirrors the StartingBalance rule in decodeCreateAccount; only a negative balance is malformed.
	if bal < 0 {
		return nil, fmt.Errorf("%w: negative SourceAccountBalance %d (ledger %d tx %s op %d)",
			ErrMalformedMovement, bal, ledger, txHash, opIndex)
	}

	destMuxed, ok := op.Body.GetDestination()
	if !ok {
		return nil, fmt.Errorf("%w: op type AccountMerge but body has no Destination (ledger %d tx %s op %d)",
			ErrMalformedMovement, ledger, txHash, opIndex)
	}
	destAddr, derr := baseAccountAddress(destMuxed)
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
		Attributes:      muxedDestinationAttrs(destMuxed),
	}}, nil
}
