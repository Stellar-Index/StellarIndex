//go:build integration

package integration_test

import (
	"context"
	"fmt"
	"math/big"
	"testing"
	"time"

	chstore "github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TestClickHouseCohortFlowsCarryTheMonthsOwnPrice runs one whole cohort
// cycle against live ClickHouse — the month-price loader, every fold,
// and the EXCHANGE that now swaps stellar.asset_month_usd_prices live
// beside the cohort tables — then reads the cohort back through the
// repo's own reader and pins the read-time join: a flow row carries
// the served tier's price for ITS (asset, month) and nothing else. USDC
// is priced for May and XLM for May only; the cohort moved USDC in May
// and XLM in June, so the May USDC row must read "0.998" and the June
// XLM row must be unpriced although XLM has a row in the table — a join
// on asset alone, or on month alone, fails this.
func TestClickHouseCohortFlowsCarryTheMonthsOwnPrice(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)
	raw := dialClickHouse(t, ctx, "stellar")

	const (
		root   = "GTEST_COHORT_THEN_SPONSOR_AAAAAAAAAAAAAAAAAAAAAAAAAA"
		member = "GTEST_COHORT_THEN_MEMBER_AAAAAAAAAAAAAAAAAAAAAAAAAAA"
		usdc   = "USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
		mayLdg = uint32(77_700_001)
		junLdg = uint32(77_700_002)
	)
	may := time.Date(2026, 5, 12, 9, 0, 0, 0, time.UTC)
	jun := time.Date(2026, 6, 12, 9, 0, 0, 0, time.UTC)
	mayMonth := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)

	// A lake tip the cycle can walk to, in a partition of its own.
	lb, err := raw.PrepareBatch(ctx, `INSERT INTO stellar.ledgers (ledger_seq, close_time, ledger_hash, prev_hash, protocol_version)`)
	if err != nil {
		t.Fatalf("prepare ledgers: %v", err)
	}
	for seq, at := range map[uint32]time.Time{mayLdg: may, junLdg: jun} {
		if err := lb.Append(seq, at, fmt.Sprintf("%064d", seq), "00", uint32(23)); err != nil {
			t.Fatalf("append ledger: %v", err)
		}
	}
	if err := lb.Send(); err != nil {
		t.Fatalf("send ledgers: %v", err)
	}

	// One sponsored member — every sponsor is covered, no floor.
	if err := raw.Exec(ctx, `INSERT INTO stellar.account_sponsor_edges
		(sponsor, sponsored, sponsorships_started, first_ledger, last_ledger, first_at, last_at)
		VALUES (?, ?, 1, ?, ?, ?, ?)`, root, member, mayLdg, mayLdg, may, may); err != nil {
		t.Fatalf("insert sponsor edge: %v", err)
	}

	// The member's movements: 100 USDC in and 25 out in May, 3 XLM in
	// in June.
	mb, err := raw.PrepareBatch(ctx, `INSERT INTO stellar.account_movements
		(address, ledger, ledger_close_time, tx_hash, op_index, leg_index, direction, movement_kind, provenance, asset, counterparty, amount)`)
	if err != nil {
		t.Fatalf("prepare movements: %v", err)
	}
	for i, m := range []struct {
		ledger uint32
		at     time.Time
		dir    chstore.AccountMovementDirection
		asset  string
		amount int64
	}{
		{mayLdg, may, chstore.AccountMovementReceived, usdc, 1_000_000_000},
		{mayLdg, may, chstore.AccountMovementSent, usdc, 250_000_000},
		{junLdg, jun, chstore.AccountMovementReceived, "native", 30_000_000},
	} {
		if err := mb.Append(member, m.ledger, m.at, fmt.Sprintf("%064d", m.ledger), uint32(0), uint32(i),
			string(m.dir), "payment", "classic", m.asset, root, big.NewInt(m.amount)); err != nil {
			t.Fatalf("append movement: %v", err)
		}
	}
	if err := mb.Send(); err != nil {
		t.Fatalf("send movements: %v", err)
	}

	prices := []timescale.MonthlyUSDVWAP{
		{Asset: usdc, Month: mayMonth, VWAPUSD: "0.998", VolumeUSD: 12345.5},
		{Asset: "native", Month: mayMonth, VWAPUSD: "0.1", VolumeUSD: 99.25},
	}
	if err := chstore.RunCohortRollup(ctx, addr, nil, prices, t.Logf); err != nil {
		t.Fatalf("RunCohortRollup: %v", err)
	}

	// The loader's rows were staged and the swap served them.
	var served uint64
	if err := raw.QueryRow(ctx, `SELECT count() FROM stellar.asset_month_usd_prices`).Scan(&served); err != nil {
		t.Fatalf("count served month prices: %v", err)
	}
	if served != uint64(len(prices)) {
		t.Fatalf("stellar.asset_month_usd_prices serves %d rows after the cycle, want %d", served, len(prices))
	}
	var servedPrice string
	var servedVolume float64
	if err := raw.QueryRow(ctx, `SELECT vwap_usd, volume_usd FROM stellar.asset_month_usd_prices WHERE asset = ? AND month = ?`,
		usdc, mayMonth).Scan(&servedPrice, &servedVolume); err != nil {
		t.Fatalf("read served USDC May price: %v", err)
	}
	if servedPrice != "0.998" || servedVolume != 12345.5 {
		t.Errorf("served USDC May = (%q, %v), want (\"0.998\", 12345.5)", servedPrice, servedVolume)
	}

	er, err := chstore.NewExplorerReader(ctx, addr)
	if err != nil {
		t.Fatalf("NewExplorerReader: %v", err)
	}
	t.Cleanup(func() { _ = er.Close() })
	cohort, ok, err := er.AccountCohort(ctx, root, chstore.CohortRelationSponsored)
	if err != nil {
		t.Fatalf("AccountCohort: %v", err)
	}
	if !ok || !cohort.Covered {
		t.Fatalf("cohort ok=%v covered=%v, want a covered sponsor cohort", ok, cohort.Covered)
	}

	type key struct {
		month string
		asset string
	}
	got := map[key]*string{}
	for _, f := range cohort.Flows {
		got[key{f.Month.UTC().Format("2006-01"), f.Asset}] = f.PriceUSDThen
	}
	show := func(p *string) string {
		if p == nil {
			return "<absent>"
		}
		return *p
	}
	for _, tc := range []struct {
		k    key
		want string // "" = absent
	}{
		{key{"2026-05", usdc}, "0.998"},
		{key{"2026-05", chstore.CohortAllAssets}, ""},
		{key{"2026-06", "native"}, ""}, // XLM is priced for May, not June: the join is on (asset, month)
		{key{"2026-06", chstore.CohortAllAssets}, ""},
	} {
		p, present := got[tc.k]
		if !present {
			t.Errorf("no flow row for %s %s (flows: %+v)", tc.k.month, tc.k.asset, cohort.Flows)
			continue
		}
		if show(p) != tc.want && !(tc.want == "" && p == nil) {
			t.Errorf("%s %s: price_usd_then = %s, want %q", tc.k.month, tc.k.asset, show(p), tc.want)
		}
	}
	if len(got) != 4 {
		t.Errorf("flow rows = %d, want 4 (two months × asset + all-assets): %+v", len(got), cohort.Flows)
	}
	if f := got[key{"2026-05", usdc}]; f != nil {
		// The amount the price applies to is the fold's own.
		for _, fl := range cohort.Flows {
			if fl.Asset == usdc && fl.Inflow.Int64() != 1_000_000_000 {
				t.Errorf("USDC May inflow = %s, want 1000000000", fl.Inflow)
			}
		}
	}
}
