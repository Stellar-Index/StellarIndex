package supply_test

import (
	"context"
	"errors"
	"math/big"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/supply"
)

// validContractID is a real, valid C-strkey we use across tests; the
// canonical package validates strkey CRC on construction so we
// can't pass placeholder values. (Reused from key_test happy-path
// fixture — the SAC contract address for native XLM on pubnet.)
const validContractID = "CAS3J7GYLGXMF6TDJBBYYSE3HQ6BBSMLNUQ34T6TZMYMW2EVH34XOWMA"

// stubSEP41Reader is a minimal supply.SEP41SupplyReader for tests.
type stubSEP41Reader struct {
	comps supply.SEP41SupplyComponents
	err   error
	calls int
	last  struct {
		asset  canonical.Asset
		locked supply.LockedSet
		ledger uint32
	}
}

func (s *stubSEP41Reader) SEP41SupplyAt(_ context.Context, asset canonical.Asset, locked supply.LockedSet, ledger uint32) (supply.SEP41SupplyComponents, error) {
	s.calls++
	s.last.asset = asset
	s.last.locked = locked
	s.last.ledger = ledger
	if s.err != nil {
		return supply.SEP41SupplyComponents{}, s.err
	}
	return s.comps, nil
}

func mustSoroban(t *testing.T, contractID string) canonical.Asset {
	t.Helper()
	a, err := canonical.NewSorobanAsset(contractID)
	if err != nil {
		t.Fatalf("NewSorobanAsset: %v", err)
	}
	return a
}

// TestNewSEP41Computer_RejectsNilReader — same loud-misconfig stance
// as the other algorithms.
func TestNewSEP41Computer_RejectsNilReader(t *testing.T) {
	_, err := supply.NewSEP41Computer(supply.Policy{}, nil)
	if !errors.Is(err, supply.ErrNilReader) {
		t.Errorf("err = %v, want ErrNilReader", err)
	}
}

