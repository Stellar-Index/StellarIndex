//go:build integration

package integration_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/ops/chops"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TestComputeCompleteness_AbsentByDesign drives the real compute-completeness path over a quiet network's lake: ledgers
// [2,40], no SDEX trade, an empty served tier. Undeclared it stays "no evidence"; declared it is complete and flagged;
// once the lake holds a trade the declaration is ignored and flagged as wrong. Its own ClickHouse container, so the
// shared lake's tip never moves.
func TestComputeCompleteness_AbsentByDesign(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()

	chAddr, chContainer, err := startClickHouse(ctx)
	if err != nil {
		t.Fatalf("start clickhouse: %v", err)
	}
	t.Cleanup(func() { _ = chContainer.Terminate(context.Background()) })
	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	cfgPath := filepath.Join(t.TempDir(), "stellarindex.toml")
	if err := os.WriteFile(cfgPath, []byte(fmt.Sprintf("[storage]\npostgres_dsn = %q\n", dsn)), 0o600); err != nil {
		t.Fatal(err)
	}

	const tip = 40
	seqs := make([]uint32, 0, tip-1)
	for s := uint32(2); s <= tip; s++ {
		seqs = append(seqs, s)
	}
	insertContiguityLedgers(ctx, t, chAddr, seqs...)
	// The lake holds no contract event, so a recognition scan would refuse to certify; carry a clean prior instead.
	if err := store.UpsertCompletenessSnapshot(ctx, timescale.CompletenessSnapshot{
		Source: "sdex", Genesis: 2, Tip: tip, Watermark: tip, CoveragePct: 1,
		SubstrateOK: true, RecognitionOK: true, Detail: "seeded prior",
	}); err != nil {
		t.Fatalf("seed prior verdict: %v", err)
	}

	run := func(extra ...string) error {
		args := []string{
			"compute-completeness", "-config", cfgPath, "-ch", "-ch-addr", chAddr,
			"-to", fmt.Sprint(tip), "-source", "sdex", "-skip-recognition", "-write",
		}
		return chops.Run(append(args, extra...))
	}
	sdex := func() timescale.CompletenessSnapshot {
		t.Helper()
		snaps, err := store.ListCompletenessSnapshots(ctx)
		if err != nil {
			t.Fatalf("list snapshots: %v", err)
		}
		for _, s := range snaps {
			if s.Source == "sdex" {
				return s
			}
		}
		t.Fatal("no sdex verdict")
		return timescale.CompletenessSnapshot{}
	}

	if err := run("-absent-by-design", "sdx"); err == nil || !strings.Contains(err.Error(), `"sdx"`) {
		t.Fatalf("unknown declared source: err = %v, want a failure naming it", err)
	}

	if err := run(); err != nil {
		t.Fatalf("undeclared run: %v", err)
	}
	if s := sdex(); s.Complete || s.ProjectionOK || s.AbsentByDesign || !strings.Contains(s.Detail, "projection: no evidence") {
		t.Fatalf("undeclared: complete=%v projection_ok=%v absent=%v detail=%q, want the no-evidence refusal", s.Complete, s.ProjectionOK, s.AbsentByDesign, s.Detail)
	}

	if err := run("-absent-by-design", "sdex"); err != nil {
		t.Fatalf("declared run: %v", err)
	}
	s := sdex()
	if !s.Complete || !s.ProjectionOK || !s.AbsentByDesign || s.AbsentDeclaredButPresent || s.ProjectionVerifiedFrom != 2 {
		t.Fatalf("declared, empty lake: complete=%v projection_ok=%v absent=%v present=%v verified_from=%d detail=%q",
			s.Complete, s.ProjectionOK, s.AbsentByDesign, s.AbsentDeclaredButPresent, s.ProjectionVerifiedFrom, s.Detail)
	}
	if want := "projection: absent by design on this network (declared); lake holds no trades in [2,40]"; !strings.Contains(s.Detail, want) {
		t.Errorf("declared detail = %q, want it to contain %q", s.Detail, want)
	}
	if strings.Contains(s.Detail, "no evidence") {
		t.Errorf("declared detail still carries the refusal: %q", s.Detail)
	}

	seedAbsentSDEXTrade(t, ctx, chAddr, 20)
	if err := run("-absent-by-design", "sdex"); err != nil {
		t.Fatalf("declared-but-present run: %v", err)
	}
	s = sdex()
	if s.Complete || s.ProjectionOK || s.AbsentByDesign || !s.AbsentDeclaredButPresent {
		t.Fatalf("declared, lake holds a trade: complete=%v projection_ok=%v absent=%v present=%v detail=%q, want the normal failing verdict and the present flag",
			s.Complete, s.ProjectionOK, s.AbsentByDesign, s.AbsentDeclaredButPresent, s.Detail)
	}
	if !strings.Contains(s.Detail, "absent-by-design declaration ignored: the lake holds 1 expected trades row(s) in [2,40]") {
		t.Errorf("declared-but-present detail = %q", s.Detail)
	}
}

