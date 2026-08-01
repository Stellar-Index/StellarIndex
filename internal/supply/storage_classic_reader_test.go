package supply

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"math/big"
	"strings"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// fakeClassicStore satisfies ClassicSupplyStore with in-memory
// values so the reader logic can be unit-tested without a real
// DB.
type fakeClassicStore struct {
	trustlineSum *big.Int
	claimableSum *big.Int
	lpSum        *big.Int
	sacSum       *big.Int
	// per-(account, asset) trustline lookup; key is "account:assetKey".
	// Used for issuer balance + LockedSet.Accounts.
	trustlinePerAccount map[string]*big.Int
	// per-(contract, asset) SAC lookup; key is "contract:assetKey".
	// Used for LockedSet.Contracts.
	sacPerContract map[string]*big.Int

	wantErrSum bool
	// wantErrMinLedger makes MinClassicComponentLedger fail, exercising
	// the fail-permissive freshness-gate path (F-1236 / W6-sweep-1).
	wantErrMinLedger bool
}

func (f *fakeClassicStore) SumTrustlineBalancesAtOrBefore(_ context.Context, _ string, _ uint32) (*big.Int, error) {
	if f.wantErrSum {
		return nil, errors.New("trustline sum boom")
	}
	return f.trustlineSum, nil
}

func (f *fakeClassicStore) SumClaimableBalancesAtOrBefore(_ context.Context, _ string, _ uint32) (*big.Int, error) {
	return f.claimableSum, nil
}

func (f *fakeClassicStore) SumLPReservesAtOrBefore(_ context.Context, _ string, _ uint32) (*big.Int, error) {
	return f.lpSum, nil
}

func (f *fakeClassicStore) SumSACBalancesAtOrBefore(_ context.Context, _ string, _ uint32) (*big.Int, error) {
	return f.sacSum, nil
}

func (f *fakeClassicStore) TrustlineBalanceForAccountAtOrBefore(_ context.Context, accountID, assetKey string, _ uint32) (*big.Int, error) {
	key := accountID + ":" + assetKey
	if v, ok := f.trustlinePerAccount[key]; ok {
		return v, nil
	}
	return big.NewInt(0), nil
}

func (f *fakeClassicStore) SACBalanceForContractAtOrBefore(_ context.Context, contractID, assetKey string, _ uint32) (*big.Int, error) {
	key := contractID + ":" + assetKey
	if v, ok := f.sacPerContract[key]; ok {
		return v, nil
	}
	return big.NewInt(0), nil
}

// MinClassicComponentLedger — fakes a single per-asset value;
// tests that don't care leave it at 0 (gate-skip).
func (f *fakeClassicStore) MinClassicComponentLedger(_ context.Context, _ string, _ uint32) (uint32, error) {
	if f.wantErrMinLedger {
		return 0, errors.New("min-component-ledger boom")
	}
	return 0, nil
}

func mustClassic(t *testing.T, code, issuer string) canonical.Asset {
	t.Helper()
	a, err := canonical.NewClassicAsset(code, issuer)
	if err != nil {
		t.Fatalf("NewClassicAsset: %v", err)
	}
	return a
}

const tIssuer = "GAAQCAIBAEAQCAIBAEAQCAIBAEAQCAIBAEAQCAIBAEAQCAIBAEAQDZ7H"

func TestStorageClassicSupplyReader_HappyPath(t *testing.T) {
	store := &fakeClassicStore{
		trustlineSum: big.NewInt(1000),
		claimableSum: big.NewInt(50),
		lpSum:        big.NewInt(20),
		sacSum:       big.NewInt(30),
	}
	r := NewStorageClassicSupplyReader(store, ClassicSupplyReaderOptions{})

	asset := mustClassic(t, "USDC", tIssuer)
	got, err := r.ClassicSupplyAt(context.Background(), asset, LockedSet{}, 100)
	if err != nil {
		t.Fatalf("ClassicSupplyAt: %v", err)
	}
	if got.Trustline.Int64() != 1000 {
		t.Errorf("Trustline=%s want 1000", got.Trustline)
	}
	if got.Claimable.Int64() != 50 {
		t.Errorf("Claimable=%s want 50", got.Claimable)
	}
	if got.LPReserve.Int64() != 20 {
		t.Errorf("LPReserve=%s want 20", got.LPReserve)
	}
	if got.SACWrapped.Int64() != 30 {
		t.Errorf("SACWrapped=%s want 30", got.SACWrapped)
	}
	if got.IssuerBalance.Sign() != 0 {
		t.Errorf("IssuerBalance=%s want 0 (no issuer trustline configured)", got.IssuerBalance)
	}
	if got.LockedAccountBalances.Sign() != 0 {
		t.Errorf("LockedAccountBalances=%s want 0 (empty LockedSet)", got.LockedAccountBalances)
	}
	if got.LockedContractBalances.Sign() != 0 {
		t.Errorf("LockedContractBalances=%s want 0 (empty LockedSet)", got.LockedContractBalances)
	}
}

func TestStorageClassicSupplyReader_RejectsNonClassic(t *testing.T) {
	r := NewStorageClassicSupplyReader(&fakeClassicStore{}, ClassicSupplyReaderOptions{})
	_, err := r.ClassicSupplyAt(context.Background(), canonical.NativeAsset(), LockedSet{}, 1)
	if !errors.Is(err, ErrNotClassic) {
		t.Errorf("err=%v want wrapping ErrNotClassic", err)
	}
}