// TestSEP41_Compute_HappyPath — total = mint − burn − clawback;
// circulating = total − admin − locked sums; default basis is
// AdminExclusion when no overrides apply.
func TestSEP41_Compute_HappyPath(t *testing.T) {
	reader := &stubSEP41Reader{
		comps: supply.SEP41SupplyComponents{
			MintTotal:              bigInt(10_000_000_000), // 1000 tokens
			BurnTotal:              bigInt(500_000_000),    //   50 burned
			ClawbackTotal:          bigInt(100_000_000),    //   10 clawed back
			AdminBalance:           bigInt(40_000_000),     //    4 sitting on admin
			LockedAccountBalances:  bigInt(0),
			LockedContractBalances: bigInt(0),
		},
	}
	c, err := supply.NewSEP41Computer(supply.Policy{}, reader)
	if err != nil {
		t.Fatalf("constructor: %v", err)
	}

	asset := mustSoroban(t, validContractID)
	got, err := c.Compute(context.Background(), asset, 50_000_000, time.Date(2026, 4, 28, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}

	wantTotal := bigInt(9_400_000_000) // 10000 − 500 − 100 (millions of stroops)
	wantCirculating := bigInt(9_360_000_000)
	if got.TotalSupply.Cmp(wantTotal) != 0 {
		t.Errorf("TotalSupply = %s, want %s", got.TotalSupply, wantTotal)
	}
	if got.CirculatingSupply.Cmp(wantCirculating) != 0 {
		t.Errorf("CirculatingSupply = %s, want %s", got.CirculatingSupply, wantCirculating)
	}
	if got.MaxSupply != nil {
		t.Errorf("MaxSupply = %s, want nil", got.MaxSupply)
	}
	if got.Basis != supply.BasisAdminExclusion {
		t.Errorf("Basis = %q, want %q", got.Basis, supply.BasisAdminExclusion)
	}
	if got.AssetKey != validContractID {
		t.Errorf("AssetKey = %q, want %q", got.AssetKey, validContractID)
	}
}

// TestSEP41_Compute_RejectsNonSoroban — feeding a classic or native
// asset is a routing bug.
func TestSEP41_Compute_RejectsNonSoroban(t *testing.T) {
	c, _ := supply.NewSEP41Computer(supply.Policy{}, &stubSEP41Reader{})
	if _, err := c.Compute(context.Background(), canonical.NativeAsset(), 1, time.Now()); !errors.Is(err, supply.ErrNotSoroban) {
		t.Errorf("err = %v, want ErrNotSoroban", err)
	}
}

// TestSEP41_Compute_NegativeTotalRejected — burn > mint can never
// be a real on-chain state for a SEP-41 token whose LIFETIME supply
// is captured (genesis baseline seeded); an indexer that produces
// this is mis-summing somewhere. Refuse to publish, and surface the
// paging sentinel.
func TestSEP41_Compute_NegativeTotalRejected(t *testing.T) {
	reader := &stubSEP41Reader{
		comps: supply.SEP41SupplyComponents{
			MintTotal:              bigInt(100),
			BurnTotal:              bigInt(150), // burned more than minted — impossible
			ClawbackTotal:          bigInt(0),
			AdminBalance:           bigInt(0),
			LockedAccountBalances:  bigInt(0),
			LockedContractBalances: bigInt(0),
			GenesisBaselineSeeded:  true, // lifetime totals — a negative here is genuine corruption
		},
	}
	c, _ := supply.NewSEP41Computer(supply.Policy{}, reader)
	asset := mustSoroban(t, validContractID)
	_, err := c.Compute(context.Background(), asset, 1, time.Now())
	if !errors.Is(err, supply.ErrNegativeTotalSupply) {
		t.Errorf("err = %v, want ErrNegativeTotalSupply", err)
	}
	// A genuine inconsistency must NOT masquerade as the benign
	// missing-baseline case.
	if errors.Is(err, supply.ErrNegativeTotalMissingBaseline) {
		t.Errorf("seeded-but-negative must not map to ErrNegativeTotalMissingBaseline: %v", err)
	}
}

// TestSEP41_Compute_NegativeTotalMissingBaseline — a SAC-wrapper whose
// pre-Soroban opening balance has NOT been seeded legitimately reads
// Σburn > Σmint over the Soroban-era-only window (its mints predate
// Soroban). This is a range-scoped-baseline-missing condition, not
// corruption — surface the benign sentinel so the refresher reports
// `missing_baseline` (needs a seed) rather than paging (incident
// 2026-07-06 / migration 0088).
func TestSEP41_Compute_NegativeTotalMissingBaseline(t *testing.T) {
	reader := &stubSEP41Reader{
		comps: supply.SEP41SupplyComponents{
			MintTotal:              bigInt(2),
			BurnTotal:              bigInt(2_180_000_000_000), // VELO-shaped: huge Soroban-era unwrap/burn
			ClawbackTotal:          bigInt(0),
			AdminBalance:           bigInt(0),
			LockedAccountBalances:  bigInt(0),
			LockedContractBalances: bigInt(0),
			GenesisBaselineSeeded:  false, // opening balance not seeded yet
		},
	}
	c, _ := supply.NewSEP41Computer(supply.Policy{}, reader)
	asset := mustSoroban(t, validContractID)
	_, err := c.Compute(context.Background(), asset, 1, time.Now())
	if !errors.Is(err, supply.ErrNegativeTotalMissingBaseline) {
		t.Errorf("err = %v, want ErrNegativeTotalMissingBaseline", err)
	}
	// It must be distinguishable from the genuine-corruption sentinel
	// so the refresher routes it to the non-paging outcome.
	if errors.Is(err, supply.ErrNegativeTotalSupply) {
		t.Errorf("missing-baseline case must not also match ErrNegativeTotalSupply: %v", err)
	}
}

// TestSEP41_Compute_GenesisBaselineMakesTotalPositive — once the pre-Soroban
// baseline IS folded into the totals (the reader returns lifetime mint/burn/
// clawback), the same SAC-wrapper computes a POSITIVE total and the guard does
// not trip. This is the fixed steady state for the 9 watched SAC-wrappers.
func TestSEP41_Compute_GenesisBaselineMakesTotalPositive(t *testing.T) {
	reader := &stubSEP41Reader{
		comps: supply.SEP41SupplyComponents{
			// Lifetime totals: pre-Soroban mint dwarfs the Soroban-era burn.
			MintTotal:              bigInt(2_400_000_000_000),
			BurnTotal:              bigInt(2_180_000_000_000),
			ClawbackTotal:          bigInt(0),
			AdminBalance:           bigInt(0),
			LockedAccountBalances:  bigInt(0),
			LockedContractBalances: bigInt(0),
			GenesisBaselineSeeded:  true,
		},
	}
	c, _ := supply.NewSEP41Computer(supply.Policy{}, reader)
	asset := mustSoroban(t, validContractID)
	got, err := c.Compute(context.Background(), asset, 1, time.Now())
	if err != nil {
		t.Fatalf("seeded lifetime total should compute cleanly; got %v", err)
	}
	if got.TotalSupply.Sign() <= 0 {
		t.Errorf("TotalSupply = %s, want > 0 once genesis baseline is folded in", got.TotalSupply)
	}
	if got.TotalSupply.Cmp(bigInt(220_000_000_000)) != 0 {
		t.Errorf("TotalSupply = %s, want 220000000000 (2.4e12 − 2.18e12)", got.TotalSupply)
	}
}

// TestSEP41_Compute_GenesisBaselineGuardMatrix locks in the migration-0088
// money-math guard as a single table: how the sign of the SEP-41 total and the
// GenesisBaselineSeeded flag route to a published supply, the PAGING sentinel,
// or the BENIGN one. The reader folds the pre-Soroban baseline (CH lake, ledger
// < boundary) into the Soroban-era totals (PG, ledger >= boundary) BEFORE
// Compute runs, so the computer's guard operates purely on the already-folded
// components — exactly what this exercises. A refactor that drops the
// seeded-vs-unseeded distinction, lets a negative total through, or shifts a
// positive total when a (zero) baseline is present regresses one of these rows.
func TestSEP41_Compute_GenesisBaselineGuardMatrix(t *testing.T) {
	asset := mustSoroban(t, validContractID)
	// Both negative-total sentinels; a case's wantErr is one of these, and the
	// non-matched one MUST NOT match — the refresher routes paging vs benign
	// off exactly that distinction.
	negSentinels := []error{supply.ErrNegativeTotalSupply, supply.ErrNegativeTotalMissingBaseline}

	cases := []struct {
		name      string
		mint      *big.Int // folded LIFETIME mint the reader returns
		burn      *big.Int // folded LIFETIME burn
		seeded    bool
		wantErr   error    // nil => expect a clean non-negative total
		wantTotal *big.Int // checked only when wantErr == nil
	}{
		{
			// (a) seeded, and the pre-Soroban baseline dominates the Soroban-era
			// burn → positive lifetime total, guard not tripped. The fixed
			// steady state for the 9 watched SAC-wrappers post-seed.
			name:      "a_seeded_genesis_dominates_negative_soroban",
			mint:      bigInt(2_400_000_000_002), // 2 Soroban-era + 2.4e12 pre-Soroban
			burn:      bigInt(2_180_000_000_000),
			seeded:    true,
			wantTotal: bigInt(220_000_000_002),
		},
		{
			// (b) seeded but STILL negative (baseline too small, or a genuine
			// upstream mis-sum) → PAGING ErrNegativeTotalSupply. Once lifetime
			// totals are captured, burn > mint is physically impossible.
			name:    "b_seeded_still_negative_pages",
			mint:    bigInt(100),
			burn:    bigInt(150),
			seeded:  true,
			wantErr: supply.ErrNegativeTotalSupply,
		},
		{
			// (c) NOT seeded + negative → BENIGN ErrNegativeTotalMissingBaseline
			// (needs a seed; does not page). A SAC-wrapper's mints predate
			// Soroban, so the Soroban-era-only window legitimately reads
			// burn > mint until the opening balance is seeded.
			name:    "c_unseeded_negative_missing_baseline",
			mint:    bigInt(2),
			burn:    bigInt(2_180_000_000_000),
			seeded:  false,
			wantErr: supply.ErrNegativeTotalMissingBaseline,
		},
		{
			// (d) zero baseline / Soroban-only token → total is EXACTLY the
			// Soroban-era total, unchanged. A seeded ZERO genesis (the seed for
			// a token with no pre-genesis flows) never touches the guard and
			// cannot double-count.
			name:      "d_zero_baseline_soroban_only_unchanged",
			mint:      bigInt(1_000_000_000),
			burn:      bigInt(0),
			seeded:    true,
			wantTotal: bigInt(1_000_000_000),
		},
		{
			// (d') the same token NOT seeded — identical published total.
			// Proves the seeded flag alone never shifts a positive number.
			name:      "d_prime_unseeded_positive_unchanged",
			mint:      bigInt(1_000_000_000),
			burn:      bigInt(0),
			seeded:    false,
			wantTotal: bigInt(1_000_000_000),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reader := &stubSEP41Reader{
				comps: supply.SEP41SupplyComponents{
					MintTotal:              tc.mint,
					BurnTotal:              tc.burn,
					ClawbackTotal:          bigInt(0),
					AdminBalance:           bigInt(0),
					LockedAccountBalances:  bigInt(0),
					LockedContractBalances: bigInt(0),
					GenesisBaselineSeeded:  tc.seeded,
				},
			}
			c, _ := supply.NewSEP41Computer(supply.Policy{}, reader)
			// Ledger is above the boundary (the aggregator always reads at tip);
			// the stub ignores it — the fold/gate is the real reader's job,
			// covered by the timescale integration test.
			got, err := c.Compute(context.Background(), asset, 60_000_000, time.Now())

			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("err = %v, want %v", err, tc.wantErr)
				}
				// Sentinels must stay mutually exclusive: only the wanted one
				// may match, so the refresher's paging-vs-benign routing holds.
				for _, s := range negSentinels {
					if got, want := errors.Is(err, s), errors.Is(tc.wantErr, s); got != want {
						t.Errorf("errors.Is(err, %v) = %v, want %v", s, got, want)
					}
				}
				return
			}

			if err != nil {
				t.Fatalf("Compute: unexpected error %v", err)
			}
			if got.TotalSupply.Cmp(tc.wantTotal) != 0 {
				t.Errorf("TotalSupply = %s, want %s", got.TotalSupply, tc.wantTotal)
			}
			if got.TotalSupply.Sign() < 0 {
				t.Errorf("TotalSupply = %s, must never publish negative", got.TotalSupply)
			}
		})
	}
}

