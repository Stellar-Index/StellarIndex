package supply

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"math/big"
	"strings"
	"testing"
	"time"

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
	sacObserved  bool
	// per-(account, asset) trustline lookup; key is "account:assetKey".
	// Used for issuer balance + LockedSet.Accounts.
	trustlinePerAccount map[string]*big.Int
	// per-(contract, asset) SAC lookup; key is "contract:assetKey".
	// Used for LockedSet.Contracts.
	sacPerContract map[string]*big.Int

	wantErrSum bool
	// wantErrMinLedger makes MinClassicComponentLedger fail, exercising
	// the fail-permissive freshness-gate path.
	wantErrMinLedger bool
	// minLedger is returned by MinClassicComponentLedger when
	// wantErrMinLedger is false; defaults to 0 (gate-skip).
	minLedger uint32
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

func (f *fakeClassicStore) SACBalanceObservationsExist(_ context.Context, _ string, _ uint32) (bool, error) {
	return f.sacObserved, nil
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
	return f.minLedger, nil
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
		sacObserved:  true,
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
	if !got.SACObserved {
		t.Error("SACObserved = false, want true — the store reported a real observation")
	}
}

// A zero SAC sum with no backing observation (SumSACBalancesAtOrBefore's COALESCE-to-zero) must arrive as
// SACObserved=false, not conflated with a genuine zero reading.
func TestStorageClassicSupplyReader_SACObservedFalsePropagates(t *testing.T) {
	store := &fakeClassicStore{
		trustlineSum: big.NewInt(1000),
		claimableSum: big.NewInt(0),
		lpSum:        big.NewInt(0),
		sacSum:       big.NewInt(0),
		sacObserved:  false,
	}
	r := NewStorageClassicSupplyReader(store, ClassicSupplyReaderOptions{})

	got, err := r.ClassicSupplyAt(context.Background(), mustClassic(t, "USDC", tIssuer), LockedSet{}, 100)
	if err != nil {
		t.Fatalf("ClassicSupplyAt: %v", err)
	}
	if got.SACObserved {
		t.Error("SACObserved = true, want false — the store reported no observation")
	}
}

