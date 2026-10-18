package timescale

import (
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

func TestMergeOracleLatest_NewestPerSourceQuote(t *testing.T) {
	parse := func(id string) canonical.Asset {
		a, err := canonical.ParseAsset(id)
		if err != nil {
			t.Fatalf("ParseAsset(%q): %v", id, err)
		}
		return a
	}
	usd, eur := parse("fiat:USD"), parse("fiat:EUR")
	t0 := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	row := func(src string, q canonical.Asset, ts time.Time, ledger uint32) canonical.OracleUpdate {
		return canonical.OracleUpdate{Source: src, Quote: q, Timestamp: ts, Ledger: ledger}
	}
	allTime := []canonical.OracleUpdate{
		row("dia", usd, t0, 1),                      // silent stream: only the all-time read has it
		row("reflector", usd, t0, 5),                // superseded by the recent read
		row("reflector", eur, t0.Add(time.Hour), 9), // same ts as recent, higher ledger wins
	}
	recent := []canonical.OracleUpdate{
		row("reflector", usd, t0.Add(48*time.Hour), 7),
		row("reflector", eur, t0.Add(time.Hour), 8),
	}
	got := mergeOracleLatest(recent, allTime)
	want := []struct {
		src    string
		quote  canonical.Asset
		ledger uint32
	}{{"dia", usd, 1}, {"reflector", eur, 9}, {"reflector", usd, 7}}
	if len(got) != len(want) {
		t.Fatalf("got %d rows, want %d: %+v", len(got), len(want), got)
	}
	for i, w := range want {
		if got[i].Source != w.src || got[i].Quote.String() != w.quote.String() || got[i].Ledger != w.ledger {
			t.Errorf("row %d = %s/%s ledger %d, want %s/%s ledger %d", i, got[i].Source, got[i].Quote, got[i].Ledger, w.src, w.quote, w.ledger)
		}
	}
}