// TestSEP41_Compute_LockedSetForwarded — operator-extended locked-set
// is passed to the reader so it can compute the LockedAccount /
// LockedContract sums in a single query. Basis upgrades to Override.
func TestSEP41_Compute_LockedSetForwarded(t *testing.T) {
	reader := &stubSEP41Reader{
		comps: supply.SEP41SupplyComponents{
			MintTotal:              bigInt(1_000),
			BurnTotal:              bigInt(0),
			ClawbackTotal:          bigInt(0),
			AdminBalance:           bigInt(0),
			LockedAccountBalances:  bigInt(100),
			LockedContractBalances: bigInt(50),
		},
	}
	policy := supply.Policy{
		PerAsset: map[string]supply.LockedSet{
			validContractID: {
				Accounts:  []string{"GTREASURY..."},
				Contracts: []string{"CVESTING..."},
			},
		},
	}
	c, _ := supply.NewSEP41Computer(policy, reader)

	asset := mustSoroban(t, validContractID)
	got, err := c.Compute(context.Background(), asset, 1, time.Now())
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	if reader.last.locked.IsEmpty() {
		t.Error("reader received empty locked-set; expected forwarded operator override")
	}
	wantCirculating := bigInt(1_000 - 100 - 50)
	if got.CirculatingSupply.Cmp(wantCirculating) != 0 {
		t.Errorf("CirculatingSupply = %s, want %s", got.CirculatingSupply, wantCirculating)
	}
	if got.Basis != supply.BasisOverride {
		t.Errorf("Basis = %q, want %q", got.Basis, supply.BasisOverride)
	}
}

