package ingest

import (
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/config"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

func cgPair(t *testing.T, base string) canonical.Pair {
	t.Helper()
	b, err := canonical.NewCryptoAsset(base)
	if err != nil {
		t.Fatal(err)
	}
	q, err := canonical.NewFiatAsset("USD")
	if err != nil {
		t.Fatal(err)
	}
	pr, err := canonical.NewPair(b, q)
	if err != nil {
		t.Fatal(err)
	}
	return pr
}

// Live rows every 5 minutes up to 16:35 and from 18:15, the shape of an outage inside a window
// the operator padded on both sides; only vendor points in the hole may be written.
func TestDropNearLive_KeepsOnlyTheHole(t *testing.T) {
	xlm, btc := cgPair(t, "XLM"), cgPair(t, "BTC")
	day := time.Date(2026, 11, 15, 0, 0, 0, 0, time.UTC)
	at := func(h, m int) time.Time { return day.Add(time.Duration(h)*time.Hour + time.Duration(m)*time.Minute) }

	var live []timescale.OracleStamp
	for m := 0; m <= 35; m += 5 {
		live = append(live, timescale.OracleStamp{Asset: xlm.Base.String(), Quote: xlm.Quote.String(), TS: at(16, m)})
	}
	for m := 15; m <= 60; m += 5 {
		live = append(live, timescale.OracleStamp{Asset: xlm.Base.String(), Quote: xlm.Quote.String(), TS: at(18, m)})
	}
	upd := func(pr canonical.Pair, ts time.Time) canonical.OracleUpdate {
		return canonical.OracleUpdate{Source: "coingecko", Asset: pr.Base, Quote: pr.Quote, Timestamp: ts}
	}
	updates := []canonical.OracleUpdate{
		upd(xlm, at(16, 3)),  // between live rows
		upd(xlm, at(16, 45)), // exactly 10m after the last live row: still covered
		upd(xlm, at(16, 46)), // in the hole
		upd(xlm, at(17, 3)),  // in the hole
		upd(xlm, at(18, 4)),  // 11m before live resumes: in the hole
		upd(xlm, at(18, 5)),  // 10m before live resumes: covered
		upd(btc, at(16, 3)),  // another pair's live rows do not cover this one
	}
	kept, covered := dropNearLive(updates, live, 10*time.Minute)
	want := []canonical.OracleUpdate{updates[2], updates[3], updates[4], updates[6]}
	if covered != 3 || len(kept) != len(want) {
		t.Fatalf("kept %d covered %d, want kept %d covered 3: %+v", len(kept), covered, len(want), kept)
	}
	for i := range want {
		if !kept[i].Timestamp.Equal(want[i].Timestamp) || kept[i].Asset != want[i].Asset {
			t.Errorf("kept[%d] = %s %s, want %s %s", i, kept[i].Asset, kept[i].Timestamp, want[i].Asset, want[i].Timestamp)
		}
	}
}

func TestCoinGeckoHistoryPlan_MirrorsLiveWiring(t *testing.T) {
	vc := config.CoinGeckoVenueConfig{APIKey: " pro-not-real ", PollInterval: 2 * time.Minute}
	p, pairs, err := coingeckoHistoryPlan(vc, "all")
	if err != nil {
		t.Fatal(err)
	}
	if p.APIKey != "pro-not-real" || p.PollInterval() != 2*time.Minute || len(p.TickerToID) == 0 {
		t.Errorf("poller not wired like the indexer's: key=%q interval=%v ids=%d", p.APIKey, p.PollInterval(), len(p.TickerToID))
	}
	var hasXLM bool
	for _, pr := range pairs {
		hasXLM = hasXLM || pr.String() == cgPair(t, "XLM").String()
	}
	if !hasXLM {
		t.Errorf("all-pairs plan lacks XLM/USD: %v", pairs)
	}
	_, one, err := coingeckoHistoryPlan(vc, "xlm/usd")
	if err != nil || len(one) != 1 || one[0].String() != cgPair(t, "XLM").String() {
		t.Errorf("single pair plan = %v, %v", one, err)
	}
	if _, _, err := coingeckoHistoryPlan(vc, "NOPE/USD"); err == nil {
		t.Error("unknown pair accepted")
	}
}
