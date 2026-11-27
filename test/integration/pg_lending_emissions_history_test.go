//go:build integration

package integration_test

import (
	"context"
	"fmt"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/domain"
	"github.com/Stellar-Index/StellarIndex/internal/sources/blnt"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TestLendingEmissionsHistory_ReadsExact pins the emissions history reads on real Postgres: the schedule carries every update
// in the window plus each token id's last update at or before from; eps and sums above 2^63 round-trip exact; claims split
// by how many reserves they name with the pool total including all; backstop allocations and BLNT tier claims bucket by hour.
func TestLendingEmissionsHistory_ReadsExact(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	const (
		pool    = "CAJJZSGMMM3PD7N33TAPHGBUGTB43OC73HVIK2L2G6BNGGGYOSSYBXBD"
		other   = "CDVQVKOY2YSXS2IC7KN6MNASSHPAO7UN2UR2ON4OI2SKMFJNVAMDX6DP"
		userA   = "GDEA2WMOWYQ3T56A2PZH5QYYEQBAORW2AUUJYX4FFD5GDPESAICTFTAL"
		userB   = "GBZXN7PIRZGNMHGA7MUUUF4GWPY5AYPV6LY4UV2GL6VJGIQRXFDNMADI"
		u64Max  = uint64(18_446_744_073_709_551_615)
		big9e18 = "9000000000000000000"
	)
	from := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	to := from.Add(3 * time.Hour)
	n := 0
	put := func(e domain.BlendEmissionEvent) {
		n++
		e.Ledger, e.TxHash = uint32(60_000_000+n), fmt.Sprintf("%064x", n)
		if err := store.InsertBlendEmissionEvent(ctx, e); err != nil {
			t.Fatalf("InsertBlendEmissionEvent %d: %v", n, err)
		}
	}
	reu := func(p string, id uint32, at time.Time, eps uint64) {
		put(domain.BlendEmissionEvent{Pool: p, Kind: domain.BlendEventReserveEmissions, Timestamp: at, ResTokenID: id, EmissionsPerSec: eps, Expiration: u64Max})
	}
	reu(pool, 1, from.Add(-2*time.Hour), 1)
	reu(pool, 1, from.Add(-time.Hour), u64Max) // the seed for token 1
	reu(pool, 1, from.Add(30*time.Minute), 7)
	reu(pool, 3, from.Add(-5*time.Hour), 3) // only a seed
	reu(pool, 5, to, 9)                     // at to: outside [from, to)
	reu(other, 1, from.Add(time.Minute), 11)
	reu(pool, 7, from.Add(-time.Hour), 70)
	reu(pool, 7, from, 71) // exactly at from: the seed, not dropped as in-window
	// Two transactions in one ledger update token 9: no apply order is recorded, so tx_hash orders them and both are flagged.
	for _, tx := range []struct {
		hash string
		eps  uint64
	}{{strings.Repeat("b", 64), 90}, {strings.Repeat("a", 64), 91}} {
		if err := store.InsertBlendEmissionEvent(ctx, domain.BlendEmissionEvent{
			Pool: pool, Kind: domain.BlendEventReserveEmissions, Timestamp: from.Add(time.Hour), ResTokenID: 9,
			EmissionsPerSec: tx.eps, Expiration: u64Max, Ledger: 60_500_000, TxHash: tx.hash,
		}); err != nil {
			t.Fatal(err)
		}
	}
	amt := func(s string) *big.Int {
		if s == "" {
			return nil
		}
		v, _ := new(big.Int).SetString(s, 10)
		return v
	}
	claim := func(p, user string, at time.Time, ids []uint32, amount string) {
		put(domain.BlendEmissionEvent{Pool: p, Kind: domain.BlendEventClaim, Timestamp: at, User: user, ReserveTokenIDs: ids, Amount: amt(amount)})
	}
	claim(pool, userA, from.Add(time.Minute), []uint32{1}, big9e18)
	claim(pool, userA, from.Add(2*time.Minute), []uint32{1}, big9e18)
	claim(pool, userB, from.Add(3*time.Minute), []uint32{1, 2}, "5")
	claim(pool, userB, from.Add(4*time.Minute), nil, "")
	claim(pool, userB, from.Add(-time.Minute), []uint32{1}, "1") // before the window
	claim(other, userA, from.Add(time.Minute), []uint32{1}, "1")
	if err := store.InsertBlendBackstopEvent(ctx, timescale.BlendBackstopEvent{
		ContractID: blnt.MainnetBackstop, Ledger: 60_100_000, TxHash: fmt.Sprintf("%064x", 999), ObservedAt: from.Add(2 * time.Hour),
		EventType: timescale.BackstopGulpEmissions, Pool: pool, Amount: big9e18, Amount2: big9e18,
	}); err != nil {
		t.Fatal(err)
	}

	got, err := store.LendingEmissionsHistory(ctx, pool, false, timescale.EmissionHistoryQuery{
		From: from, To: to, Schedule: true, BackstopAlloc: true, BackstopClaim: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	var sched []string
	for _, e := range got.Schedule {
		sched = append(sched, fmt.Sprintf("%d@%s eps=%s exp=%s amb=%t", e.TokenID, e.At.Format("15:04"), e.EPS, e.Expiration, e.OrderAmbiguous))
	}
	const forever = "exp=18446744073709551615"
	want := "1@23:00 eps=18446744073709551615 " + forever + " amb=false|1@00:30 eps=7 " + forever + " amb=false|" +
		"3@19:00 eps=3 " + forever + " amb=false|7@00:00 eps=71 " + forever + " amb=false|" +
		"9@01:00 eps=91 " + forever + " amb=true|9@01:00 eps=90 " + forever + " amb=true"
	if strings.Join(sched, "|") != want {
		t.Fatalf("schedule = %s\nwant      %s", strings.Join(sched, "|"), want)
	}
	var claims []string
	for _, c := range got.Claims {
		id := "-"
		if c.TokenID != nil {
			id = fmt.Sprint(*c.TokenID)
		}
		claims = append(claims, fmt.Sprintf("%s/%s/%s n=%d who=%d amt=%s unread=%d", c.Start.Format("15:04"), c.Scope, id, c.Claims, c.Claimers, c.Amount, c.AmountUnread))
	}
	wantClaims := "00:00/none/- n=1 who=1 amt=0 unread=1|00:00/pool/- n=4 who=2 amt=18000000000000000005 unread=1|" +
		"00:00/several/- n=1 who=1 amt=5 unread=0|00:00/single/1 n=2 who=1 amt=18000000000000000000 unread=0"
	if strings.Join(claims, "|") != wantClaims {
		t.Fatalf("claims = %s\nwant     %s", strings.Join(claims, "|"), wantClaims)
	}
	if len(got.Alloc) != 1 || got.Alloc[0].BackstopShare.String() != big9e18 || got.Alloc[0].PoolShare.String() != big9e18 ||
		!got.Alloc[0].Start.Equal(from.Add(2*time.Hour)) || got.BackstopClaims != nil {
		t.Fatalf("alloc = %+v backstop claims %+v", got.Alloc, got.BackstopClaims)
	}

	blntRows := []timescale.BLNTEvent{
		{
			ContractID: blnt.MainnetFixedPool, ContractRole: string(blnt.RolePool), EventKind: "reserve_emission_update", LedgerCloseTime: from.Add(-time.Hour),
			Attributes: map[string]any{"res_token_id": 1, "eps": "72619047619", "expiration": "1793568773"},
		},
		{
			ContractID: blnt.MainnetBackstop, ContractRole: string(blnt.RoleBackstop), EventKind: "claim", UserAddress: userA, Pool: blnt.MainnetFixedPool,
			Tier: "FirstLoss", LedgerCloseTime: from.Add(90 * time.Minute), Amount: big9e18, Amount2: "3", Attributes: map[string]any{"shares": big9e18},
		},
		{
			ContractID: blnt.MainnetBackstop, ContractRole: string(blnt.RoleBackstop), EventKind: "claim", UserAddress: userB, Pool: blnt.MainnetFixedPool,
			Tier: "FirstLoss", LedgerCloseTime: from.Add(100 * time.Minute), Amount: big9e18, Amount2: "4", Attributes: map[string]any{"shares": big9e18},
		},
		{
			ContractID: blnt.MainnetBackstop, ContractRole: string(blnt.RoleBackstop), EventKind: "gulp_emissions", Pool: blnt.MainnetFixedPool,
			LedgerCloseTime: from.Add(time.Hour), Amount: "8", Amount2: "2",
		},
	}
	for i, r := range blntRows {
		r.Ledger, r.TxHash, r.EventIndex = uint32(61_000_000+i), fmt.Sprintf("%064x", 500+i), uint32(i)
		if err := store.InsertBLNTEvent(ctx, r); err != nil {
			t.Fatalf("InsertBLNTEvent %d: %v", i, err)
		}
	}
	bh, err := store.LendingEmissionsHistory(ctx, blnt.MainnetFixedPool, true, timescale.EmissionHistoryQuery{
		From: from, To: to, Daily: true, Schedule: true, BackstopAlloc: true, BackstopClaim: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(bh.Schedule) != 1 || bh.Schedule[0].EPS.String() != "72619047619" || bh.Schedule[0].Expiration.String() != "1793568773" ||
		len(bh.Claims) != 0 || len(bh.Alloc) != 1 || bh.Alloc[0].BackstopShare.String() != "8" || !bh.Alloc[0].Start.Equal(from) {
		t.Fatalf("blnt = %+v", bh)
	}
	if len(bh.BackstopClaims) != 1 {
		t.Fatalf("blnt backstop claims = %+v", bh.BackstopClaims)
	}
	if b := bh.BackstopClaims[0]; b.Tier != "FirstLoss" || b.Claims != 2 || b.Claimers != 2 || b.BLNT.String() != "18000000000000000000" ||
		b.LP.String() != "7" || b.Shares.String() != "18000000000000000000" || b.Unread != 0 {
		t.Fatalf("blnt tier = %+v", b)
	}
	// With Hours the same window's hours come back from one snapshot, adding up to its days; no schedule is re-read.
	wh, err := store.LendingEmissionsHistory(ctx, blnt.MainnetFixedPool, true, timescale.EmissionHistoryQuery{
		From: from, To: to, Daily: true, Schedule: true, BackstopAlloc: true, BackstopClaim: true, Hours: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if hs := wh.Hours; hs == nil || hs.Schedule != nil || len(hs.Alloc) != 1 || hs.Alloc[0].BackstopShare.String() != "8" ||
		len(hs.BackstopClaims) == 0 || len(wh.Alloc) != 1 || len(wh.Schedule) != 1 {
		t.Fatalf("hours = %+v of %+v", wh.Hours, wh)
	}
	var tierBLNT big.Int
	for _, b := range wh.Hours.BackstopClaims {
		tierBLNT.Add(&tierBLNT, b.BLNT.BigInt())
	}
	if tierBLNT.String() != "18000000000000000000" {
		t.Fatalf("hourly tier blnt = %s, want the day's 18000000000000000000", &tierBLNT)
	}

	assertEmissionPlanUsesIndex(ctx, t, store, timescale.LendingEmissionScheduleQuery(false), "blend_emissions_pool_ts_idx", pool, from, to)
	assertEmissionPlanUsesIndex(ctx, t, store, timescale.LendingEmissionClaimsQuery(false), "blend_emissions_pool_ts_idx", pool, from, to, "hour")
	// BLNT's (contract_role, event_kind, ts) index reads only pool reserve_emission_update rows: narrower than per contract.
	assertEmissionPlanUsesIndex(ctx, t, store, timescale.LendingEmissionScheduleQuery(true), "blnt_events_role_kind_ts_idx", blnt.MainnetFixedPool, from, to)
}

// assertEmissionPlanUsesIndex: the read is led by the per-pool index, so the last-update-before-from lookup walks one
// pool's rows, never the table.
func assertEmissionPlanUsesIndex(ctx context.Context, t *testing.T, store *timescale.Store, q, index string, args ...any) {
	t.Helper()
	conn, err := store.DB().Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	if _, err := conn.ExecContext(ctx, `SET enable_seqscan = off`); err != nil {
		t.Fatal(err)
	}
	rows, err := conn.QueryContext(ctx, `EXPLAIN `+q, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var lines []string
	for rows.Next() {
		var l string
		if err := rows.Scan(&l); err != nil {
			t.Fatal(err)
		}
		lines = append(lines, l)
	}
	if text := strings.Join(lines, "\n"); !strings.Contains(text, index) || strings.Contains(text, "Seq Scan") {
		t.Errorf("plan does not read %s:\n%s", index, text)
	}
}