// TestSEP41_Compute_MaxSupplyOverride — operator-supplied max
// becomes the published value; basis is Override.
func TestSEP41_Compute_MaxSupplyOverride(t *testing.T) {
	reader := &stubSEP41Reader{
		comps: supply.SEP41SupplyComponents{
			MintTotal: bigInt(0), BurnTotal: bigInt(0), ClawbackTotal: bigInt(0),
			AdminBalance: bigInt(0), LockedAccountBalances: bigInt(0), LockedContractBalances: bigInt(0),
		},
	}
	policy := supply.Policy{
		MaxSupplyOverrides: map[string]string{validContractID: "1000000000"},
	}
	c, _ := supply.NewSEP41Computer(policy, reader)
	asset := mustSoroban(t, validContractID)
	got, err := c.Compute(context.Background(), asset, 1, time.Now())
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	if got.MaxSupply == nil || got.MaxSupply.String() != "1000000000" {
		t.Errorf("MaxSupply = %v, want 1000000000", got.MaxSupply)
	}
	if got.Basis != supply.BasisOverride {
		t.Errorf("Basis = %q, want %q", got.Basis, supply.BasisOverride)
	}
}

// TestSEP41_Compute_PropagatesReaderError — reader failure must surface.
func TestSEP41_Compute_PropagatesReaderError(t *testing.T) {
	reader := &stubSEP41Reader{err: errors.New("postgres unavailable")}
	c, _ := supply.NewSEP41Computer(supply.Policy{}, reader)
	asset := mustSoroban(t, validContractID)
	if _, err := c.Compute(context.Background(), asset, 1, time.Now()); err == nil {
		t.Error("expected error from failing reader; got nil")
	}
}

