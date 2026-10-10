package v1_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"

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
