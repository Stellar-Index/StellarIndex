//go:build integration

package integration_test

import (
	"context"
	"math/big"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

const (
	aquariusRwPool   = "CAB6MICC2WKRT372U3FRPKGGVB5R3FDJSMWSLPF2UJNJPYMBZ76RQVYE"
	aquariusRwRouter = "CBQDHNBFBZYE4MKPWBSJOPIYLW4SFSXAXUTSXJN76GNKYVYPCKWC6QUK"
	aquariusRwUserA  = "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
	aquariusRwUserB  = "GBRPYHIL2CI3FNQ4BXLFMNDLFJUNPU2HY3ZMFSHONUCEOASW7QC7OX2H"
)

// TestAquariusRewardsLifetimeByKind exercises the LIFETIME per-kind
// rewards-gauge reader (AquariusRewardsLifetimeByKind, the v0.12 gap
// closed by migration 0099): every one of the twelve kinds comes back in
// migration-0099 census order, kinds with no captured rows still appear
// with Events=0/Amount=0 (never a missing row), and i128 amounts survive
// the NUMERIC round trip.
func TestAquariusRewardsLifetimeByKind(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	// Empty table: all twelve kinds still returned, all zero.
	byKind, err := store.AquariusRewardsLifetimeByKind(ctx)
	if err != nil {
		t.Fatalf("AquariusRewardsLifetimeByKind (empty): %v", err)
	}
	if len(byKind) != 12 {
		t.Fatalf("AquariusRewardsLifetimeByKind (empty) = %d kinds, want 12", len(byKind))
	}
	for _, k := range byKind {
		if k.Events != 0 || k.Amount.Sign() != 0 {
			t.Errorf("kind %q on empty table = %+v, want zero", k.Kind, k)
		}
	}

	t0 := time.Now().UTC().Add(-48 * time.Hour)
	claimHuge, _ := new(big.Int).SetString("55555555555555555555", 10) // > 2^63

	rows := []timescale.AquariusRewardsEvent{
		{
			ContractID: aquariusRwPool, Ledger: 62_000_000, LedgerCloseTime: t0, TxHash: pad64("a", 0), OpIndex: 0, EventIndex: 0,
			Kind: timescale.AquariusRewardsClaimReward, UserAddress: aquariusRwUserA, Amount: amtPtr(canonical.NewAmount(claimHuge)),
		},
		{
			ContractID: aquariusRwPool, Ledger: 62_000_001, LedgerCloseTime: t0.Add(time.Minute), TxHash: pad64("a", 1), OpIndex: 0, EventIndex: 0,
			Kind: timescale.AquariusRewardsClaimReward, UserAddress: aquariusRwUserB, Amount: amtPtr(canonical.NewAmount(big.NewInt(2_000))),
		},
		{
			ContractID: aquariusRwPool, Ledger: 62_000_002, LedgerCloseTime: t0.Add(2 * time.Minute), TxHash: pad64("a", 2), OpIndex: 0, EventIndex: 0,
			Kind: timescale.AquariusRewardsPoolState,
		},
		{
			ContractID: aquariusRwRouter, Ledger: 62_000_003, LedgerCloseTime: t0.Add(3 * time.Minute), TxHash: pad64("a", 3), OpIndex: 0, EventIndex: 0,
			Kind: timescale.AquariusRewardsConfigRewards,
		},
	}
	for _, e := range rows {
		if err := store.InsertAquariusRewardsEvent(ctx, e); err != nil {
			t.Fatalf("InsertAquariusRewardsEvent %s: %v", e.Kind, err)
		}
		// Idempotent re-insert.
		if err := store.InsertAquariusRewardsEvent(ctx, e); err != nil {
			t.Fatalf("InsertAquariusRewardsEvent %s (dup): %v", e.Kind, err)
		}
	}

	byKind, err = store.AquariusRewardsLifetimeByKind(ctx)
	if err != nil {
		t.Fatalf("AquariusRewardsLifetimeByKind: %v", err)
	}
	if len(byKind) != 12 {
		t.Fatalf("AquariusRewardsLifetimeByKind = %d kinds, want 12", len(byKind))
	}
	// Census order: pool_state is first, claim_reward second.
	if byKind[0].Kind != timescale.AquariusRewardsPoolState || byKind[0].Events != 1 {
		t.Errorf("byKind[0] = %+v, want {pool_state 1}", byKind[0])
	}
	if byKind[1].Kind != timescale.AquariusRewardsClaimReward || byKind[1].Events != 2 {
		t.Errorf("byKind[1] = %+v, want {claim_reward 2}", byKind[1])
	}
	wantClaimSum := new(big.Int).Add(claimHuge, big.NewInt(2_000))
	if byKind[1].Amount.BigInt().Cmp(wantClaimSum) != 0 {
		t.Errorf("byKind[1].Amount = %s, want %s — i128/NUMERIC lost precision", byKind[1].Amount, wantClaimSum)
	}
	// config_rewards (router-side) is last in census order.
	last := byKind[len(byKind)-1]
	if last.Kind != timescale.AquariusRewardsConfigRewards || last.Events != 1 {
		t.Errorf("byKind[last] = %+v, want {config_rewards 1}", last)
	}
	// A kind nothing was inserted for stays at zero (never dropped from the set).
	if byKind[5].Kind != timescale.AquariusRewardsClaimFees || byKind[5].Events != 0 {
		t.Errorf("byKind[5] (claim_fees) = %+v, want zero events", byKind[5])
	}
}

// TestAquariusRewardsClaimWindow exercises the windowed claim_reward
// drill-down (AquariusRewardsClaimWindow): empty-safe on no claims, a
// claim outside the window is excluded (sargable ledger_close_time bound),
// distinct claimants dedupes a repeat claimant, and i128 amounts survive.
func TestAquariusRewardsClaimWindow(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	if w, err := store.AquariusRewardsClaimWindow(ctx, 30); err != nil {
		t.Fatalf("AquariusRewardsClaimWindow (empty): %v", err)
	} else if w != nil {
		t.Fatalf("AquariusRewardsClaimWindow (empty) = %+v, want nil", w)
	}

	inWindow := time.Now().UTC().Add(-5 * 24 * time.Hour)
	outOfWindow := time.Now().UTC().Add(-90 * 24 * time.Hour)

	claims := []timescale.AquariusRewardsEvent{
		// Two claims inside the 30d window, same claimant (dedupes to 1 distinct).
		{
			ContractID: aquariusRwPool, Ledger: 63_000_000, LedgerCloseTime: inWindow, TxHash: pad64("c", 0), OpIndex: 0, EventIndex: 0,
			Kind: timescale.AquariusRewardsClaimReward, UserAddress: aquariusRwUserA, Amount: amtPtr(canonical.NewAmount(big.NewInt(1_000))),
		},
		{
			ContractID: aquariusRwPool, Ledger: 63_000_001, LedgerCloseTime: inWindow.Add(time.Hour), TxHash: pad64("c", 1), OpIndex: 0, EventIndex: 0,
			Kind: timescale.AquariusRewardsClaimReward, UserAddress: aquariusRwUserA, Amount: amtPtr(canonical.NewAmount(big.NewInt(500))),
		},
		// A different claimant, also inside the window.
		{
			ContractID: aquariusRwPool, Ledger: 63_000_002, LedgerCloseTime: inWindow.Add(2 * time.Hour), TxHash: pad64("c", 2), OpIndex: 0, EventIndex: 0,
			Kind: timescale.AquariusRewardsClaimReward, UserAddress: aquariusRwUserB, Amount: amtPtr(canonical.NewAmount(big.NewInt(250))),
		},
		// Outside the 30d window — must be excluded.
		{
			ContractID: aquariusRwPool, Ledger: 60_000_000, LedgerCloseTime: outOfWindow, TxHash: pad64("c", 3), OpIndex: 0, EventIndex: 0,
			Kind: timescale.AquariusRewardsClaimReward, UserAddress: aquariusRwUserA, Amount: amtPtr(canonical.NewAmount(big.NewInt(999_999))),
		},
	}
	for _, e := range claims {
		if err := store.InsertAquariusRewardsEvent(ctx, e); err != nil {
			t.Fatalf("InsertAquariusRewardsEvent: %v", err)
		}
	}

	w, err := store.AquariusRewardsClaimWindow(ctx, 30)
	if err != nil {
		t.Fatalf("AquariusRewardsClaimWindow: %v", err)
	}
	if w == nil {
		t.Fatal("AquariusRewardsClaimWindow = nil, want a summary")
	}
	if w.Events != 3 {
		t.Errorf("Events = %d, want 3 (the out-of-window claim excluded)", w.Events)
	}
	if want := big.NewInt(1_000 + 500 + 250); w.Amount.BigInt().Cmp(want) != 0 {
		t.Errorf("Amount = %s, want %s", w.Amount, want)
	}
	if w.DistinctClaimants != 2 {
		t.Errorf("DistinctClaimants = %d, want 2", w.DistinctClaimants)
	}
}

// TestLatestAquariusAdminEvents exercises the governance-events reader
// (LatestAquariusAdminEvents): empty-safe, newest-first ordering
// (unwindowed, per the reader's doc — unlike the windowed bespoke augments),
// and kind/admin/target/ledger fields round-trip.
func TestLatestAquariusAdminEvents(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	if events, err := store.LatestAquariusAdminEvents(ctx, 25); err != nil {
		t.Fatalf("LatestAquariusAdminEvents (empty): %v", err)
	} else if len(events) != 0 {
		t.Fatalf("LatestAquariusAdminEvents (empty) = %+v, want none", events)
	}
	if total, err := store.AquariusAdminLifetimeTotal(ctx); err != nil {
		t.Fatalf("AquariusAdminLifetimeTotal (empty): %v", err)
	} else if total != 0 {
		t.Fatalf("AquariusAdminLifetimeTotal (empty) = %d, want 0", total)
	}

	base := time.Now().UTC().Add(-72 * time.Hour)
	admin := "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
	wasmHash := "d0d1b1e0f56d4c3a9b2e1f7c8a6d5e4b3c2a1908f7e6d5c4b3a291807f6e5d4c"

	events := []timescale.AquariusAdminEvent{
		{
			ContractID: aquariusRwRouter, Ledger: 61_000_000, LedgerCloseTime: base, TxHash: pad64("g", 0), OpIndex: 0, EventIndex: 0,
			Kind: timescale.AquariusAdminCommitUpgrade, Admin: admin, Target: wasmHash,
		},
		{
			ContractID: aquariusRwRouter, Ledger: 61_000_500, LedgerCloseTime: base.Add(time.Hour), TxHash: pad64("g", 1), OpIndex: 0, EventIndex: 0,
			Kind: timescale.AquariusAdminApplyUpgrade, Admin: admin, Target: wasmHash,
		},
		{
			ContractID: aquariusRwPool, Ledger: 61_001_000, LedgerCloseTime: base.Add(2 * time.Hour), TxHash: pad64("g", 2), OpIndex: 0, EventIndex: 0,
			Kind: timescale.AquariusAdminPoolGaugeSwitchToken, Target: aquariusRwUserA,
		},
	}
	for _, e := range events {
		if err := store.InsertAquariusAdminEvent(ctx, e); err != nil {
			t.Fatalf("InsertAquariusAdminEvent %s: %v", e.Kind, err)
		}
	}

	got, err := store.LatestAquariusAdminEvents(ctx, 25)
	if err != nil {
		t.Fatalf("LatestAquariusAdminEvents: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("LatestAquariusAdminEvents = %d rows, want 3", len(got))
	}
	// Newest first: pool_gauge_switch_token (base+2h) leads.
	if got[0].Kind != timescale.AquariusAdminPoolGaugeSwitchToken || got[0].ContractID != aquariusRwPool {
		t.Errorf("got[0] = %+v, want the pool_gauge_switch_token row first", got[0])
	}
	if got[0].Admin != "" {
		t.Errorf("got[0].Admin = %q, want empty (kind carries none)", got[0].Admin)
	}
	if got[0].Target != aquariusRwUserA {
		t.Errorf("got[0].Target = %q, want %q", got[0].Target, aquariusRwUserA)
	}
	// Oldest last: commit_upgrade.
	last := got[len(got)-1]
	if last.Kind != timescale.AquariusAdminCommitUpgrade || last.Ledger != 61_000_000 {
		t.Errorf("got[last] = %+v, want the commit_upgrade row last", last)
	}
	if last.Admin != admin || last.Target != wasmHash {
		t.Errorf("got[last] admin/target = %q/%q, want %q/%q", last.Admin, last.Target, admin, wasmHash)
	}

	total, err := store.AquariusAdminLifetimeTotal(ctx)
	if err != nil {
		t.Fatalf("AquariusAdminLifetimeTotal: %v", err)
	}
	if total != 3 {
		t.Errorf("AquariusAdminLifetimeTotal = %d, want 3", total)
	}
}

// TestBespokeAquariusRewardsSurfaced proves the v0.12 "backfilled but
// served nowhere" gap (7.3M+ rows across aquarius_rewards_events +
// aquarius_admin) is closed: the Aquarius bespoke DEX block gains the
// rewards + governance KPIs/tables/series once either table carries data,
// stays empty-safe (nil block) when nothing has been captured, and the new
// KPIs sit ALONGSIDE (not replacing) the existing reserve-depth block from
// aquariusReserveBlocks. No frontend change is needed — BespokeSection
// renders KPIs/series/tables generically.
func TestBespokeAquariusRewardsSurfaced(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	// Empty-safe: no trades, no reserves, no rewards/admin → nil block.
	blk, err := store.BuildProtocolBespoke(ctx, "aquarius", "amm", 90)
	if err != nil {
		t.Fatalf("BuildProtocolBespoke (empty): %v", err)
	}
	if blk != nil {
		t.Fatalf("BuildProtocolBespoke (empty) = %+v, want nil", blk)
	}

	// Seed a reserve snapshot (the pre-existing depth block) + rewards +
	// governance activity, so we can prove they coexist.
	if err := store.InsertAquariusReserves(ctx, timescale.AquariusReservesEvent{
		ContractID:      aquariusRwPool,
		Ledger:          63_500_000,
		LedgerCloseTime: time.Now().UTC().Add(-24 * time.Hour),
		TxHash:          pad64("r", 0),
		OpIndex:         0,
		EventIndex:      0,
		Reserves: []canonical.Amount{
			canonical.NewAmount(big.NewInt(123_456_789)),
			canonical.NewAmount(big.NewInt(987_654_321)),
		},
	}); err != nil {
		t.Fatalf("InsertAquariusReserves: %v", err)
	}

	claimT := time.Now().UTC().Add(-5 * 24 * time.Hour)
	if err := store.InsertAquariusRewardsEvent(ctx, timescale.AquariusRewardsEvent{
		ContractID: aquariusRwPool, Ledger: 63_500_100, LedgerCloseTime: claimT, TxHash: pad64("s", 0), OpIndex: 0, EventIndex: 0,
		Kind: timescale.AquariusRewardsClaimReward, UserAddress: aquariusRwUserA, Amount: amtPtr(canonical.NewAmount(big.NewInt(42_000))),
	}); err != nil {
		t.Fatalf("InsertAquariusRewardsEvent: %v", err)
	}
	if err := store.InsertAquariusAdminEvent(ctx, timescale.AquariusAdminEvent{
		ContractID: aquariusRwRouter, Ledger: 63_500_050, LedgerCloseTime: claimT, TxHash: pad64("s", 1), OpIndex: 0, EventIndex: 0,
		Kind: timescale.AquariusAdminEnableEmergencyMode, Admin: aquariusRwUserB,
	}); err != nil {
		t.Fatalf("InsertAquariusAdminEvent: %v", err)
	}

	blk, err = store.BuildProtocolBespoke(ctx, "aquarius", "amm", 90)
	if err != nil {
		t.Fatalf("BuildProtocolBespoke: %v", err)
	}
	if blk == nil {
		t.Fatal("BuildProtocolBespoke = nil, want a block carrying reserves + rewards + governance")
	}

	kpis := kpiMap(blk)
	assertKPI(t, kpis, "Rewards-gauge events (lifetime)", "1")
	assertKPI(t, kpis, "Reward claims (30d)", "1")
	assertKPI(t, kpis, "Reward volume (30d)", "42000")
	assertKPI(t, kpis, "Distinct claimants (30d)", "1")
	assertKPI(t, kpis, "Governance events (lifetime)", "1")
	// The pre-existing reserve-depth KPI must still be present (additive, not replaced).
	if _, ok := kpis["Latest reserve snapshot"]; !ok {
		t.Errorf("aquarius block lost its existing reserve-depth KPI after adding rewards; KPIs=%+v", blk.KPIs)
	}

	var hasKindTable, hasGovTable bool
	for _, tb := range blk.Tables {
		switch tb.Title {
		case "Rewards events by kind (lifetime)":
			hasKindTable = true
			if len(tb.Rows) != 1 || tb.Rows[0][0] != "claim_reward" {
				t.Errorf("rewards-by-kind table = %+v, want a single claim_reward row", tb.Rows)
			}
		case "Recent governance events":
			hasGovTable = true
			if len(tb.Rows) != 1 || tb.Rows[0][1] != "enable_emergency_mode" {
				t.Errorf("governance table = %+v, want a single enable_emergency_mode row", tb.Rows)
			}
		}
	}
	if !hasKindTable {
		t.Errorf("bespoke block missing 'Rewards events by kind (lifetime)' table; Tables=%+v", blk.Tables)
	}
	if !hasGovTable {
		t.Errorf("bespoke block missing 'Recent governance events' table; Tables=%+v", blk.Tables)
	}

	var hasClaimSeries bool
	for _, s := range blk.Series {
		if s.Name == "Daily reward claims" {
			hasClaimSeries = true
			if len(s.Points) != 1 || s.Points[0].Value != "1" {
				t.Errorf("claim series points = %+v, want one point of value 1", s.Points)
			}
		}
	}
	if !hasClaimSeries {
		t.Errorf("bespoke block missing 'Daily reward claims' series; Series=%+v", blk.Series)
	}
}

// amtPtr returns a pointer to a canonical.Amount value, for the
// AquariusRewardsEvent.Amount *canonical.Amount optional field.
func amtPtr(a canonical.Amount) *canonical.Amount { return &a }
