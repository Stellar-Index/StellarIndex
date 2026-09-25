// Copyright 2026 Stellar Index contributors
// SPDX-License-Identifier: Apache-2.0

package chops

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/consumer"
	"github.com/Stellar-Index/StellarIndex/internal/dispatcher"
	"github.com/Stellar-Index/StellarIndex/internal/sources/sdex"
)

func sdexTestAsset(code string, issuerSeed byte) xdr.Asset {
	var pub xdr.Uint256
	pub[0] = issuerSeed
	var codeArr [4]byte
	copy(codeArr[:], code)
	return xdr.Asset{
		Type: xdr.AssetTypeAssetTypeCreditAlphanum4,
		AlphaNum4: &xdr.AlphaNum4{
			AssetCode: codeArr,
			Issuer:    xdr.AccountId{Type: xdr.PublicKeyTypePublicKeyTypeEd25519, Ed25519: &pub},
		},
	}
}

func sdexTestClaim(offerID, sold, bought int64) xdr.ClaimAtom {
	var seller xdr.Uint256
	seller[0] = 0x20
	return xdr.ClaimAtom{
		Type: xdr.ClaimAtomTypeClaimAtomTypeOrderBook,
		OrderBook: &xdr.ClaimOfferAtom{
			SellerId:     xdr.AccountId{Type: xdr.PublicKeyTypePublicKeyTypeEd25519, Ed25519: &seller},
			OfferId:      xdr.Int64(offerID),
			AssetSold:    xdr.Asset{Type: xdr.AssetTypeAssetTypeNative},
			AmountSold:   xdr.Int64(sold),
			AssetBought:  sdexTestAsset("USDC", 0x10),
			AmountBought: xdr.Int64(bought),
		},
	}
}

// sdexTestDecode runs one ManageSellOffer carrying claims through the REAL
// SDEX decoder, exactly as the ClickHouse op pass feeds it.
func sdexTestDecode(t *testing.T, ledger uint32, opIndex int, claims []xdr.ClaimAtom) []consumer.Event {
	t.Helper()
	op := xdr.Operation{Body: xdr.OperationBody{Type: xdr.OperationTypeManageSellOffer}}
	res := xdr.OperationResult{
		Code: xdr.OperationResultCodeOpInner,
		Tr: &xdr.OperationResultTr{
			Type: xdr.OperationTypeManageSellOffer,
			ManageSellOfferResult: &xdr.ManageSellOfferResult{
				Code:    xdr.ManageSellOfferResultCodeManageSellOfferSuccess,
				Success: &xdr.ManageOfferSuccessResult{OffersClaimed: claims},
			},
		},
	}
	outs, err := sdex.NewDecoder().Decode(dispatcher.OpContext{
		Ledger:   ledger,
		ClosedAt: time.Unix(1_700_000_000, 0).UTC(),
		TxHash:   strings.Repeat("ab", 32),
		OpIndex:  opIndex,
		Op:       op,
		OpResult: res,
	})
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	return outs
}

