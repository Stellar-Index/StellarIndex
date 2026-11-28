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
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/ops/chops"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

const (
	absentTip     = 40
	absentSource  = "GBNZILSTVQZ4R7IKQDGHYGY2QXL5QOFJYQMXPKWRRM5PAV7Y4M67AQUA"
	absentSeller  = "GCEZWKCA5VLDNRLN3RPRJMRZOX3Z6G5CHCGSNFHEYVXM3XOJMDS674JZ"
	absentForeign = "CDLZFC3SYJYDZT7K67VZ75HPJVIEUVNIXF47ZG2FB2RMQQVU2HHGCYSC"
)

// TestComputeCompleteness_AbsentByDesign drives the real compute-completeness -pass path (what the units run, recognition
// scanned) over a quiet network's lake, ledgers [2,40] with no SDEX trade and an empty served tier:
//   - headers declare one op per ledger but stellar.operations is empty: declared or not, "no evidence" stands;
//   - the classic tables made whole (one payment per ledger, still no trade): the declaration applies;
//   - a trade lands: the declaration is ignored and flagged.
//
// Its own ClickHouse container, so the shared lake's tip never moves.
func TestComputeCompleteness_AbsentByDesign(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
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
	cfg := fmt.Sprintf("[storage]\npostgres_dsn = %q\n\n[stellar]\nsoroban_genesis_ledger = 2\n", dsn)
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	conn, err := clickhouse.Open(&clickhouse.Options{Addr: []string{chAddr}, Auth: clickhouse.Auth{Database: "stellar"}, DialTimeout: 10 * time.Second})
	if err != nil {
		t.Fatalf("dial clickhouse: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	seqs := make([]uint32, 0, absentTip-1)
	for s := uint32(2); s <= absentTip; s++ {
		seqs = append(seqs, s)
	}
	// Headers declare one tx and one op per ledger; the op tables stay empty until the second phase.
	insertContiguityLedgers(ctx, t, chAddr, seqs...)
	seedAbsentForeignEvent(t, ctx, conn)

	run := func(extra ...string) error {
		args := []string{"compute-completeness", "-config", cfgPath, "-ch", "-ch-addr", chAddr, "-to", fmt.Sprint(absentTip), "-pass", "-write"}
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

	for name, want := range map[string]string{"sdx": "matches no reconciliation source", "soroswap": "gated on configured contract identity"} {
		if err := run("-absent-by-design", name); err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("-absent-by-design %s: err = %v, want a failure containing %q", name, err, want)
		}
	}

	if err := run(); err != nil {
		t.Fatalf("undeclared run: %v", err)
	}
	if s := sdex(); s.Complete || s.ProjectionOK || s.AbsentByDesign || !s.RecognitionOK || !strings.Contains(s.Detail, "projection: no evidence") {
		t.Fatalf("undeclared: complete=%v projection_ok=%v absent=%v recognition_ok=%v detail=%q, want the no-evidence refusal",
			s.Complete, s.ProjectionOK, s.AbsentByDesign, s.RecognitionOK, s.Detail)
	}

	if err := run("-absent-by-design", "sdex"); err != nil {
		t.Fatalf("declared run, short op tables: %v", err)
	}
	if s := sdex(); s.Complete || s.ProjectionOK || s.AbsentByDesign || s.AbsentDeclaredButPresent ||
		!strings.Contains(s.Detail, "projection: no evidence") || !strings.Contains(s.Detail, "stellar.operations partition 0 holds 0 of the 39 rows") {
		t.Fatalf("declared, operations missing: complete=%v projection_ok=%v absent=%v present=%v detail=%q, want not applied naming operations",
			s.Complete, s.ProjectionOK, s.AbsentByDesign, s.AbsentDeclaredButPresent, s.Detail)
	}

	seedAbsentPayments(t, ctx, conn, seqs)
	if err := run("-absent-by-design", "sdex"); err != nil {
		t.Fatalf("declared run, whole tables: %v", err)
	}
	s := sdex()
	if !s.Complete || !s.ProjectionOK || !s.AbsentByDesign || s.AbsentDeclaredButPresent || s.ProjectionVerifiedFrom != 2 {
		t.Fatalf("declared, whole tables and no trade: complete=%v projection_ok=%v absent=%v present=%v verified_from=%d detail=%q",
			s.Complete, s.ProjectionOK, s.AbsentByDesign, s.AbsentDeclaredButPresent, s.ProjectionVerifiedFrom, s.Detail)
	}
	if want := "projection: absent by design on this network (declared); lake holds no trades in [2,40] and its raw tables are whole"; !strings.Contains(s.Detail, want) {
		t.Errorf("declared detail = %q, want it to contain %q", s.Detail, want)
	}
	if strings.Contains(s.Detail, "no evidence") {
		t.Errorf("declared detail still carries the refusal: %q", s.Detail)
	}

	seedAbsentSDEXTrade(t, ctx, conn, 20)
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

func absentExec(t *testing.T, ctx context.Context, conn driver.Conn, q string, args ...any) {
	t.Helper()
	if err := conn.Exec(ctx, q, args...); err != nil {
		t.Fatalf("exec %.80q: %v", q, err)
	}
}

func absentB64(t *testing.T, v any) string {
	t.Helper()
	s, err := xdr.MarshalBase64(v)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// seedAbsentForeignEvent gives the recognition scan one shape to read, on a contract no source owns.
func seedAbsentForeignEvent(t *testing.T, ctx context.Context, conn driver.Conn) {
	t.Helper()
	sym := xdr.ScSymbol("ping")
	topic := absentB64(t, xdr.ScVal{Type: xdr.ScValTypeScvSymbol, Sym: &sym})
	absentExec(t, ctx, conn, `INSERT INTO stellar.contract_events
		(ledger_seq, close_time, tx_hash, op_index, event_index, contract_id, event_type, topic_count, topic_0_sym, topics_xdr, data_xdr, op_args_xdr, in_successful_call)
		VALUES (10, ?, ?, 0, 0, ?, 'contract', 1, 'ping', [?], '', [], 1)`,
		time.Date(2019, 3, 1, 0, 0, 0, 0, time.UTC), fmt.Sprintf("%064d", 10), absentForeign, topic)
}

// seedAbsentPayments writes, for each ledger, the one successful tx with one payment op its header declares.
func seedAbsentPayments(t *testing.T, ctx context.Context, conn driver.Conn, seqs []uint32) {
	t.Helper()
	body := absentB64(t, xdr.OperationBody{Type: xdr.OperationTypePayment, PaymentOp: &xdr.PaymentOp{
		Destination: xdr.MustMuxedAddress(absentSeller), Asset: xdr.MustNewNativeAsset(), Amount: 42_0000000,
	}})
	result := absentB64(t, xdr.OperationResult{Code: xdr.OperationResultCodeOpInner, Tr: &xdr.OperationResultTr{
		Type: xdr.OperationTypePayment, PaymentResult: &xdr.PaymentResult{Code: xdr.PaymentResultCodePaymentSuccess},
	}})
	closeAt := time.Date(2019, 3, 1, 0, 0, 0, 0, time.UTC)
	for _, seq := range seqs {
		hash := fmt.Sprintf("a%063d", seq) // hex: the SDEX decoder reads tx_hash as hex
		absentExec(t, ctx, conn, `INSERT INTO stellar.operations (ledger_seq, close_time, tx_hash, tx_index, op_index, op_type, source_account, body_xdr)
			VALUES (?, ?, ?, 0, 0, 'OperationTypePayment', ?, ?)`, seq, closeAt, hash, absentSource, body)
		absentExec(t, ctx, conn, `INSERT INTO stellar.operation_results (ledger_seq, tx_hash, op_index, result_code, result_xdr)
			VALUES (?, ?, 0, 0, ?)`, seq, hash, result)
		absentExec(t, ctx, conn, `INSERT INTO stellar.transactions (ledger_seq, close_time, tx_hash, tx_index, source_account, fee_charged, max_fee,
			operation_count, successful, result_code, memo_type, memo)
			VALUES (?, ?, ?, 0, ?, 100, 200, 1, 1, 0, 'none', '')`, seq, closeAt, hash, absentSource)
	}
}

// seedAbsentSDEXTrade writes one successful ManageSellOffer at ledger that crossed one order-book offer.
func seedAbsentSDEXTrade(t *testing.T, ctx context.Context, conn driver.Conn, ledger uint32) {
	t.Helper()
	usdc := xdr.MustNewCreditAsset("USDC", absentSeller)
	body := absentB64(t, xdr.OperationBody{
		Type: xdr.OperationTypeManageSellOffer,
		ManageSellOfferOp: &xdr.ManageSellOfferOp{
			Selling: xdr.MustNewNativeAsset(), Buying: usdc, Amount: 10_000_000, Price: xdr.Price{N: 1, D: 2},
		},
	})
	result := absentB64(t, xdr.OperationResult{
		Code: xdr.OperationResultCodeOpInner,
		Tr: &xdr.OperationResultTr{
			Type: xdr.OperationTypeManageSellOffer,
			ManageSellOfferResult: &xdr.ManageSellOfferResult{
				Code: xdr.ManageSellOfferResultCodeManageSellOfferSuccess,
				Success: &xdr.ManageOfferSuccessResult{
					OffersClaimed: []xdr.ClaimAtom{{
						Type: xdr.ClaimAtomTypeClaimAtomTypeOrderBook,
						OrderBook: &xdr.ClaimOfferAtom{
							SellerId: xdr.MustAddress(absentSeller), OfferId: 7,
							AssetSold: usdc, AmountSold: 5_000_000,
							AssetBought: xdr.MustNewNativeAsset(), AmountBought: 10_000_000,
						},
					}},
					Offer: xdr.ManageOfferSuccessResultOffer{Effect: xdr.ManageOfferEffectManageOfferDeleted},
				},
			},
		},
	})
	hash := fmt.Sprintf("b%063d", ledger)
	closeAt := time.Date(2019, 3, 1, 0, 0, 0, 0, time.UTC)
	absentExec(t, ctx, conn, `INSERT INTO stellar.operations (ledger_seq, close_time, tx_hash, tx_index, op_index, op_type, source_account, body_xdr)
		VALUES (?, ?, ?, 1, 0, 'OperationTypeManageSellOffer', ?, ?)`, ledger, closeAt, hash, absentSource, body)
	absentExec(t, ctx, conn, `INSERT INTO stellar.operation_results (ledger_seq, tx_hash, op_index, result_code, result_xdr)
		VALUES (?, ?, 0, 0, ?)`, ledger, hash, result)
	absentExec(t, ctx, conn, `INSERT INTO stellar.transactions (ledger_seq, close_time, tx_hash, tx_index, source_account, fee_charged, max_fee,
		operation_count, successful, result_code, memo_type, memo)
		VALUES (?, ?, ?, 1, ?, 100, 200, 1, 1, 0, 'none', '')`, ledger, closeAt, hash, absentSource)
}