func TestStorageClassicSupplyReader_PropagatesSumError(t *testing.T) {
	store := &fakeClassicStore{wantErrSum: true}
	r := NewStorageClassicSupplyReader(store, ClassicSupplyReaderOptions{})
	asset := mustClassic(t, "USDC", tIssuer)
	_, err := r.ClassicSupplyAt(context.Background(), asset, LockedSet{}, 1)
	if err == nil || !strings.Contains(err.Error(), "trustline") {
		t.Errorf("err=%v should mention trustline failure", err)
	}
}

// TestStorageClassicSupplyReader_MinLedgerErrorWarnsAndStaysPermissive
// pins W6-sweep-1: a MinClassicComponentLedger query error is
// fail-permissive (MinComponentLedger=0, snapshot still returned) but
// must no longer be silent — the reader emits a WARN so the operator
// sees the stale-component gate drop to permissive.
func TestStorageClassicSupplyReader_MinLedgerErrorWarnsAndStaysPermissive(t *testing.T) {
	store := &fakeClassicStore{
		trustlineSum:     big.NewInt(1000),
		claimableSum:     big.NewInt(0),
		lpSum:            big.NewInt(0),
		sacSum:           big.NewInt(0),
		wantErrMinLedger: true,
	}
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
	r := NewStorageClassicSupplyReader(store, ClassicSupplyReaderOptions{Logger: logger})

	asset := mustClassic(t, "USDC", tIssuer)
	got, err := r.ClassicSupplyAt(context.Background(), asset, LockedSet{}, 100)
	if err != nil {
		t.Fatalf("ClassicSupplyAt: %v (must stay fail-permissive, not error)", err)
	}
	// Fail-permissive posture preserved: snapshot returned, gate open.
	if got.MinComponentLedger != 0 {
		t.Errorf("MinComponentLedger=%d want 0 (permissive on query error)", got.MinComponentLedger)
	}
	if got.Trustline.Int64() != 1000 {
		t.Errorf("Trustline=%s want 1000 (snapshot still correct)", got.Trustline)
	}
	// The failure is now observable.
	logged := buf.String()
	if !strings.Contains(logged, "level=WARN") {
		t.Errorf("expected a WARN log line, got: %q", logged)
	}
	if !strings.Contains(logged, "MinClassicComponentLedger") {
		t.Errorf("WARN line should name the failing query, got: %q", logged)
	}
}

// TestStorageClassicSupplyReader_IssuerBalanceLookedUp — when the
// issuer is on their own trustline (rare but legal), the reader
// surfaces the balance for the algorithm to subtract.
func TestStorageClassicSupplyReader_IssuerBalanceLookedUp(t *testing.T) {
	assetKey := "USDC:" + tIssuer
	store := &fakeClassicStore{
		trustlineSum: big.NewInt(1000),
		claimableSum: big.NewInt(0),
		lpSum:        big.NewInt(0),
		sacSum:       big.NewInt(0),
		trustlinePerAccount: map[string]*big.Int{
			tIssuer + ":" + assetKey: big.NewInt(123),
		},
	}
	r := NewStorageClassicSupplyReader(store, ClassicSupplyReaderOptions{})
	asset := mustClassic(t, "USDC", tIssuer)
	got, err := r.ClassicSupplyAt(context.Background(), asset, LockedSet{}, 1)
	if err != nil {
		t.Fatalf("ClassicSupplyAt: %v", err)
	}
	if got.IssuerBalance.Int64() != 123 {
		t.Errorf("IssuerBalance=%s want 123", got.IssuerBalance)
	}
}

// TestStorageClassicSupplyReader_LockedSetSummed — operator-
// configured locked accounts and contracts contribute to the
// per-component sums.
func TestStorageClassicSupplyReader_LockedSetSummed(t *testing.T) {
	assetKey := "USDC:" + tIssuer
	store := &fakeClassicStore{
		trustlineSum: big.NewInt(0),
		claimableSum: big.NewInt(0),
		lpSum:        big.NewInt(0),
		sacSum:       big.NewInt(0),
		trustlinePerAccount: map[string]*big.Int{
			"G_LOCKED_1:" + assetKey: big.NewInt(100),
			"G_LOCKED_2:" + assetKey: big.NewInt(200),
		},
		sacPerContract: map[string]*big.Int{
			"C_LOCKED_1:" + assetKey: big.NewInt(50),
		},
	}
	r := NewStorageClassicSupplyReader(store, ClassicSupplyReaderOptions{})
	asset := mustClassic(t, "USDC", tIssuer)
	locked := LockedSet{
		Accounts:  []string{"G_LOCKED_1", "G_LOCKED_2"},
		Contracts: []string{"C_LOCKED_1"},
	}
	got, err := r.ClassicSupplyAt(context.Background(), asset, locked, 1)
	if err != nil {
		t.Fatalf("ClassicSupplyAt: %v", err)
	}
	if got.LockedAccountBalances.Int64() != 300 {
		t.Errorf("LockedAccountBalances=%s want 300", got.LockedAccountBalances)
	}
	if got.LockedContractBalances.Int64() != 50 {
		t.Errorf("LockedContractBalances=%s want 50", got.LockedContractBalances)
	}
}