// A non-zero MinClassicComponentLedger from storage must reach ClassicSupplyComponents.MinComponentLedger unchanged;
// other tests leave minLedger at zero (gate-skip), so none would catch a reader that drops the field.
func TestStorageClassicSupplyReader_MinLedgerPropagates(t *testing.T) {
	store := &fakeClassicStore{
		trustlineSum: big.NewInt(1000),
		claimableSum: big.NewInt(0),
		lpSum:        big.NewInt(0),
		sacSum:       big.NewInt(0),
		minLedger:    54_321,
	}
	r := NewStorageClassicSupplyReader(store, ClassicSupplyReaderOptions{})
	asset := mustClassic(t, "USDC", tIssuer)
	got, err := r.ClassicSupplyAt(context.Background(), asset, LockedSet{}, 100_000)
	if err != nil {
		t.Fatalf("ClassicSupplyAt: %v", err)
	}
	if got.MinComponentLedger != 54_321 {
		t.Errorf("MinComponentLedger=%d want 54321 (must propagate the storage anchor, not gate-skip)", got.MinComponentLedger)
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

// A MinClassicComponentLedger query error is fail-permissive (MinComponentLedger=0, snapshot still returned) but not
// silent: the reader emits a WARN so the operator sees the stale-component gate drop to permissive.
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
	// The failure is observable.
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

type fakeSEP41Store struct {
	totals          SEP41KindTotals
	totalsErr       error
	holderBalances  map[string]*big.Int // key: "holder:assetKey"
	holderLookupErr error
	// trustlines is TrustlineBalanceForAccountAtOrBefore's table, keyed
	// "account|assetKey".
	trustlines    map[string]*big.Int
	genesisSeeded bool
	genesisErr    error
	// minLedger is returned by MinSEP41ComponentLedger; defaults to 0
	// (gate-skip).
	minLedger uint32
}

func (f *fakeSEP41Store) SEP41KindTotalsAtOrBefore(_ context.Context, _ string, _ uint32) (SEP41KindTotals, error) {
	if f.totalsErr != nil {
		return SEP41KindTotals{}, f.totalsErr
	}
	return f.totals, nil
}

func (f *fakeSEP41Store) SACBalanceForContractAtOrBefore(_ context.Context, holder, assetKey string, _ uint32) (*big.Int, error) {
	if f.holderLookupErr != nil {
		return nil, f.holderLookupErr
	}
	key := holder + ":" + assetKey
	if v, ok := f.holderBalances[key]; ok {
		return v, nil
	}
	return big.NewInt(0), nil
}

func (f *fakeSEP41Store) TrustlineBalanceForAccountAtOrBefore(_ context.Context, accountID, assetKey string, _ uint32) (*big.Int, error) {
	if v, ok := f.trustlines[accountID+"|"+assetKey]; ok {
		return v, nil
	}
	return big.NewInt(0), nil
}

// MinSEP41ComponentLedger — fake returns minLedger (0 = gate-skip by default).
func (f *fakeSEP41Store) MinSEP41ComponentLedger(_ context.Context, _ string, _ uint32) (uint32, error) {
	return f.minLedger, nil
}

// SEP41GenesisBaselineSeeded — fake returns the configured flag (default
// false = not seeded).
func (f *fakeSEP41Store) SEP41GenesisBaselineSeeded(_ context.Context, _ string) (bool, error) {
	if f.genesisErr != nil {
		return false, f.genesisErr
	}
	return f.genesisSeeded, nil
}

const tContract = "CAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAABSC4"

func mustSorobanAsset(t *testing.T, contractID string) canonical.Asset {
	t.Helper()
	a, err := canonical.NewSorobanAsset(contractID)
	if err != nil {
		t.Fatalf("NewSorobanAsset: %v", err)
	}
	return a
}

func TestStorageSEP41SupplyReader_HappyPath(t *testing.T) {
	store := &fakeSEP41Store{
		totals: SEP41KindTotals{
			Mint:     big.NewInt(10_000),
			Burn:     big.NewInt(2_000),
			Clawback: big.NewInt(500),
		},
	}
	r := NewStorageSEP41SupplyReader(store, nil)
	asset := mustSorobanAsset(t, tContract)
	got, err := r.SEP41SupplyAt(context.Background(), asset, LockedSet{}, 100)
	if err != nil {
		t.Fatalf("SEP41SupplyAt: %v", err)
	}
	if got.MintTotal.Int64() != 10_000 {
		t.Errorf("MintTotal=%s want 10000", got.MintTotal)
	}
	if got.BurnTotal.Int64() != 2_000 {
		t.Errorf("BurnTotal=%s want 2000", got.BurnTotal)
	}
	if got.ClawbackTotal.Int64() != 500 {
		t.Errorf("ClawbackTotal=%s want 500", got.ClawbackTotal)
	}
	if got.AdminBalance.Sign() != 0 {
		t.Errorf("AdminBalance=%s want 0 (operator-policy via locked-set)", got.AdminBalance)
	}
	if got.LockedAccountBalances.Sign() != 0 {
		t.Errorf("LockedAccountBalances=%s want 0 (empty LockedSet)", got.LockedAccountBalances)
	}
	if got.LockedContractBalances.Sign() != 0 {
		t.Errorf("LockedContractBalances=%s want 0 (empty LockedSet)", got.LockedContractBalances)
	}
}

// TestStorageSEP41SupplyReader_MinLedgerPropagates pins that a non-zero MinSEP41ComponentLedger
// from storage reaches SEP41SupplyComponents.MinComponentLedger unchanged. Every other test here
// leaves minLedger at zero (gate-skip), so none would catch a reader dropping the field.
func TestStorageSEP41SupplyReader_MinLedgerPropagates(t *testing.T) {
	store := &fakeSEP41Store{
		totals: SEP41KindTotals{
			Mint:     big.NewInt(10_000),
			Burn:     big.NewInt(2_000),
			Clawback: big.NewInt(500),
		},
		minLedger: 98_765,
	}
	r := NewStorageSEP41SupplyReader(store, nil)
	asset := mustSorobanAsset(t, tContract)
	got, err := r.SEP41SupplyAt(context.Background(), asset, LockedSet{}, 100_000)
	if err != nil {
		t.Fatalf("SEP41SupplyAt: %v", err)
	}
	if got.MinComponentLedger != 98_765 {
		t.Errorf("MinComponentLedger=%d want 98765 (must propagate the storage anchor, not gate-skip)", got.MinComponentLedger)
	}
}

// The reader threads the store's genesis-baseline-seeded flag onto the components so the computer can route a negative
// total to `missing_baseline` rather than `compute_error`.
func TestStorageSEP41SupplyReader_GenesisSeededPropagates(t *testing.T) {
	asset := mustSorobanAsset(t, tContract)
	for _, seeded := range []bool{false, true} {
		store := &fakeSEP41Store{
			totals: SEP41KindTotals{
				Mint:     big.NewInt(1),
				Burn:     big.NewInt(0),
				Clawback: big.NewInt(0),
			},
			genesisSeeded: seeded,
		}
		r := NewStorageSEP41SupplyReader(store, nil)
		got, err := r.SEP41SupplyAt(context.Background(), asset, LockedSet{}, 100)
		if err != nil {
			t.Fatalf("SEP41SupplyAt (seeded=%v): %v", seeded, err)
		}
		if got.GenesisBaselineSeeded != seeded {
			t.Errorf("GenesisBaselineSeeded=%v want %v", got.GenesisBaselineSeeded, seeded)
		}
	}
	// A genesis-seeded query error is non-fatal — defaults to not-seeded
	// (the safe posture: a negative total routes to the benign outcome).
	store := &fakeSEP41Store{
		totals:     SEP41KindTotals{Mint: big.NewInt(1), Burn: big.NewInt(0), Clawback: big.NewInt(0)},
		genesisErr: errors.New("DB blip"),
	}
	got, err := NewStorageSEP41SupplyReader(store, nil).SEP41SupplyAt(context.Background(), asset, LockedSet{}, 100)
	if err != nil {
		t.Fatalf("SEP41SupplyAt (genesis err): %v", err)
	}
	if got.GenesisBaselineSeeded {
		t.Error("GenesisBaselineSeeded should default false on query error")
	}
}

func TestStorageSEP41SupplyReader_RejectsNonSoroban(t *testing.T) {
	r := NewStorageSEP41SupplyReader(&fakeSEP41Store{}, nil)
	_, err := r.SEP41SupplyAt(context.Background(), canonical.NativeAsset(), LockedSet{}, 1)
	if !errors.Is(err, ErrNotSoroban) {
		t.Errorf("err=%v want wrapping ErrNotSoroban", err)
	}
}

func TestStorageSEP41SupplyReader_PropagatesTotalsError(t *testing.T) {
	store := &fakeSEP41Store{totalsErr: errors.New("DB unreachable")}
	r := NewStorageSEP41SupplyReader(store, nil)
	asset := mustSorobanAsset(t, tContract)
	_, err := r.SEP41SupplyAt(context.Background(), asset, LockedSet{}, 1)
	if err == nil || !strings.Contains(err.Error(), "kind totals") {
		t.Errorf("err=%v should mention kind totals failure", err)
	}
}

// TestStorageSEP41SupplyReader_LockedSetSummed pins that operator-configured locked accounts and
// contracts contribute to the per-component sums. Balances are looked up by (holder, contract_id)
// because for SEP-41 contracts the asset_key IS the contract_id per supply.AssetKey.
func TestStorageSEP41SupplyReader_LockedSetSummed(t *testing.T) {
	store := &fakeSEP41Store{
		totals: SEP41KindTotals{
			Mint:     big.NewInt(0),
			Burn:     big.NewInt(0),
			Clawback: big.NewInt(0),
		},
		holderBalances: map[string]*big.Int{
			"G_LOCKED_1:" + tContract: big.NewInt(100),
			"G_LOCKED_2:" + tContract: big.NewInt(200),
			"C_LOCKED_1:" + tContract: big.NewInt(50),
		},
	}
	r := NewStorageSEP41SupplyReader(store, nil)
	asset := mustSorobanAsset(t, tContract)
	locked := LockedSet{
		Accounts:  []string{"G_LOCKED_1", "G_LOCKED_2"},
		Contracts: []string{"C_LOCKED_1"},
	}
	got, err := r.SEP41SupplyAt(context.Background(), asset, locked, 1)
	if err != nil {
		t.Fatalf("SEP41SupplyAt: %v", err)
	}
	if got.LockedAccountBalances.Int64() != 300 {
		t.Errorf("LockedAccountBalances=%s want 300", got.LockedAccountBalances)
	}
	if got.LockedContractBalances.Int64() != 50 {
		t.Errorf("LockedContractBalances=%s want 50", got.LockedContractBalances)
	}
}

// TestAssetBoundSEP41Computer_HappyPath — the wrapper delegates
// Compute with the baked-in asset, returning a Supply for the
// per-asset Refresher.Tick path.
func TestAssetBoundSEP41Computer_HappyPath(t *testing.T) {
	store := &fakeSEP41Store{
		totals: SEP41KindTotals{
			Mint:     big.NewInt(1_000),
			Burn:     big.NewInt(0),
			Clawback: big.NewInt(0),
		},
	}
	reader := NewStorageSEP41SupplyReader(store, nil)
	computer, err := NewSEP41Computer(Policy{}, reader)
	if err != nil {
		t.Fatalf("NewSEP41Computer: %v", err)
	}
	asset := mustSorobanAsset(t, tContract)
	bound, err := NewAssetBoundSEP41Computer(computer, asset)
	if err != nil {
		t.Fatalf("NewAssetBoundSEP41Computer: %v", err)
	}
	snap, err := bound.Compute(context.Background(), 1, time.Time{})
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	if snap.AssetKey != tContract {
		t.Errorf("AssetKey=%q want %q", snap.AssetKey, tContract)
	}
	if snap.TotalSupply.Int64() != 1_000 {
		t.Errorf("TotalSupply=%s want 1000", snap.TotalSupply)
	}
	// The reader hardcodes AdminBalance=0 and Policy{} carries no locked-set, so nothing was
	// excluded; "admin_exclusion" would falsely claim issuer holdings were netted out.
	// BasisSEP41TotalOnly is the honest reading, mirroring BasisXLMTotalOnly.
	if snap.Basis != BasisSEP41TotalOnly {
		t.Errorf("Basis=%s want %s", snap.Basis, BasisSEP41TotalOnly)
	}
	if snap.CirculatingSupply.Cmp(snap.TotalSupply) != 0 {
		t.Errorf("circulating=%s total=%s — nothing should have been excluded",
			snap.CirculatingSupply, snap.TotalSupply)
	}
}

func TestAssetBoundSEP41Computer_RejectsNonSoroban(t *testing.T) {
	reader := NewStorageSEP41SupplyReader(&fakeSEP41Store{}, nil)
	computer, _ := NewSEP41Computer(Policy{}, reader)
	_, err := NewAssetBoundSEP41Computer(computer, canonical.NativeAsset())
	if !errors.Is(err, ErrNotSoroban) {
		t.Errorf("err=%v want wrapping ErrNotSoroban", err)
	}
}

// A SAC's balance rows are stored under its classic CODE:ISSUER key and its
// G holders' balances are trustlines, so a locked set keyed on the SAC must
// read there; querying under the contract id reads 0 for every member.
func TestStorageSEP41SupplyReader_SACLockedSetReadsClassicKey(t *testing.T) {
	const (
		usdcKey         = "USDC:GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
		treasuryAccount = "GDUY7J7A33TQWOSOQGDO776GGLM3UQERL4J3SPT56F6YS4ID7MLDERI4"
		vestingContract = "CCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUZPUTHXSTZLEO7SJMI75"
	)
	sacClassicKeys, err := SACClassicKeys(map[string]string{tContract: "USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"})
	if err != nil {
		t.Fatalf("SACClassicKeys: %v", err)
	}
	store := &fakeSEP41Store{
		totals:         SEP41KindTotals{Mint: big.NewInt(10_000), Burn: big.NewInt(0), Clawback: big.NewInt(0)},
		trustlines:     map[string]*big.Int{treasuryAccount + "|" + usdcKey: big.NewInt(3_000)},
		holderBalances: map[string]*big.Int{vestingContract + ":" + usdcKey: big.NewInt(1_500)},
	}
	got, err := NewStorageSEP41SupplyReader(store, sacClassicKeys).SEP41SupplyAt(context.Background(),
		mustSorobanAsset(t, tContract), LockedSet{Accounts: []string{treasuryAccount}, Contracts: []string{vestingContract}}, 100)
	if err != nil {
		t.Fatalf("SEP41SupplyAt: %v", err)
	}
	if got.LockedAccountBalances.Cmp(big.NewInt(3_000)) != 0 {
		t.Errorf("LockedAccountBalances=%s want 3000 (the G holder's %s trustline)", got.LockedAccountBalances, usdcKey)
	}
	if got.LockedContractBalances.Cmp(big.NewInt(1_500)) != 0 {
		t.Errorf("LockedContractBalances=%s want 1500 (the C holder's SAC balance under %s)", got.LockedContractBalances, usdcKey)
	}
}

// fakeLookup satisfies AccountObservationLookup with an in-memory
// map for testing. nil row + non-nil err => "not found"
// scenario; nil err + populated row => happy path.
type fakeLookup struct {
	rows map[string]AccountObservationRow
	err  error

	// watermark is what the account OBSERVER has reached across all accounts, the freshness anchor; deliberately distinct
	// from any row's Ledger, since the tests assert the anchor tracks this and NOT the per-account minimum.
	watermark uint32
}

func (f *fakeLookup) MaxAccountObservationLedger(_ context.Context, _ uint32) (uint32, error) {
	if f.err != nil {
		return 0, f.err
	}
	return f.watermark, nil
}

func (f *fakeLookup) LatestAccountObservationAtOrBefore(_ context.Context, accountID string, _ uint32) (AccountObservationRow, error) {
	if f.err != nil {
		return AccountObservationRow{}, f.err
	}
	row, ok := f.rows[accountID]
	if !ok {
		return AccountObservationRow{}, errors.New("no observation")
	}
	return row, nil
}

func TestLCMReserveBalanceReader_HappyPath(t *testing.T) {
	r := NewLCMReserveBalanceReader(&fakeLookup{
		rows: map[string]AccountObservationRow{
			"GA1": {Balance: big.NewInt(100), Ledger: 50},
			"GA2": {Balance: big.NewInt(200), Ledger: 51},
		},
	})
	got, err := r.ReserveBalanceTotal(context.Background(), []string{"GA1", "GA2"}, 100)
	if err != nil {
		t.Fatalf("ReserveBalanceTotal: %v", err)
	}
	if got.Cmp(big.NewInt(300)) != 0 {
		t.Errorf("total=%s want 300", got)
	}
}

// When any account has no observation the reader returns ErrNoObservation; the caller's chained-fallback handler keys on
// errors.Is(err, ErrNoObservation) to drop to the static path.
func TestLCMReserveBalanceReader_MissingAccountErrorsWithSentinel(t *testing.T) {
	r := NewLCMReserveBalanceReader(&fakeLookup{
		rows: map[string]AccountObservationRow{
			"GA1": {Balance: big.NewInt(100)},
			// GA2 missing on purpose.
		},
	})
	_, err := r.ReserveBalanceTotal(context.Background(), []string{"GA1", "GA2"}, 100)
	if err == nil {
		t.Fatal("expected ErrNoObservation")
	}
	if !errors.Is(err, ErrNoObservation) {
		t.Errorf("err=%v not wrapping ErrNoObservation", err)
	}
}

// A removed account contributes 0 to the total, matching on-chain post-state (the AccountEntry no longer exists, so its
// balance is 0).
func TestLCMReserveBalanceReader_RemovalCountsAsZero(t *testing.T) {
	r := NewLCMReserveBalanceReader(&fakeLookup{
		rows: map[string]AccountObservationRow{
			"GA1": {Balance: big.NewInt(100)},
			"GA2": {IsRemoval: true},
		},
	})
	got, err := r.ReserveBalanceTotal(context.Background(), []string{"GA1", "GA2"}, 100)
	if err != nil {
		t.Fatalf("ReserveBalanceTotal: %v", err)
	}
	if got.Cmp(big.NewInt(100)) != 0 {
		t.Errorf("total=%s want 100 (GA2 removed → 0)", got)
	}
}

// TestLCMReserveBalanceReader_StoreError — a generic storage error (not "not found") still
// returns wrapped in ErrNoObservation so the caller falls back to static config; the fallback
// is the same for a transient DB error and a missing row.
func TestLCMReserveBalanceReader_StoreError(t *testing.T) {
	r := NewLCMReserveBalanceReader(&fakeLookup{
		err: errors.New("connection refused"),
	})
	_, err := r.ReserveBalanceTotal(context.Background(), []string{"GA1"}, 100)
	if !errors.Is(err, ErrNoObservation) {
		t.Errorf("err=%v want wrapping ErrNoObservation", err)
	}
}

// The anchor is the account OBSERVER's watermark, not the oldest per-account observation (49,999,500 here): SDF reserve
// accounts move only every few days to weeks, so the per-account minimum goes stale while nothing is wrong, the gate
// reads a stalled observer, and XLM supply freezes.
func TestLCMReserveBalanceReader_MinReserveAccountLedger_HappyPath(t *testing.T) {
	r := NewLCMReserveBalanceReader(&fakeLookup{
		rows: map[string]AccountObservationRow{
			"GA1": {Balance: big.NewInt(100), Ledger: 50_000_000},
			"GA2": {Balance: big.NewInt(200), Ledger: 49_999_500},
			"GA3": {Balance: big.NewInt(300), Ledger: 50_000_100},
		},
		watermark: 50_000_900,
	})
	got, err := r.MinReserveAccountLedger(context.Background(), []string{"GA1", "GA2", "GA3"}, 50_001_000)
	if err != nil {
		t.Fatalf("MinReserveAccountLedger: %v", err)
	}
	if got == 49_999_500 {
		t.Fatal("anchor regressed to the per-account MINIMUM — a quiet reserve " +
			"account is not a stalled observer, and this freezes XLM supply")
	}
	if got != 50_000_900 {
		t.Errorf("anchor=%d want 50_000_900 (observer watermark)", got)
	}
}

// A removed account still COUNTS as observed (an AccountEntry delete is a real observation), so it does not force the
// gate-permissive bypass, but its ledger does not set the anchor; the observer watermark does, so a long-ago removal
// cannot drag XLM freshness backwards.
func TestLCMReserveBalanceReader_MinReserveAccountLedger_RemovalCounts(t *testing.T) {
	r := NewLCMReserveBalanceReader(&fakeLookup{
		rows: map[string]AccountObservationRow{
			"GA1": {Balance: big.NewInt(100), Ledger: 50_000_000},
			"GA2": {IsRemoval: true, Ledger: 49_990_000},
		},
		watermark: 50_000_900,
	})
	got, err := r.MinReserveAccountLedger(context.Background(), []string{"GA1", "GA2"}, 50_001_000)
	if err != nil {
		t.Fatalf("MinReserveAccountLedger: %v", err)
	}
	if got != 50_000_900 {
		t.Errorf("anchor=%d want 50_000_900 (watermark); a removed account must "+
			"not pin freshness to its removal ledger forever", got)
	}
}

// TestLCMReserveBalanceReader_MinReserveAccountLedger_EmptyAccounts —
// no accounts means no freshness signal to compute; the gate-permissive
// 0 sentinel matches the [ConfigReserveBalanceReader] posture.
func TestLCMReserveBalanceReader_MinReserveAccountLedger_EmptyAccounts(t *testing.T) {
	r := NewLCMReserveBalanceReader(&fakeLookup{})
	got, err := r.MinReserveAccountLedger(context.Background(), nil, 100)
	if err != nil {
		t.Fatalf("MinReserveAccountLedger: %v", err)
	}
	if got != 0 {
		t.Errorf("min=%d want 0 (no accounts → no signal)", got)
	}
}

// TestLCMReserveBalanceReader_MinReserveAccountLedger_MissingAccount — when a supplied account
// has no observation, the reader returns the sentinel-wrapped error so the chain reader drops
// to the gate-permissive bypass instead of applying the other accounts' freshness to it.
func TestLCMReserveBalanceReader_MinReserveAccountLedger_MissingAccount(t *testing.T) {
	r := NewLCMReserveBalanceReader(&fakeLookup{
		rows: map[string]AccountObservationRow{
			"GA1": {Balance: big.NewInt(100), Ledger: 50_000_000},
			// GA2 missing on purpose.
		},
	})
	_, err := r.MinReserveAccountLedger(context.Background(), []string{"GA1", "GA2"}, 50_001_000)
	if !errors.Is(err, ErrNoObservation) {
		t.Errorf("err=%v want wrapping ErrNoObservation", err)
	}
}
