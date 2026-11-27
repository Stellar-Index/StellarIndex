//go:build integration

package integration_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/scval"
	chstore "github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// isUTCDay reports a UTC midnight: a calendar day read from ClickHouse must name the UTC day whatever the server's zone.
func isUTCDay(t time.Time) bool {
	return t.Location() == time.UTC && t.Equal(t.Truncate(24*time.Hour))
}

func dayZoneSeed(ctx context.Context, t *testing.T, addr string, events []chstore.ContractEventRow) {
	t.Helper()
	sink, err := chstore.Open(ctx, addr, 1000)
	if err != nil {
		t.Fatalf("open sink: %v", err)
	}
	t.Cleanup(func() { _ = sink.Close(ctx) })
	for _, e := range events {
		ext := chstore.LedgerExtract{
			Ledger: chstore.LedgerRow{LedgerSeq: e.LedgerSeq, CloseTime: e.CloseTime, ProtocolVersion: 23, SorobanEventCount: 1},
			Events: []chstore.ContractEventRow{e},
		}
		if err := sink.Add(ctx, ext); err != nil {
			t.Fatalf("sink add %d: %v", e.LedgerSeq, err)
		}
	}
	if err := sink.Flush(ctx); err != nil {
		t.Fatalf("sink flush: %v", err)
	}
}

func dayZoneEvent(ledger uint32, at time.Time, contractID, sym string) chstore.ContractEventRow {
	return chstore.ContractEventRow{
		LedgerSeq: ledger, CloseTime: at, TxHash: fmt.Sprintf("%064x", ledger), ContractID: contractID,
		EventType: "contract", TopicCount: 1, Topic0Sym: sym, TopicsXDR: []string{scval.MustEncodeSymbol(sym)},
		DataXDR: scval.MustEncodeString("body"), OpArgsXDR: []string{}, InSuccessfulCall: 1,
	}
}

