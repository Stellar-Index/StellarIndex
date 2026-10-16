// Copyright 2026 Stellar Index contributors
// SPDX-License-Identifier: Apache-2.0

package chops

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/consumer"
	"github.com/Stellar-Index/StellarIndex/internal/dispatcher"
	"github.com/Stellar-Index/StellarIndex/internal/sources/sdex"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
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

// The SDEX decoder keeps one-side-zero fills and fans claims out on a 1024 op_index stride. The served tier holds no
// duplicate primary key (ON CONFLICT DO NOTHING) and the census counts only priceable fills, so the served COUNT carries
// the same sdexPriceableFilter: a ledger with only a one-side-zero fill expects 0 rows.
func TestSDEXServedCensus_CountsOnlyPriceableFills(t *testing.T) {
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
		t.Errorf("ledger %d (one-side-zero only) expects %d served rows; the census excludes it", zeroOnly, n)
	}
}

// Every SDEX projection oracle in chops must count through the one served-tier projection. Counting raw decoder output
// (no Validate, no PK dedup) mislabels unstorable one-side-zero fills as recovered loss, and diffing against
// ledger_ingest_log (which keeps them) gives a permanent unclearable mismatch.
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
	if body := funcBodyFrom(cc, "reDeriveSDEXCensus"); !strings.Contains(body, "sdexServedCensus{}") {
		t.Error("reDeriveSDEXCensus does not count through sdexServedCensus")
	}
	if strings.Count(cc, "clickhouse.StreamSDEXOps(") != 1 {
		t.Error("compute_completeness.go streams SDEX ops outside reDeriveSDEXCensusViaDecoder")
	}
	for file, fn := range map[string]string{
		"ch_reproject.go":          "chReproject",
		"verify_reconciliation.go": "verifyReconExpected",
		"compute_completeness.go":  "expectedProjection",
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

// The census streams windows concurrently; every window must be read exactly once and its trades and blind ledgers merged.
func TestReDeriveSDEXCensus_ConcurrentWindowsMerge(t *testing.T) {
	t.Parallel()
	mkOp := func(ledger uint32, claim xdr.ClaimAtom) clickhouse.SDEXOp {
		return clickhouse.SDEXOp{
			Ledger: ledger, ClosedAt: time.Unix(1_700_000_000, 0).UTC(), TxHash: strings.Repeat("ab", 32),
			Op: xdr.Operation{Body: xdr.OperationBody{Type: xdr.OperationTypeManageSellOffer}},
			OpResult: xdr.OperationResult{
				Code: xdr.OperationResultCodeOpInner,
				Tr: &xdr.OperationResultTr{
					Type: xdr.OperationTypeManageSellOffer,
					ManageSellOfferResult: &xdr.ManageSellOfferResult{
						Code:    xdr.ManageSellOfferResultCodeManageSellOfferSuccess,
						Success: &xdr.ManageOfferSuccessResult{OffersClaimed: []xdr.ClaimAtom{claim}},
					},
				},
			},
		}
	}
	bad := xdr.ClaimAtom{Type: xdr.ClaimAtomType(99)}
	if op := mkOp(1, bad); func() int {
		_, f := sdex.NewDecoder().DecodeCounted(dispatcher.OpContext{Ledger: 1, ClosedAt: op.ClosedAt, TxHash: op.TxHash, Op: op.Op, OpResult: op.OpResult})
		return f
	}() != 1 {
		t.Fatal("precondition: an unknown claim-atom type must count as one failed decode")
	}

	const from, to = 1_000, 1_000 + 10*sdexCensusWindow + 7 // 11 windows, the last partial
	const blindLedger = from + 3*sdexCensusWindow + 1
	var mu sync.Mutex
	calls := map[uint32]uint32{}
	stream := func(_ context.Context, lo, hi uint32, fn func(clickhouse.SDEXOp) error) error {
		mu.Lock()
		calls[lo] = hi
		mu.Unlock()
		for _, l := range []uint32{lo, hi} {
			if err := fn(mkOp(l, sdexTestClaim(1, 5_000_000, 7_000_000))); err != nil {
				return err
			}
		}
		if lo <= blindLedger && blindLedger <= hi {
			return fn(mkOp(blindLedger, bad))
		}
		return nil
	}
	got, blind, err := reDeriveSDEXCensus(context.Background(), stream, from, to)
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 11 || calls[from] != from+sdexCensusWindow-1 || calls[from+10*sdexCensusWindow] != to {
		t.Fatalf("windows read: %v", calls)
	}
	if len(got) != 22 || got[from] != 1 || got[to] != 1 {
		t.Fatalf("got %d ledgers (from=%d to=%d), want 22 ledgers of 1 trade", len(got), got[from], got[to])
	}
	if blind.UndecodableMatched != 1 || len(blind.Ledgers) != 1 || blind.Ledgers[0] != blindLedger {
		t.Fatalf("blind = %+v, want one undecodable at %d", blind, blindLedger)
	}

	boom := errors.New("ch down")
	if _, _, err := reDeriveSDEXCensus(context.Background(), func(_ context.Context, lo, _ uint32, _ func(clickhouse.SDEXOp) error) error {
		if lo == from+5*sdexCensusWindow {
			return boom
		}
		return nil
	}, from, to); !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the window's error", err)
	}
}
