package vendorcsv

import (
	"context"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/sources/external"
	"github.com/Stellar-Index/StellarIndex/internal/sources/external/scale"
)

const fixture = `event_date,close_price_usd,market_cap_usd,volume_usd
2014-08-06 00:00:00 UTC,0.002511,963158.13045,30983.999999999996
2014-08-07 00:00:00 UTC,0.00254,974000.5,31000
2014-08-09 00:00:00 UTC,0.0026,980000,32000
2026-09-30 00:00:00 UTC,"",7789819433.437433,514112733.7019048
`

func xlmUSD(t *testing.T) canonical.Pair {
	t.Helper()
	base, err := canonical.NewCryptoAsset("XLM")
	if err != nil {
		t.Fatal(err)
	}
	quote, err := canonical.NewFiatAsset("USD")
	if err != nil {
		t.Fatal(err)
	}
	pair, err := canonical.NewPair(base, quote)
	if err != nil {
		t.Fatal(err)
	}
	return pair
}

func writeFixture(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "export.csv")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func day(s string) time.Time {
	d, err := time.Parse("2006-01-02", s)
	if err != nil {
		panic(err)
	}
	return d.UTC()
}

func TestParseCloses_HeaderDrivenAndScaled(t *testing.T) {
	body := "\uFEFFvolume_usd,close_price_usd,event_date\n" +
		"30983.999999999996,30983.999999999996,2014-08-06 00:00:00 UTC\n" +
		"1,0.002511,2014-08-07 00:00:00 UTC\n"
	got, err := ParseCloses(strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d closes, want 2", len(got))
	}
	if !got[0].Day.Equal(day("2014-08-06")) || got[0].Close.String() != "3098399999999" {
		t.Errorf("row 0 = %s %s; want 2014-08-06 3098399999999 (over-precision truncated at 8 decimals)", got[0].Day, got[0].Close)
	}
	if got[1].Close.String() != "251100" {
		t.Errorf("row 1 close = %s; want 251100", got[1].Close)
	}
}

func TestParseCloses_BlankAndNonPositiveClosesAreSkipped(t *testing.T) {
	body := "event_date,close_price_usd\n" +
		"2014-08-06 00:00:00 UTC,\"\"\n" +
		"2014-08-07 00:00:00 UTC,0\n" +
		"2014-08-08 00:00:00 UTC,-1\n" +
		"2014-08-09 00:00:00 UTC,0.5\n"
	got, err := ParseCloses(strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || !got[0].Day.Equal(day("2014-08-09")) {
		t.Fatalf("got %+v; want only 2014-08-09", got)
	}
}

func TestParseCloses_Rejects(t *testing.T) {
	cases := map[string]string{
		"missing column": "event_date,volume_usd\n2014-08-06 00:00:00 UTC,1\n",
		"out of order":   "event_date,close_price_usd\n2014-08-07 00:00:00 UTC,1\n2014-08-06 00:00:00 UTC,1\n",
		"duplicate day":  "event_date,close_price_usd\n2014-08-06 00:00:00 UTC,1\n2014-08-06 00:00:00 UTC,1\n",
		"not midnight":   "event_date,close_price_usd\n2014-08-06 12:00:00 UTC,1\n",
		"bad date":       "event_date,close_price_usd\n06/08/2014,1\n",
		"bad close":      "event_date,close_price_usd\n2014-08-06 00:00:00 UTC,abc\n",
		"scientific":     "event_date,close_price_usd\n2014-08-06 00:00:00 UTC,2.5e-3\n",
		"ragged row":     "event_date,close_price_usd\n2014-08-06 00:00:00 UTC\n",
	}
	for name, body := range cases {
		if _, err := ParseCloses(strings.NewReader(body)); err == nil {
			t.Errorf("%s: want error, got nil", name)
		}
	}
}

func TestBackfill_StampsHashesAndAmounts(t *testing.T) {
	b := &Backfiller{Path: writeFixture(t, fixture), Now: func() time.Time { return day("2026-10-01") }}
	got, err := b.Backfill(context.Background(), xlmUSD(t), day("2014-01-01"), day("2027-01-01"), Day)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d trades, want 3 (blank close dropped)", len(got))
	}
	first := got[0]
	wantTS := day("2014-08-06").Add(-time.Second)
	if !first.Timestamp.Equal(wantTS) {
		t.Errorf("timestamp = %s; want %s (one second before the snapshot day)", first.Timestamp, wantTS)
	}
	wantHash, err := scale.CandleTxHash("XLMUSD", wantTS.Unix(), Day)
	if err != nil {
		t.Fatal(err)
	}
	if first.TxHash != wantHash {
		t.Errorf("tx_hash = %s; want %s", first.TxHash, wantHash)
	}
	if first.Source != SourceName || first.Ledger != 0 || first.OpIndex != 0 {
		t.Errorf("identity = %s/%d/%d; want %s/0/0", first.Source, first.Ledger, first.OpIndex, SourceName)
	}
	if first.BaseAmount.String() != big.NewInt(100_000_000).String() || first.QuoteAmount.String() != "251100" {
		t.Errorf("amounts = %s/%s; want 100000000/251100", first.BaseAmount, first.QuoteAmount)
	}
	if err := first.Validate(); err != nil {
		t.Errorf("Validate: %v", err)
	}
	// The same input yields the same identity: a re-run upserts instead of duplicating.
	again, err := b.Backfill(context.Background(), xlmUSD(t), day("2014-01-01"), day("2027-01-01"), Day)
	if err != nil || again[0].TxHash != first.TxHash || !again[0].Timestamp.Equal(first.Timestamp) {
		t.Errorf("second run = %+v, %v; want the same identity", again[0], err)
	}
}