// An event late in yesterday's UTC day, read back on a server east of UTC (the harness runs Europe/Berlin): every
// calendar day the explorer serves for it must be yesterday's UTC date, and the complete-days cut must keep it.
func TestClickHouseDayZone_ExplorerReadsNameTheUTCDay(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)

	const (
		contractID = "CTEST_DAY_ZONE_EXPLORER_CCCCCCCCCCCCCCCCCCCCCCCC"
		asset      = "DAYZONE:GTEST_DAY_ZONE_ISSUER"
		ledger     = uint32(79_510_001)
	)
	// 23:30Z sits past Berlin's midnight in either season, so a server-zone day boundary moves it.
	yesterday := time.Now().UTC().Truncate(24*time.Hour).AddDate(0, 0, -1)
	if time.Now().UTC().Hour() >= 22 {
		t.Skip("within two hours of UTC midnight Berlin's date is already tomorrow's; the cut cannot tell the zones apart")
	}
	at := yesterday.Add(23*time.Hour + 30*time.Minute)
	// 00:30Z bounds the day from the other side, for a server west of UTC.
	dayZoneSeed(ctx, t, addr, []chstore.ContractEventRow{
		dayZoneEvent(ledger, yesterday.Add(30*time.Minute), contractID, "swap"),
		dayZoneEvent(ledger+1, at, contractID, "swap"),
	})

	er, err := chstore.NewExplorerReader(ctx, addr)
	if err != nil {
		t.Fatalf("explorer reader: %v", err)
	}
	t.Cleanup(func() { _ = er.Close() })

	sum, ok, err := er.ContractActivitySummaryFor(ctx, contractID, 30, ledger)
	if err != nil || !ok {
		t.Fatalf("ContractActivitySummaryFor = ok %v, %v", ok, err)
	}
	if len(sum.Daily) != 1 || !sum.Daily[0].Date.Equal(yesterday) || !isUTCDay(sum.Daily[0].Date) || sum.Daily[0].ActiveLedgers != 2 {
		t.Errorf("activity series = %+v; want one day at %s", sum.Daily, yesterday.Format(time.RFC3339))
	}

	daily, err := er.ProtocolDailyActivity(ctx, []string{contractID}, ledger)
	if err != nil {
		t.Fatalf("ProtocolDailyActivity: %v", err)
	}
	if len(daily) != 1 || daily[0].Date != yesterday.Format(time.DateOnly) || daily[0].Events != 2 {
		t.Errorf("protocol daily = %+v; want yesterday (%s) with both its events: the complete-days cut is a UTC midnight",
			daily, yesterday.Format(time.DateOnly))
	}

	fast, err := er.ProtocolContractActivityFast(ctx, []string{contractID}, yesterday)
	if err != nil {
		t.Fatalf("ProtocolContractActivityFast: %v", err)
	}
	if len(fast) != 1 || !fast[0].LastSeen.Equal(yesterday) {
		t.Errorf("contract activity (daily table) = %+v; want last_seen %s", fast, yesterday.Format(time.RFC3339))
	}

	raw := dialClickHouse(t, ctx, "stellar")
	if err := raw.Exec(ctx, `INSERT INTO stellar.asset_entry_ledgers (asset, entry_type, ledger, n_created, n_updated, n_removed)
		VALUES (?, 'trustline', ?, 1, 0, 0)`, asset, ledger); err != nil {
		t.Fatalf("seed asset_entry_ledgers: %v", err)
	}
	trust, err := er.AssetTrustlineDaily(ctx, asset, ledger)
	if err != nil {
		t.Fatalf("AssetTrustlineDaily: %v", err)
	}
	if len(trust) != 1 || !trust[0].Day.Equal(yesterday) || !isUTCDay(trust[0].Day) {
		t.Errorf("trustline days = %+v; want one at %s", trust, yesterday.Format(time.RFC3339))
	}

	if err := raw.Exec(ctx, `INSERT INTO stellar.contracts_census_daily (day, contract_id, events, last_ledger, last_seen)
		VALUES (?, ?, 1, ?, ?)`, yesterday.Format(time.DateOnly), contractID, ledger, at); err != nil {
		t.Fatalf("seed census: %v", err)
	}
	stats, err := er.ContractStats(ctx)
	if err != nil {
		t.Fatalf("ContractStats: %v", err)
	}
	if stats.Active90d == nil || !isUTCDay(stats.CensusThrough) {
		t.Errorf("census through = %s (active90d %v); want a UTC midnight", stats.CensusThrough.Format(time.RFC3339), stats.Active90d)
	}
	for _, m := range stats.Deployments {
		if !isUTCDay(m.Month) || m.Month.Day() != 1 {
			t.Errorf("deployment month = %s; want the 1st at UTC midnight", m.Month.Format(time.RFC3339))
		}
	}
	for name, read := range map[string]func(context.Context, string) (time.Time, error){
		"CensusMaxDay": func(ctx context.Context, addr string) (time.Time, error) {
			d, _, err := chstore.CensusMaxDay(ctx, addr)
			return d, err
		},
		"EarliestEventDay": chstore.EarliestEventDay,
	} {
		d, err := read(ctx, addr)
		if err != nil || !isUTCDay(d) {
			t.Errorf("%s = %s, %v; want a UTC midnight", name, d.Format(time.RFC3339), err)
		}
	}
}

// A first mint late in its UTC day must be found: the raw read's window is that UTC day, not the server zone's.
func TestClickHouseDayZone_FirstSupplyEventLateInTheUTCDay(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)

	const contractID = "CTEST_DAY_ZONE_FIRST_SUPPLY_CCCCCCCCCCCCCCCCCCCCC"
	// Berlin is UTC+2 in June: 22:30Z is already the next Berlin day.
	at := time.Date(2026, 6, 2, 22, 30, 0, 0, time.UTC)
	dayZoneSeed(ctx, t, addr, []chstore.ContractEventRow{dayZoneEvent(79_520_001, at, contractID, "mint")})

	sr, err := chstore.NewSupplyReader(ctx, addr)
	if err != nil {
		t.Fatalf("supply reader: %v", err)
	}
	t.Cleanup(func() { _ = sr.Close() })
	ledger, got, err := sr.FirstSupplyEvent(ctx, contractID)
	if err != nil {
		t.Fatalf("FirstSupplyEvent: %v", err)
	}
	if ledger != 79_520_001 || !got.Equal(at) {
		t.Fatalf("FirstSupplyEvent = %d at %v; want 79520001 at %v", ledger, got, at)
	}
}
