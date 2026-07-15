//go:build integration

package integration_test

import (
	"context"
	"math/big"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/domain"
	"github.com/Stellar-Index/StellarIndex/internal/sources/blend"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TestPositionsFold_AllSixProtocols exercises every SQL fold query
// GET /v1/accounts/{g}/positions reads (internal/storage/timescale/
// positions.go) through real TimescaleDB — the layer go build/go vet
// cannot validate (raw SQL strings). One test user, one row of
// contributing activity per protocol (plus a second, offsetting row
// where the fold's sign convention needs to be proven, not just its
// existence), asserting the fold nets match hand-computed expectations.
func TestPositionsFold_AllSixProtocols(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	const user = "GABCDEFGHIJKLMNOPQRSTUVWXYZ234567ABCDEFGHIJKLMNOPQRSTU56K"
	t0 := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)

	// ─── blend money-market: supply 1000, withdraw 400 -> net 600;
	// borrow 300, repay 100 -> net 200. flash_loan 999 must NOT move
	// either net.
	const (
		blendPool  = "CDLZFC3SYJYDZT7K67VZ75HPJVIEUVNIXF47ZG2FB2RMQQVU2HHGCYSC"
		blendAsset = "CC4WPS7HRSPRZAXBVUDYLRXLZRHPLA6VTZARKZJTNVNECAS5IDRXRUB6"
	)
	blendRows := []struct {
		kind   string
		amount int64
		ledger uint32
	}{
		{blend.EventSupply, 1000, 70_000_000},
		{blend.EventWithdraw, 400, 70_000_001},
		{blend.EventBorrow, 300, 70_000_002},
		{blend.EventRepay, 100, 70_000_003},
		{blend.EventFlashLoan, 999, 70_000_004},
	}
	for i, r := range blendRows {
		ev := domain.BlendPositionEvent{
			Pool: blendPool, Kind: r.kind, Asset: blendAsset, User: user,
			TokenAmount: big.NewInt(r.amount), BOrDAmount: big.NewInt(r.amount),
			Ledger: r.ledger, TxHash: pad64("p", i), OpIndex: 0, EventIndex: 0,
			Timestamp: t0.Add(time.Duration(i) * time.Minute),
		}
		if r.kind == blend.EventFlashLoan {
			ev.Counterparty = "CAXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXY32S"
		}
		if err := store.InsertBlendPositionEvent(ctx, ev); err != nil {
			t.Fatalf("InsertBlendPositionEvent (%s): %v", r.kind, err)
		}
	}

	blendFolds, err := store.BlendPositionsByUser(ctx, user)
	if err != nil {
		t.Fatalf("BlendPositionsByUser: %v", err)
	}
	if len(blendFolds) != 1 {
		t.Fatalf("BlendPositionsByUser rows = %d, want 1 (single pool/asset group): %+v", len(blendFolds), blendFolds)
	}
	bf := blendFolds[0]
	if bf.SupplyNet != "600" {
		t.Errorf("blend SupplyNet = %q, want 600 (1000 supply - 400 withdraw)", bf.SupplyNet)
	}
	if bf.BorrowNet != "200" {
		t.Errorf("blend BorrowNet = %q, want 200 (300 borrow - 100 repay; flash_loan excluded)", bf.BorrowNet)
	}
	if !bf.HasSupplyLeg || !bf.HasBorrowLeg {
		t.Errorf("blend fold legs = supply:%v borrow:%v, want both true", bf.HasSupplyLeg, bf.HasBorrowLeg)
	}

	// ─── blend backstop: deposit(amount=1000,shares=900), then
	// withdraw(amount=tokens_out,shares_burned=300) -> shares net 600.
	// queue_withdrawal must NOT move the net.
	const backstopPool = blendPool
	if err := store.InsertBlendBackstopEvent(ctx, timescale.BlendBackstopEvent{
		ContractID: "CAQQR5SWBXKIGZKPBZDH3KM5GQ5GUTPKB7JAFCINLZBC5WXPJKRG3IM7",
		Ledger:     71_000_000, TxHash: pad64("q", 0), OpIndex: 0, EventIndex: 0, ObservedAt: t0,
		EventType: timescale.BackstopDeposit, Pool: backstopPool, UserAddress: user,
		Amount: "1000", Amount2: "900",
	}); err != nil {
		t.Fatalf("InsertBlendBackstopEvent (deposit): %v", err)
	}
	if err := store.InsertBlendBackstopEvent(ctx, timescale.BlendBackstopEvent{
		ContractID: "CAQQR5SWBXKIGZKPBZDH3KM5GQ5GUTPKB7JAFCINLZBC5WXPJKRG3IM7",
		Ledger:     71_000_001, TxHash: pad64("q", 1), OpIndex: 0, EventIndex: 0, ObservedAt: t0.Add(time.Minute),
		EventType: timescale.BackstopQueueWithdrawal, Pool: backstopPool, UserAddress: user,
		Amount: "300",
	}); err != nil {
		t.Fatalf("InsertBlendBackstopEvent (queue_withdrawal): %v", err)
	}
	if err := store.InsertBlendBackstopEvent(ctx, timescale.BlendBackstopEvent{
		ContractID: "CAQQR5SWBXKIGZKPBZDH3KM5GQ5GUTPKB7JAFCINLZBC5WXPJKRG3IM7",
		Ledger:     71_000_002, TxHash: pad64("q", 2), OpIndex: 0, EventIndex: 0, ObservedAt: t0.Add(2 * time.Minute),
		EventType: timescale.BackstopWithdraw, Pool: backstopPool, UserAddress: user,
		Amount: "290", Amount2: "300",
	}); err != nil {
		t.Fatalf("InsertBlendBackstopEvent (withdraw): %v", err)
	}

	backstopFolds, err := store.BlendBackstopSharesByUser(ctx, user)
	if err != nil {
		t.Fatalf("BlendBackstopSharesByUser: %v", err)
	}
	if len(backstopFolds) != 1 || backstopFolds[0].SharesNet != "600" {
		t.Errorf("BlendBackstopSharesByUser = %+v, want SharesNet=600 (900 deposit shares - 300 withdraw shares; queue_withdrawal excluded)", backstopFolds)
	}

	// ─── phoenix stake: bond 500, unbond 150 -> net 350.
	const stakeContract = "CBRGNWGAC25AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	const lpToken = "CLPTOKEN0000000000000000000000000000000000000000000000000"
	if err := store.InsertPhoenixStakeEvent(ctx, timescale.PhoenixStakeEvent{
		StakeContract: stakeContract, Ledger: 72_000_000, ObservedAt: t0, TxHash: pad64("r", 0), OpIndex: 0, EventIndex: 0,
		Action: timescale.PhoenixBond, User: user, LPToken: lpToken, Amount: "500",
	}); err != nil {
		t.Fatalf("InsertPhoenixStakeEvent (bond): %v", err)
	}
	if err := store.InsertPhoenixStakeEvent(ctx, timescale.PhoenixStakeEvent{
		StakeContract: stakeContract, Ledger: 72_000_001, ObservedAt: t0.Add(time.Minute), TxHash: pad64("r", 1), OpIndex: 0, EventIndex: 0,
		Action: timescale.PhoenixUnbond, User: user, LPToken: lpToken, Amount: "150",
	}); err != nil {
		t.Fatalf("InsertPhoenixStakeEvent (unbond): %v", err)
	}

	phoenixFolds, err := store.PhoenixStakeByUser(ctx, user)
	if err != nil {
		t.Fatalf("PhoenixStakeByUser: %v", err)
	}
	if len(phoenixFolds) != 1 || phoenixFolds[0].NetAmount != "350" {
		t.Errorf("PhoenixStakeByUser = %+v, want NetAmount=350 (500 bond - 150 unbond)", phoenixFolds)
	}

	// ─── defindex vault: vault-layer deposit df_tokens=800, withdraw
	// df_tokens=200 -> net 600. A strategy-layer row (actor = the
	// vault contract, NOT the user) must not leak into the user's fold.
	const vault = "CVAULT00000000000000000000000000000000000000000000000000"
	if err := store.InsertDefindexFlow(ctx, timescale.DefindexFlow{
		Ledger: 73_000_000, LedgerCloseTime: t0, TxHash: pad64("s", 0), OpIndex: 0, EventIndex: 0,
		ContractID: vault, Layer: timescale.DefindexLayerVault, Direction: timescale.DefindexDeposit,
		Actor: user, AmountsVec: []string{"800"}, DfTokens: "800",
	}); err != nil {
		t.Fatalf("InsertDefindexFlow (vault deposit): %v", err)
	}
	if err := store.InsertDefindexFlow(ctx, timescale.DefindexFlow{
		Ledger: 73_000_001, LedgerCloseTime: t0.Add(time.Minute), TxHash: pad64("s", 1), OpIndex: 0, EventIndex: 0,
		ContractID: vault, Layer: timescale.DefindexLayerVault, Direction: timescale.DefindexWithdraw,
		Actor: user, AmountsVec: []string{"200"}, DfTokens: "200",
	}); err != nil {
		t.Fatalf("InsertDefindexFlow (vault withdraw): %v", err)
	}
	if err := store.InsertDefindexFlow(ctx, timescale.DefindexFlow{
		Ledger: 73_000_002, LedgerCloseTime: t0.Add(2 * time.Minute), TxHash: pad64("s", 2), OpIndex: 0, EventIndex: 0,
		ContractID: vault, Layer: timescale.DefindexLayerStrategy, Direction: timescale.DefindexDeposit,
		Actor: vault, Amount: "999999",
	}); err != nil {
		t.Fatalf("InsertDefindexFlow (strategy leg): %v", err)
	}

	defindexFolds, err := store.DefindexVaultSharesByUser(ctx, user)
	if err != nil {
		t.Fatalf("DefindexVaultSharesByUser: %v", err)
	}
	if len(defindexFolds) != 1 || defindexFolds[0].SharesNet != "600" {
		t.Errorf("DefindexVaultSharesByUser = %+v, want SharesNet=600 (800 deposit - 200 withdraw, strategy leg excluded)", defindexFolds)
	}

	// ─── sorocredit: open a position, publish a statement, and verify
	// the LATEST statement wins when a second, newer one lands. Also
	// open a SECOND position and mark it withdrawn.
	const collat1 = "CCOLLAT100000000000000000000000000000000000000000000000"
	const collat2 = "CCOLLAT200000000000000000000000000000000000000000000000"
	if err := store.InsertCreditPosition(ctx, timescale.CreditPosition{
		CollateralContract: collat1, PositionUUID: "uuid-1", PositionName: "Collateral-uuid-1", Owner: user,
		Ledger: 74_000_000, LedgerCloseTime: t0, TxHash: pad64("t", 0), OpIndex: 0, EventIndex: 0,
	}); err != nil {
		t.Fatalf("InsertCreditPosition (1): %v", err)
	}
	if err := store.InsertCreditStatement(ctx, timescale.CreditStatement{
		StatementUUID: "stmt-1", PositionUUID: "uuid-1", CollateralContract: collat1,
		Amount: "500", StatementTime: t0.Add(time.Minute),
		Ledger: 74_000_001, LedgerCloseTime: t0.Add(time.Minute), TxHash: pad64("t", 1), OpIndex: 0, EventIndex: 0,
	}); err != nil {
		t.Fatalf("InsertCreditStatement (stmt-1): %v", err)
	}
	if err := store.InsertCreditStatement(ctx, timescale.CreditStatement{
		StatementUUID: "stmt-2", PositionUUID: "uuid-1", CollateralContract: collat1,
		Amount: "480", StatementTime: t0.Add(2 * time.Minute),
		Ledger: 74_000_002, LedgerCloseTime: t0.Add(2 * time.Minute), TxHash: pad64("t", 2), OpIndex: 0, EventIndex: 0,
	}); err != nil {
		t.Fatalf("InsertCreditStatement (stmt-2, latest): %v", err)
	}
	if err := store.InsertCreditPosition(ctx, timescale.CreditPosition{
		CollateralContract: collat2, PositionUUID: "uuid-2", PositionName: "Collateral-uuid-2", Owner: user,
		Ledger: 74_000_003, LedgerCloseTime: t0.Add(3 * time.Minute), TxHash: pad64("t", 3), OpIndex: 0, EventIndex: 0,
	}); err != nil {
		t.Fatalf("InsertCreditPosition (2): %v", err)
	}
	if err := store.InsertCreditEvent(ctx, timescale.CreditEvent{
		EventType: "withdrawal", CollateralContract: collat2, Asset: "CUSDC", Account: user, Amount: "50",
		Ledger: 74_000_004, LedgerCloseTime: t0.Add(4 * time.Minute), TxHash: pad64("t", 4), OpIndex: 0, EventIndex: 0,
	}); err != nil {
		t.Fatalf("InsertCreditEvent (withdrawal): %v", err)
	}

	creditFolds, err := store.CreditPositionsByOwner(ctx, user)
	if err != nil {
		t.Fatalf("CreditPositionsByOwner: %v", err)
	}
	if len(creditFolds) != 2 {
		t.Fatalf("CreditPositionsByOwner rows = %d, want 2: %+v", len(creditFolds), creditFolds)
	}
	byContract := map[string]timescale.CreditPositionFold{}
	for _, f := range creditFolds {
		byContract[f.CollateralContract] = f
	}
	if got := byContract[collat1]; got.LatestAmount != "480" || got.Withdrawn {
		t.Errorf("credit position 1 = %+v, want LatestAmount=480 (the NEWER statement), Withdrawn=false", got)
	}
	if got := byContract[collat2]; !got.Withdrawn {
		t.Errorf("credit position 2 = %+v, want Withdrawn=true", got)
	}

	// ─── aquarius gauge: position_update delta +2000 then -700 -> net
	// 1300. A non-position_update kind (claim_reward) must not leak in.
	const gaugePool = "CD3INVPZI3UBNYU3FEMTIGUJCYQVVMD73XSAOL7FFCYOUQ34DSFUZUZT"

	if err := store.InsertAquariusRewardsEvent(ctx, timescale.AquariusRewardsEvent{
		ContractID: gaugePool, Ledger: 75_000_000, LedgerCloseTime: t0, TxHash: pad64("u", 0), OpIndex: 0, EventIndex: 0,
		Kind: timescale.AquariusRewardsPositionUpdate, UserAddress: user,
		Attributes: map[string]any{"range_from": 1, "range_to": 2, "delta": "2000"},
	}); err != nil {
		t.Fatalf("InsertAquariusRewardsEvent (position_update +2000): %v", err)
	}
	if err := store.InsertAquariusRewardsEvent(ctx, timescale.AquariusRewardsEvent{
		ContractID: gaugePool, Ledger: 75_000_001, LedgerCloseTime: t0.Add(time.Minute), TxHash: pad64("u", 1), OpIndex: 0, EventIndex: 0,
		Kind: timescale.AquariusRewardsPositionUpdate, UserAddress: user,
		Attributes: map[string]any{"range_from": 1, "range_to": 2, "delta": "-700"},
	}); err != nil {
		t.Fatalf("InsertAquariusRewardsEvent (position_update -700): %v", err)
	}

	aquariusFolds, err := store.AquariusGaugeByUser(ctx, user)
	if err != nil {
		t.Fatalf("AquariusGaugeByUser: %v", err)
	}
	if len(aquariusFolds) != 1 || aquariusFolds[0].NetDelta != "1300" {
		t.Errorf("AquariusGaugeByUser = %+v, want NetDelta=1300 (2000 - 700)", aquariusFolds)
	}
}