func TestBackfill_MissingDaysAreOmittedNotInterpolated(t *testing.T) {
	b := &Backfiller{Path: writeFixture(t, fixture), Now: func() time.Time { return day("2026-10-01") }}
	got, err := b.Backfill(context.Background(), xlmUSD(t), day("2014-01-01"), day("2015-01-01"), Day)
	if err != nil {
		t.Fatal(err)
	}
	var days []string
	for _, tr := range got {
		days = append(days, tr.Timestamp.Add(time.Second).Format("2006-01-02"))
	}
	if strings.Join(days, " ") != "2014-08-06 2014-08-07 2014-08-09" {
		t.Errorf("days = %v; want 2014-08-08 absent", days)
	}
}

func TestBackfill_ClipsToWindowAndClosedDays(t *testing.T) {
	b := &Backfiller{Path: writeFixture(t, fixture), Now: func() time.Time { return day("2014-08-07") }}
	// -to at the snapshot day keeps the row stamped one second before it.
	got, err := b.Backfill(context.Background(), xlmUSD(t), day("2014-08-06"), day("2014-08-07"), Day)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || !got[0].Timestamp.Equal(day("2014-08-07").Add(-time.Second)) {
		t.Fatalf("got %+v; want only the 2014-08-07 row", got)
	}
	// A snapshot day after Now is not closed yet.
	b.Now = func() time.Time { return day("2014-08-06").Add(12 * time.Hour) }
	got, err = b.Backfill(context.Background(), xlmUSD(t), day("2014-01-01"), day("2027-01-01"), Day)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || !got[0].Timestamp.Equal(day("2014-08-06").Add(-time.Second)) {
		t.Fatalf("got %+v; want only the 2014-08-06 row", got)
	}
}

func TestBackfill_RejectsGranularityPairWindowAndPath(t *testing.T) {
	ctx := context.Background()
	pair := xlmUSD(t)
	b := &Backfiller{Path: writeFixture(t, fixture)}
	if _, err := b.Backfill(ctx, pair, day("2014-01-01"), day("2015-01-01"), time.Hour); err == nil {
		t.Error("1h granularity accepted")
	}
	if _, err := b.Backfill(ctx, pair, day("2015-01-01"), day("2015-01-01"), Day); err == nil {
		t.Error("empty window accepted")
	}
	btc, err := canonical.NewCryptoAsset("BTC")
	if err != nil {
		t.Fatal(err)
	}
	other, err := canonical.NewPair(btc, pair.Quote)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.Backfill(ctx, other, day("2014-01-01"), day("2015-01-01"), Day); err == nil {
		t.Error("BTC/USD accepted")
	}
	missing := &Backfiller{Path: filepath.Join(t.TempDir(), "nope.csv")}
	if _, err := missing.Backfill(ctx, pair, day("2014-01-01"), day("2015-01-01"), Day); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("missing file: err = %v; want ErrNotExist", err)
	}
	if b.Name() != SourceName || b.Class() != external.ClassAggregator {
		t.Errorf("connector = %s/%s; want %s/%s", b.Name(), b.Class(), SourceName, external.ClassAggregator)
	}
}
