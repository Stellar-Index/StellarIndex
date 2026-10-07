package chops

import (
	"testing"
	"time"

	"github.com/stellar/go-stellar-sdk/strkey"
	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/sources/classicmovements"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// mkDedupTestAccount returns a valid G-strkey + xdr.AccountId from a seed
// byte, mirroring classicmovements' own decode_test.go helper.
func mkDedupTestAccount(t *testing.T, seed byte) (string, xdr.AccountId) {
	t.Helper()
	var pub xdr.Uint256
	pub[0] = seed
	aid := xdr.AccountId{Type: xdr.PublicKeyTypePublicKeyTypeEd25519, Ed25519: &pub}
	s, err := strkey.Encode(strkey.VersionByteAccountID, pub[:])
	if err != nil {
		t.Fatalf("strkey.Encode: %v", err)
	}
	return s, aid
}

func mkDedupTestClassicOp(t *testing.T) clickhouse.ClassicOp {
	t.Helper()
	_, dest := mkDedupTestAccount(t, 0x10)
	_, issuer := mkDedupTestAccount(t, 0x11)
	var code [4]byte
	copy(code[:], "USDC")
	asset := xdr.Asset{Type: xdr.AssetTypeAssetTypeCreditAlphanum4, AlphaNum4: &xdr.AlphaNum4{AssetCode: code, Issuer: issuer}}

	return clickhouse.ClassicOp{
		Ledger:   40_000_000,
		ClosedAt: time.Unix(1_700_000_000, 0).UTC(),
		TxHash:   "dedup-test-tx",
		Source:   "GDEDUPTESTSOURCE0000000000000000000000000000000000000000",
		OpIndex:  0,
		Op: xdr.Operation{Body: xdr.OperationBody{
			Type: xdr.OperationTypePayment,
			PaymentOp: &xdr.PaymentOp{
				Destination: xdr.MuxedAccount{Type: xdr.CryptoKeyTypeKeyTypeEd25519, Ed25519: dest.Ed25519},
				Asset:       asset,
				Amount:      xdr.Int64(1_0000000),
			},
		}},
		OpResult: xdr.OperationResult{
			Code: xdr.OperationResultCodeOpInner,
			Tr: &xdr.OperationResultTr{
				Type:          xdr.OperationTypePayment,
				PaymentResult: &xdr.PaymentResult{Code: xdr.PaymentResultCodePaymentSuccess},
			},
		},
	}
}

// TestClassicMovementsDecodeOp_DedupesUnmergedDuplicateRows pins dedup:
// classicOpsQuery joins two ReplacingMergeTree tables without FINAL, so an
// unmerged part streams the SAME op twice. Feeding the same (ledger,
// tx_hash, op_index) through classicMovementsDecodeOp twice must count it
// once, or -verify's decoded count is a multiple of ClickHouse's uniqExact
// count and a genuine write loss becomes indistinguishable from lake
// duplication.
func TestClassicMovementsDecodeOp_DedupesUnmergedDuplicateRows(t *testing.T) {
	op := mkDedupTestClassicOp(t)
	dec := classicmovements.NewDecoder()
	res := &windowResult{windowCounts: map[classicmovements.Kind]int64{}}
	seen := make(map[classicMovementOpKey]struct{})

	classicMovementsDecodeOp(dec, seen, op, res)
	classicMovementsDecodeOp(dec, seen, op, res) // the unmerged-part duplicate

	if res.windowDecoded != 1 {
		t.Fatalf("windowDecoded = %d, want 1 (duplicate CH row must not be double-decoded)", res.windowDecoded)
	}
	if got := res.windowCounts[classicmovements.KindPayment]; got != 1 {
		t.Fatalf("windowCounts[payment] = %d, want 1", got)
	}
	if len(res.batch) != 1 {
		t.Fatalf("len(batch) = %d, want 1 (duplicate CH row must not be double-written)", len(res.batch))
	}

	// A genuinely distinct op (different op_index) must still be counted.
	op2 := op
	op2.OpIndex = 1
	classicMovementsDecodeOp(dec, seen, op2, res)
	if res.windowDecoded != 2 {
		t.Fatalf("windowDecoded = %d, want 2 after a genuinely distinct op", res.windowDecoded)
	}
}