// GH-933: the SDEX decoder keeps one-side-zero fills and fans claims out on a
// 1024 op_index stride, but the served trades table holds neither a zero leg
// (CHECK base_amount > 0, Validate) nor a duplicate primary key (ON CONFLICT
// DO NOTHING). The served-tier projection must count exactly what the writer
// can land, so a ledger whose only activity is a one-side-zero fill expects 0
// rows, not 1.
func TestSDEXServedCensus_CountsOnlyWhatTheWriterCanStore(t *testing.T) {
	t.Parallel()
	const zeroOnly, mixed, fanout = 100, 101, 102

	c := sdexServedCensus{}
	// The shape decode_test.go pins: AmountSold=0, AmountBought=12_000_000.
	oneSide := sdexTestDecode(t, zeroOnly, 0, []xdr.ClaimAtom{sdexTestClaim(1, 0, 12_000_000)})
	if len(oneSide) != 1 {
		t.Fatalf("decoder emitted %d events for a one-side-zero fill, want 1 (the precondition of this test)", len(oneSide))
	}
	c.add(oneSide)
	c.add(sdexTestDecode(t, mixed, 0, []xdr.ClaimAtom{
		sdexTestClaim(2, 5_000_000, 0),
		sdexTestClaim(3, 5_000_000, 7_000_000),
	}))

	// op 0 with 1025 claims: claim 1024 lands on op_index 1024, the same key
	// op 1's first claim gets. Served stores 1025 rows for 1026 decoder events.
	big := make([]xdr.ClaimAtom, 1025)
	for i := range big {
		big[i] = sdexTestClaim(int64(10+i), 1_000, 2_000)
	}
	c.add(sdexTestDecode(t, fanout, 0, big))
	c.add(sdexTestDecode(t, fanout, 1, []xdr.ClaimAtom{sdexTestClaim(5000, 1_000, 2_000)}))

	got := make(map[uint32]int)
	c.addTo(got)
	want := map[uint32]int{mixed: 1, fanout: 1025}
	if len(got) != len(want) {
		t.Fatalf("served projection = %v, want %v", got, want)
	}
	for l, n := range want {
		if got[l] != n {
			t.Errorf("ledger %d: served projection = %d, want %d (full: %v)", l, got[l], n, got)
		}
	}
	if n, ok := got[zeroOnly]; ok {
		t.Errorf("ledger %d (one-side-zero only) expects %d served rows; the writer can store none", zeroOnly, n)
	}
}

// GH-933: every SDEX projection oracle in chops must count through the one
// served-tier projection. Two consumers diverged: ch-reproject counted raw
// decoder output (no Validate, no PK dedup) and labelled the unstorable
// one-side-zero fills "recovered loss"; verify-reconciliation and the legacy
// compute-completeness path diffed served against the ledger_ingest_log
// census, which keeps one-side-zero fills by design, so they reported a
// permanent mismatch no operator action could clear.
func TestSDEXProjectionOracles_RouteThroughTheServedProjection(t *testing.T) {
	t.Parallel()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	// ch-rebuild is the writer: it buffers the events themselves (not counts)
	// and applies the same Validate + PK filter inline.
	streamAllowed := map[string]bool{"compute_completeness.go": true, "ch_rebuild.go": true}
	scanned := 0
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, rerr := os.ReadFile(f)
		if rerr != nil {
			t.Fatal(rerr)
		}
		scanned++
		src := string(b)
		if strings.Contains(src, "ClassicTradeEffectCountsByLedger(") {
			t.Errorf("%s: uses the ledger_ingest_log trade census as an SDEX projection oracle; it counts one-side-zero fills the trades table cannot hold — use sdexProjectionExpected", f)
		}
		if strings.Contains(src, "clickhouse.StreamSDEXOps(") && !streamAllowed[f] {
			t.Errorf("%s: streams SDEX ops and counts decoder output itself; count through reDeriveSDEXCensusViaDecoder", f)
		}
	}
	if scanned == 0 {
		t.Fatal("no source files scanned — this test is asserting nothing")
	}

	cc := mustRead(t, "compute_completeness.go")
	if body := funcBodyFrom(cc, "reDeriveSDEXCensusViaDecoder"); !strings.Contains(body, "sdexServedCensus{}") {
		t.Error("reDeriveSDEXCensusViaDecoder does not count through sdexServedCensus")
	}
	if strings.Count(cc, "clickhouse.StreamSDEXOps(") != 1 {
		t.Error("compute_completeness.go streams SDEX ops outside reDeriveSDEXCensusViaDecoder")
	}
	for file, fn := range map[string]string{
		"ch_reproject.go":          "chReproject",
		"verify_reconciliation.go": "verifyReconciliation",
		"compute_completeness.go":  "reconcileSourceProjection",
	} {
		body := funcBodyFrom(mustRead(t, file), fn)
		if body == "" {
			t.Fatalf("%s: %s not found — this test is asserting nothing", file, fn)
		}
		if !strings.Contains(body, "reDeriveSDEXCensusViaDecoder(") && !strings.Contains(body, "sdexProjectionExpected(") {
			t.Errorf("%s: %s does not take its SDEX expected counts from the served projection", file, fn)
		}
	}
}

func mustRead(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}
