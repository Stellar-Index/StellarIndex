package chops

import (
	"context"
	"encoding/hex"
	"math/big"
	"testing"
	"time"

	"github.com/stellar/go-stellar-sdk/strkey"
	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/dispatcher"
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

// TestClassicMovementsDecodeOp_DedupesUnmergedDuplicateRows: classicOpsQuery joins two ReplacingMergeTree tables without
// FINAL, so an unmerged part streams the same op twice. The same (ledger, tx_hash, op_index) must count once, or
// -verify's decoded count exceeds uniqExact and masks real write loss.
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

// TestClassicMovementsResumeStart_WidenedFromNotSkipped: a narrower earlier run wrote data only at the TOP of a later,
// wider -from range. MinAccountMovementLedger shows data does not start at the new -from, so the resume jump must be
// refused or the widened earlier region is never revisited.
func TestClassicMovementsResumeStart_WidenedFromNotSkipped(t *testing.T) {
	const (
		widenedFrom  = uint32(500_000) // this run's -from, widened below the prior run's start
		priorRunFrom = uint32(1_000_000)
		priorRunTip  = uint32(1_999_999) // MaxAccountMovementLedger([widenedFrom, to]) — prior run's tip, still the max in the wider range
	)

	resumeAt, jumped := classicMovementsResumeStart(widenedFrom, priorRunTip, priorRunFrom, true)

	if jumped {
		t.Fatalf("classicMovementsResumeStart(%d, %d, %d, true) = jumped=true resumeAt=%d — must NOT jump: [%d,%d) was never processed by the prior run and would be silently skipped",
			widenedFrom, priorRunTip, priorRunFrom, resumeAt, widenedFrom, priorRunFrom)
	}
	if resumeAt != widenedFrom {
		t.Fatalf("classicMovementsResumeStart(%d, %d, %d, true) resumeAt = %d, want %d (the resume must fall back to -from, not the prior run's tip)",
			widenedFrom, priorRunTip, priorRunFrom, resumeAt, widenedFrom)
	}
}

// TestClassicMovementsResumeStart_ContiguousFromStartJumps is the legitimate resume: data in [from,to] genuinely starts
// AT from, so jumping to maxLedger is sound under the ledger-ordered-insert invariant.
func TestClassicMovementsResumeStart_ContiguousFromStartJumps(t *testing.T) {
	const (
		from    = uint32(1_000_000)
		maxLdgr = uint32(1_999_999)
	)

	resumeAt, jumped := classicMovementsResumeStart(from, maxLdgr, from, true)

	if !jumped {
		t.Fatalf("classicMovementsResumeStart(%d, %d, %d, true) = jumped=false — want a jump: data starts exactly at -from, so the max is a sound resume point", from, maxLdgr, from)
	}
	if resumeAt != maxLdgr {
		t.Fatalf("classicMovementsResumeStart(%d, %d, %d, true) resumeAt = %d, want %d", from, maxLdgr, from, resumeAt, maxLdgr)
	}
}

// TestClassicMovementsResumeStart_NoDataInRange covers minFound=false
// (nothing written yet in [from,to] at all) — must not jump.
func TestClassicMovementsResumeStart_NoDataInRange(t *testing.T) {
	resumeAt, jumped := classicMovementsResumeStart(1_000_000, 1_999_999, 0, false)
	if jumped || resumeAt != 1_000_000 {
		t.Fatalf("classicMovementsResumeStart with minFound=false = (resumeAt=%d, jumped=%v), want (1000000, false)", resumeAt, jumped)
	}
}

// TestResolvePendingClaimableBalances_CAP0038SameWindowCreate: a claim against a balance created by CAP-0038 auto-
// liquidation later in the same window must resolve from that window's batch, not count as unresolved. The create
// bypasses dec.Decode and its BalanceId index, so the resolver must scan res.batch before counting a miss.
func TestResolvePendingClaimableBalances_CAP0038SameWindowCreate(t *testing.T) {
	// A claim against a balance_id the Decoder has never seen, via the real Decode() so the
	// pending ref and its balance_id hex are computed as production does, not hand-built.
	var h xdr.Hash
	h[0] = 0x42
	bid := xdr.ClaimableBalanceId{Type: xdr.ClaimableBalanceIdTypeClaimableBalanceIdTypeV0, V0: &h}
	balanceIDHex := hex.EncodeToString(h[:])

	dec := classicmovements.NewDecoder()
	claimerAddr := "GAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAWHF5"
	outs, err := dec.Decode(dispatcher.OpContext{
		Ledger:   40_000_100,
		TxHash:   "txclaim1",
		TxSource: claimerAddr,
		OpIndex:  0,
		Op: xdr.Operation{
			Body: xdr.OperationBody{
				Type:                    xdr.OperationTypeClaimClaimableBalance,
				ClaimClaimableBalanceOp: &xdr.ClaimClaimableBalanceOp{BalanceId: bid},
			},
		},
		OpResult: xdr.OperationResult{
			Code: xdr.OperationResultCodeOpInner,
			Tr: &xdr.OperationResultTr{
				Type:                        xdr.OperationTypeClaimClaimableBalance,
				ClaimClaimableBalanceResult: &xdr.ClaimClaimableBalanceResult{Code: xdr.ClaimClaimableBalanceResultCodeClaimClaimableBalanceSuccess},
			},
		},
	})
	if err != nil {
		t.Fatalf("Decode(claim): %v", err)
	}
	if len(outs) != 0 {
		t.Fatalf("Decode(claim) produced %d movements, want 0 (must be pending, not resolved yet)", len(outs))
	}

	// res.batch already holds this window's CAP-0038-created balance, built as classicMovementsHandleCAP0038Op builds it
	// (from classicmovements.DecodeCAP0038Revocation's output), keyed on the same balance_id the claim references.
	res := &windowResult{
		windowCounts: map[classicmovements.Kind]int64{},
		batch: []clickhouse.AccountMovement{
			{
				MovementKind: string(classicmovements.KindClaimableBalanceCreate),
				Asset:        "native",
				Amount:       big.NewInt(500_000_000),
				FromAddress:  "GTRUSTORAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAWHF5",
				Attributes: map[string]any{
					"balance_id": balanceIDHex,
					"revocation": true,
				},
			},
		},
	}

	// chAddr is deliberately unreachable: a fall-through to the ClickHouse lookup fails loudly
	// (dial/DNS error), proving the batch-local resolution resolved the claim.
	classicMovementsResolvePendingClaimableBalances(context.Background(), "bogus.invalid:0", dec, 40_000_000, 40_000_200, res)

	if res.windowUnresolved != 0 {
		t.Fatalf("windowUnresolved = %d, want 0 — the CAP-0038 create is right there in res.batch", res.windowUnresolved)
	}
	if res.windowResolvedIndex != 1 {
		t.Fatalf("windowResolvedIndex = %d, want 1", res.windowResolvedIndex)
	}
	if len(res.batch) != 2 {
		t.Fatalf("res.batch has %d entries, want 2 (the CAP-0038 create + the now-resolved claim)", len(res.batch))
	}
	claim := res.batch[1]
	if claim.MovementKind != string(classicmovements.KindClaimableBalanceClaim) {
		t.Errorf("resolved movement kind = %q, want %q", claim.MovementKind, classicmovements.KindClaimableBalanceClaim)
	}
	if claim.Asset != "native" || claim.Amount == nil || claim.Amount.Cmp(big.NewInt(500_000_000)) != 0 {
		t.Errorf("resolved claim asset/amount = %q/%v, want native/500000000 (from the batch-local CAP-0038 create)", claim.Asset, claim.Amount)
	}
}