// TestSEP41_Compute_RejectsNilComponents — defensive guard against a
// reader returning nil pointers.
func TestSEP41_Compute_RejectsNilComponents(t *testing.T) {
	reader := &stubSEP41Reader{
		comps: supply.SEP41SupplyComponents{
			MintTotal: nil, // sentinel
			BurnTotal: bigInt(0), ClawbackTotal: bigInt(0),
			AdminBalance: bigInt(0), LockedAccountBalances: bigInt(0), LockedContractBalances: bigInt(0),
		},
	}
	c, _ := supply.NewSEP41Computer(supply.Policy{}, reader)
	asset := mustSoroban(t, validContractID)
	if _, err := c.Compute(context.Background(), asset, 1, time.Now()); err == nil {
		t.Error("expected error for nil component; got nil")
	}
}

// TestSEP41_Compute_ZeroSupplyTokenIsValid — a fully-burned token
// (mint == burn) reports total=0 / circulating=0, NOT an error.
// Distinct from the negative-total case.
func TestSEP41_Compute_ZeroSupplyTokenIsValid(t *testing.T) {
	reader := &stubSEP41Reader{
		comps: supply.SEP41SupplyComponents{
			MintTotal:              bigInt(1_000),
			BurnTotal:              bigInt(1_000),
			ClawbackTotal:          bigInt(0),
			AdminBalance:           bigInt(0),
			LockedAccountBalances:  bigInt(0),
			LockedContractBalances: bigInt(0),
		},
	}
	c, _ := supply.NewSEP41Computer(supply.Policy{}, reader)
	asset := mustSoroban(t, validContractID)
	got, err := c.Compute(context.Background(), asset, 1, time.Now())
	if err != nil {
		t.Fatalf("fully-burned token should compute cleanly; got %v", err)
	}
	if got.TotalSupply.Sign() != 0 {
		t.Errorf("TotalSupply = %s, want 0", got.TotalSupply)
	}
	if got.CirculatingSupply.Sign() != 0 {
		t.Errorf("CirculatingSupply = %s, want 0", got.CirculatingSupply)
	}
}
