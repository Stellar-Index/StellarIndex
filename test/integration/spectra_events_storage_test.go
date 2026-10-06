//go:build integration

package integration_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TestSpectraEvents_Storage executes migration 0210 and the spectra store
// through real TimescaleDB: NUMERIC round-trip, idempotent re-insert,
// the market row merged from three discovery events in any order, the
// generation guard on both tables, and the CHECKs behind the Go validator.
func TestSpectraEvents_Storage(t *testing.T) {
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
		factory  = "CC4ZVRIYM33M5FVAUDFWK7JXO3PWIVSKKEBXVEEC5E6KPYISXIMLUJCP"
		registry = "CCUGRASBWD5SXDYMS7NM437FQ7KNKHFX74D2VRJVTRU4J2TWDMURUW3V"
		pt       = "CDRK5SWZ7DQJ4BZUAZQABPSP7MZS4NO4PPW7LW6CVD5THZDVE63MVLJJ"
		yt       = "CC3MKDR62O4Q7EEBOXXCNFJH3Z6AZQLB3QUTUEKAMSIJIYAHLUPVQR5G"
		ibt      = "CAHPZLEH6O6WJPICJAVRLCTYCAYDN52F4SM6IX3XJKWYSAASSHGZEZBO"
		holder   = "GCFB64LD5OX6XXUQW44LXAE7RAHORF2FJTSQ3DXEAV6DXVOML433X6HF"
		// 10^30: an 18-decimal market's amount well past 2^64 and 2^96.
		e30 = "1000000000000000000000000000000"
	)
	amt := func(s string) canonical.Amount {
		a, err := canonical.FromString(s)
		if err != nil {
			t.Fatalf("amount %q: %v", s, err)
		}
		return a
	}
	t0 := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
	ev := func(tx string, idx uint32, kind timescale.SpectraEventKind, role timescale.SpectraRole, contract string) timescale.SpectraEvent {
		return timescale.SpectraEvent{
			ContractID: contract, Ledger: 64_400_000, LedgerCloseTime: t0,
			TxHash: strings.Repeat(tx, 64), EventIndex: idx, Kind: kind, Role: role,
		}
	}
	count := func(where string, args ...any) int {
		var n int
		if err := store.DB().QueryRowContext(ctx, `SELECT count(*) FROM spectra_events WHERE `+where, args...).Scan(&n); err != nil {
			t.Fatalf("count: %v", err)
		}
		return n
	}

	t.Run("numeric round-trip and idempotent re-insert", func(t *testing.T) {
		mint := ev("a", 1, timescale.SpectraPTMinted, timescale.SpectraRolePT, pt)
		mint.MarketPT, mint.Caller, mint.Receiver, mint.Shares = pt, holder, holder, amt(e30)
		yield := ev("a", 2, timescale.SpectraYieldUpdated, timescale.SpectraRolePT, pt)
		yield.MarketPT, yield.Owner, yield.YieldInIBT = pt, holder, amt("-"+e30)
		transfer := ev("a", 3, timescale.SpectraTransfer, timescale.SpectraRoleYT, yt)
		transfer.MarketPT, transfer.Caller, transfer.Receiver, transfer.Amount = pt, holder, ibt, amt(e30+"7")

		for _, e := range []timescale.SpectraEvent{mint, yield, transfer, mint, transfer} {
			if err := store.InsertSpectraEvent(ctx, e); err != nil {
				t.Fatalf("insert %s: %v", e.Kind, err)
			}
		}
		if n := count(`tx_hash = $1`, mint.TxHash); n != 3 {
			t.Errorf("rows = %d, want 3 (re-insert must be idempotent)", n)
		}
		var shares, yieldStr, amount string
		if err := store.DB().QueryRowContext(ctx, `
			SELECT (SELECT shares::text FROM spectra_events WHERE tx_hash = $1 AND event_kind = 'pt_minted'),
			       (SELECT yield_in_ibt::text FROM spectra_events WHERE tx_hash = $1 AND event_kind = 'yield_updated'),
			       (SELECT amount::text FROM spectra_events WHERE tx_hash = $1 AND event_kind = 'transfer')`,
			mint.TxHash).Scan(&shares, &yieldStr, &amount); err != nil {
			t.Fatalf("read amounts: %v", err)
		}
		if shares != e30 || yieldStr != "-"+e30 || amount != e30+"7" {
			t.Errorf("shares/yield/amount = %s/%s/%s, want %s/-%s/%s7 — NUMERIC lost precision", shares, yieldStr, amount, e30, e30, e30)
		}
		var nulls int
		if err := store.DB().QueryRowContext(ctx, `
			SELECT num_nulls(owner, maker, order_id, ibt, yt, duration_s, vault_shares, assets, amount, yield_in_ibt)
			  FROM spectra_events WHERE tx_hash = $1 AND event_kind = 'pt_minted'`, mint.TxHash).Scan(&nulls); err != nil {
			t.Fatalf("read nulls: %v", err)
		}
		if nulls != 10 {
			t.Errorf("pt_minted has %d NULL columns of the 10 it does not carry, want 10", nulls)
		}
	})

	t.Run("market merged from discovery rows in any order", func(t *testing.T) {
		added := ev("b", 5, timescale.SpectraPTAdded, timescale.SpectraRoleRegistry, registry)
		added.MarketPT = pt
		ytDeployed := ev("b", 4, timescale.SpectraYTDeployed, timescale.SpectraRolePT, pt)
		ytDeployed.MarketPT, ytDeployed.YT = pt, yt
		deployed := ev("b", 3, timescale.SpectraPTDeployed, timescale.SpectraRoleFactory, factory)
		deployed.MarketPT, deployed.Caller, deployed.IBT, deployed.DurationSeconds = pt, holder, ibt, 2_592_000

		check := func(want timescale.SpectraMarket) {
			t.Helper()
			got, err := store.SpectraMarkets(ctx)
			if err != nil {
				t.Fatalf("SpectraMarkets: %v", err)
			}
			if len(got) != 1 || got[0] != want {
				t.Fatalf("markets = %+v, want [%+v]", got, want)
			}
		}

		if err := store.InsertSpectraEvent(ctx, added); err != nil {
			t.Fatalf("pt_added: %v", err)
		}
		check(timescale.SpectraMarket{PT: pt, ListedLedger: 64_400_000})

		if err := store.InsertSpectraEvent(ctx, ytDeployed); err != nil {
			t.Fatalf("yt_deployed: %v", err)
		}
		check(timescale.SpectraMarket{PT: pt, YT: yt, ListedLedger: 64_400_000})

		if err := store.InsertSpectraEvent(ctx, deployed); err != nil {
			t.Fatalf("pt_deployed: %v", err)
		}
		full := timescale.SpectraMarket{
			PT: pt, YT: yt, IBT: ibt, FactoryID: factory, Deployer: holder,
			DurationSeconds: 2_592_000, CreationLedger: 64_400_000, DeployedAt: t0,
			ListedLedger: 64_400_000,
		}
		check(full)

		// Replaying any one discovery row leaves the merged row intact.
		if err := store.InsertSpectraEvent(ctx, ytDeployed); err != nil {
			t.Fatalf("yt_deployed replay: %v", err)
		}
		check(full)
	})

	t.Run("generation guard covers the event and its market", func(t *testing.T) {
		t.Cleanup(func() { store.SetDeriveGeneration(0) })
		const pt2 = "CAAQJ6CN3KWJUG2CTUFUV27IL2BBWEIQKQA27HR7KFFZXROLHN6ELMBI"
		deployed := ev("c", 0, timescale.SpectraPTDeployed, timescale.SpectraRoleFactory, factory)
		deployed.MarketPT, deployed.Caller, deployed.IBT, deployed.DurationSeconds = pt2, holder, ibt, 100
		read := func() (eventDuration, marketDuration, gen int64) {
			t.Helper()
			if err := store.DB().QueryRowContext(ctx, `
				SELECT e.duration_s, m.duration_s, e.derive_generation
				  FROM spectra_events e JOIN spectra_markets m ON m.pt = e.market_pt
				 WHERE e.tx_hash = $1`, deployed.TxHash).Scan(&eventDuration, &marketDuration, &gen); err != nil {
				t.Fatalf("read: %v", err)
			}
			return
		}

		if err := store.InsertSpectraEvent(ctx, deployed); err != nil {
			t.Fatalf("gen0 insert: %v", err)
		}
		store.SetDeriveGeneration(1)
		corrected := deployed
		corrected.DurationSeconds = 2_592_000
		if err := store.InsertSpectraEvent(ctx, corrected); err != nil {
			t.Fatalf("gen1 insert: %v", err)
		}
		if e, m, g := read(); e != 2_592_000 || m != 2_592_000 || g != 1 {
			t.Fatalf("after gen1: event/market duration, gen = %d/%d, %d; want 2592000/2592000, 1", e, m, g)
		}
		store.SetDeriveGeneration(0)
		if err := store.InsertSpectraEvent(ctx, deployed); err != nil {
			t.Fatalf("gen0 replay: %v", err)
		}
		if e, m, g := read(); e != 2_592_000 || m != 2_592_000 || g != 1 {
			t.Errorf("after gen0 replay: event/market duration, gen = %d/%d, %d; want 2592000/2592000, 1 — a stale replay reverted the correction", e, m, g)
		}
	})

	t.Run("CHECKs refuse what the validator refuses", func(t *testing.T) {
		const ins = `
			INSERT INTO spectra_events (ledger_close_time, contract_id, ledger, tx_hash, op_index,
			    event_index, event_kind, role, market_pt, caller, receiver, amount, shares)
			VALUES ($1, $2, 1, $3, 0, $4, $5, $6, $7, $8, $9, $10, $11)`
		tx := strings.Repeat("d", 64)
		cases := []struct {
			name string
			args []any
		}{
			{"transfer with an extra shares column", []any{0, "transfer", "pt", pt, holder, ibt, "1", "1"}},
			{"pt_minted from an IBT", []any{1, "pt_minted", "ibt", pt, holder, holder, nil, "1"}},
			{"PT event naming another market", []any{2, "transfer", "pt", yt, holder, ibt, "1", nil}},
			{"negative transfer amount", []any{3, "transfer", "yt", pt, holder, ibt, "-1", nil}},
		}
		for _, c := range cases {
			args := append([]any{t0, pt, tx}, c.args...)
			if _, err := store.DB().ExecContext(ctx, ins, args...); err == nil {
				t.Errorf("%s: accepted; CHECK missing", c.name)
			}
		}
		if _, err := store.DB().ExecContext(ctx, `INSERT INTO spectra_markets (pt, ibt) VALUES ('X', 'Y')`); err == nil {
			t.Error("spectra_markets accepted ibt without the other pt_deployed columns")
		}
	})
}