// seedAbsentSDEXTrade writes one successful ManageSellOffer at ledger that crossed one order-book offer.
func seedAbsentSDEXTrade(t *testing.T, ctx context.Context, addr string, ledger uint32) {
	t.Helper()
	const (
		source = "GBNZILSTVQZ4R7IKQDGHYGY2QXL5QOFJYQMXPKWRRM5PAV7Y4M67AQUA"
		seller = "GCEZWKCA5VLDNRLN3RPRJMRZOX3Z6G5CHCGSNFHEYVXM3XOJMDS674JZ"
	)
	usdc := xdr.MustNewCreditAsset("USDC", seller)
	body, err := xdr.MarshalBase64(xdr.OperationBody{
		Type: xdr.OperationTypeManageSellOffer,
		ManageSellOfferOp: &xdr.ManageSellOfferOp{
			Selling: xdr.MustNewNativeAsset(), Buying: usdc, Amount: 10_000_000, Price: xdr.Price{N: 1, D: 2},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := xdr.MarshalBase64(xdr.OperationResult{
		Code: xdr.OperationResultCodeOpInner,
		Tr: &xdr.OperationResultTr{
			Type: xdr.OperationTypeManageSellOffer,
			ManageSellOfferResult: &xdr.ManageSellOfferResult{
				Code: xdr.ManageSellOfferResultCodeManageSellOfferSuccess,
				Success: &xdr.ManageOfferSuccessResult{
					OffersClaimed: []xdr.ClaimAtom{{
						Type: xdr.ClaimAtomTypeClaimAtomTypeOrderBook,
						OrderBook: &xdr.ClaimOfferAtom{
							SellerId: xdr.MustAddress(seller), OfferId: 7,
							AssetSold: usdc, AmountSold: 5_000_000,
							AssetBought: xdr.MustNewNativeAsset(), AmountBought: 10_000_000,
						},
					}},
					Offer: xdr.ManageOfferSuccessResultOffer{Effect: xdr.ManageOfferEffectManageOfferDeleted},
				},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	conn, err := clickhouse.Open(&clickhouse.Options{Addr: []string{addr}, Auth: clickhouse.Auth{Database: "stellar"}, DialTimeout: 10 * time.Second})
	if err != nil {
		t.Fatalf("dial clickhouse: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	hash := fmt.Sprintf("%064d", ledger)
	closeAt := time.Date(2019, 3, 1, 0, 0, 0, 0, time.UTC)
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO stellar.operations (ledger_seq, close_time, tx_hash, tx_index, op_index, op_type, source_account, body_xdr)
		  VALUES (?, ?, ?, 0, 0, 'OperationTypeManageSellOffer', ?, ?)`, []any{ledger, closeAt, hash, source, body}},
		{`INSERT INTO stellar.operation_results (ledger_seq, tx_hash, op_index, result_code, result_xdr)
		  VALUES (?, ?, 0, 0, ?)`, []any{ledger, hash, result}},
		{`INSERT INTO stellar.transactions (ledger_seq, close_time, tx_hash, tx_index, source_account, fee_charged, max_fee,
		  operation_count, successful, result_code, memo_type, memo)
		  VALUES (?, ?, ?, 0, ?, 100, 200, 1, 1, 0, 'none', '')`, []any{ledger, closeAt, hash, source}},
	} {
		if err := conn.Exec(ctx, q.sql, q.args...); err != nil {
			t.Fatalf("seed trade: %v", err)
		}
	}
}
