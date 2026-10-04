package v1_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// sourceHistoryReader adds the single-source read to the stub, filtering the
// fixture the way the SQL does.
type sourceHistoryReader struct {
	*stubHistoryReader
	gotSource string
}

func (r *sourceHistoryReader) TradesInRangeAfterFromSource(
	ctx context.Context, pair canonical.Pair, source string,
	from, to, afterTs time.Time, afterLedger uint32,
	afterTxHash, afterSource string, afterOpIndex uint32, limit int,
) ([]canonical.Trade, error) {
	r.gotSource = source
	all, err := r.TradesInRangeAfter(ctx, pair, from, to, afterTs, afterLedger,
		afterTxHash, afterSource, afterOpIndex, limit)
	var out []canonical.Trade
	for _, t := range all {
		if t.Source == source {
			out = append(out, t)
		}
	}
	return out, err
}

func historySourceFixture() *sourceHistoryReader {
	a, b := mkHistTrade(10), mkHistTrade(20)
	b.Source = "sdex"
	b.Ledger = 2
	return &sourceHistoryReader{stubHistoryReader: &stubHistoryReader{trades: []canonical.Trade{a, b}}}
}

func historyRowSources(t *testing.T, resp *http.Response) []string {
	t.Helper()
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	var page struct {
		Data []struct {
			Source string `json:"source"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &page); err != nil {
		t.Fatalf("decode: %v: %s", err, body)
	}
	var out []string
	for _, r := range page.Data {
		out = append(out, r.Source)
	}
	return out
}

func TestHistory_SourceFilterRestrictsRows(t *testing.T) {
	rd := historySourceFixture()
	ts := httpTestServer(t, v1.New(v1.Options{History: rd}))
	base := ts.URL + "/v1/history?base=native&quote=fiat:USD&from=2026-02-25T00:00:00Z&to=2026-03-02T00:00:00Z"

	resp := mustGet(t, base+"&source=sdex")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	got := historyRowSources(t, resp)
	if len(got) == 0 {
		t.Fatal("no rows for source=sdex")
	}
	for _, s := range got {
		if s != "sdex" {
			t.Errorf("source=sdex returned a %q row", s)
		}
	}
	if rd.gotSource != "sdex" {
		t.Errorf("reader saw source %q, want sdex", rd.gotSource)
	}

	if all := historyRowSources(t, mustGet(t, base)); len(all) < 2 {
		t.Errorf("unfiltered request returned %d rows, want both sources", len(all))
	}
}

func TestHistory_SourceFilterValidation(t *testing.T) {
	ts := httpTestServer(t, v1.New(v1.Options{History: historySourceFixture()}))
	for _, c := range []struct{ source, problem string }{
		{"nope-not-a-source", "unknown-source"},
		{"coingecko", "off-chain-source-filter"},
		{"binance", "off-chain-source-filter"},
	} {
		resp := mustGet(t, ts.URL+"/v1/history?base=native&quote=fiat:USD&source="+c.source)
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("source=%s: status = %d, want 400", c.source, resp.StatusCode)
		}
		if !strings.Contains(string(body), c.problem) {
			t.Errorf("source=%s: body lacks %q: %s", c.source, c.problem, body)
		}
	}
}

func TestHistory_SourceFilterUnsupportedReader503(t *testing.T) {
	ts := httpTestServer(t, v1.New(v1.Options{History: &stubHistoryReader{}}))
	resp := mustGet(t, ts.URL+"/v1/history?base=native&quote=fiat:USD&source=sdex")
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
}
