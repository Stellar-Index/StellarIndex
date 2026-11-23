package v1_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"sort"
	"testing"
	"time"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/pricingguard"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// pagedHistoryReader honours the cursor and limit, one fill per second so ts alone orders the page.
type pagedHistoryReader struct {
	stubHistoryReader
	rows []canonical.Trade
}

func (r *pagedHistoryReader) TradesInRangeAfter(_ context.Context, pair canonical.Pair, _, _, afterTs time.Time, _ uint32, _, _ string, _ uint32, limit int) ([]canonical.Trade, error) {
	var out []canonical.Trade
	for _, t := range r.rows {
		if t.Pair.Equal(pair) && (afterTs.IsZero() || t.Timestamp.After(afterTs)) {
			out = append(out, t)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Timestamp.Before(out[j].Timestamp) })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

type histPage struct {
	status  int
	cache   string
	ledgers []uint32
	next    string
	// withheld is flags.fills_withheld_reason.
	withheld string
}

func histPageGet(t *testing.T, srv *v1.Server, path string) histPage {
	t.Helper()
	ts := startHTTPTest(t, srv.Handler())
	resp := mustGet(t, ts.URL+path)
	raw, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	var body struct {
		Data       []struct{ Ledger uint32 }
		Pagination *struct{ Next string }
		Flags      struct {
			FillsWithheldReason string `json:"fills_withheld_reason"`
		}
	}
	_ = json.Unmarshal(raw, &body)
	p := histPage{status: resp.StatusCode, cache: resp.Header.Get("Cache-Control"), withheld: body.Flags.FillsWithheldReason}
	for _, d := range body.Data {
		p.ledgers = append(p.ledgers, d.Ledger)
	}
	if body.Pagination != nil {
		p.next = body.Pagination.Next
	}
	return p
}

// histFills builds native/fiat:USD fills one second apart on ledgers 10, 11, ...; soroswap at the listed indices, aquarius
// elsewhere.
func histFills(n int, soroswap ...int) []canonical.Trade {
	out := make([]canonical.Trade, n)
	for i := range out {
		out[i] = mkHistTrade(100 + int64(i))
		out[i].Source, out[i].Ledger = "aquarius", uint32(10+i)
		out[i].Timestamp = out[i].Timestamp.Add(time.Duration(i) * time.Second)
	}
	for _, i := range soroswap {
		out[i].Source = "soroswap"
	}
	return out
}

// TestHistoryPagesPastExcludedFills: covered fills are removed from a page flagged degraded (no-store) while the cursor stays
// the read's, so page 2 starts right after page 1's last RAW row and no later fill becomes unreachable. A page emptied by the
// exclusion still serves its cursor; only an emptied terminal page is withheld.
func TestHistoryPagesPastExcludedFills(t *testing.T) {
	soroswapWide := []timescale.SourceExclusion{{ID: 1, Source: "soroswap", From: time.Unix(1_000_000_000, 0)}}
	srv := func(rows []canonical.Trade) *v1.Server {
		return v1.New(v1.Options{
			History:    &pagedHistoryReader{rows: rows},
			Exclusions: pricingguard.NewSourceExclusionGate(exclTable{rows: soroswapWide}, pricingguard.SourceExclusionGateOptions{}),
		})
	}
	const path = "/v1/history?base=native&quote=fiat:USD&from=2026-01-01T00:00:00Z&to=2027-01-01T00:00:00Z&limit=4"
	// Covered fills in the middle (11) and at the END (13) of page 1.
	s := srv(histFills(6, 1, 3, 5))
	p1 := histPageGet(t, s, path)
	if p1.status != http.StatusOK || p1.cache != "no-store" || !equalLedgers(p1.ledgers, 10, 12) || p1.next == "" ||
		p1.withheld != "source_excluded" {
		t.Fatalf("page 1 = %+v, want 200 no-store [10 12] with a cursor, fills withheld for source_excluded", p1)
	}
	p2 := histPageGet(t, s, path+"&cursor="+url.QueryEscape(p1.next))
	if p2.status != http.StatusOK || !equalLedgers(p2.ledgers, 14) || p2.next != "" || p2.withheld != "source_excluded" {
		t.Fatalf("page 2 = %+v, want [14] (the fill right after page 1's last raw row 13), terminal, fills withheld", p2)
	}
	if u := histPageGet(t, srv(histFills(3)), path); u.status != http.StatusOK || !equalLedgers(u.ledgers, 10, 11, 12) ||
		u.withheld != "" || u.cache == "no-store" {
		t.Fatalf("untouched page = %+v, want every fill, no withheld marker, cacheable", u)
	}

	// A page the exclusion empties keeps its cursor; the emptied terminal page behind it is withheld.
	s = srv(histFills(6, 0, 1, 2, 3, 5))
	if e := histPageGet(t, s, path); e.status != http.StatusOK || e.cache != "no-store" || len(e.ledgers) != 0 || e.next == "" ||
		e.withheld != "source_excluded" {
		t.Fatalf("emptied page = %+v, want 200 no-store, no rows, a cursor, fills withheld for source_excluded", e)
	} else if p := histPageGet(t, s, path+"&cursor="+url.QueryEscape(e.next)); p.status != http.StatusOK || !equalLedgers(p.ledgers, 14) {
		t.Fatalf("after the emptied page = %+v, want [14]", p)
	}
	if e := histPageGet(t, srv(histFills(2, 0, 1)), path); e.status != http.StatusNotFound {
		t.Fatalf("emptied terminal page = %+v, want 404 withheld", e)
	}
}

// TestHistoryCSVCarriesFillsWithheldReason: the CSV form lists the JSON's flags in X-StellarIndex-Flags, so a page with fills
// removed says why there too; an untouched page lists no reason.
func TestHistoryCSVCarriesFillsWithheldReason(t *testing.T) {
	soroswapWide := []timescale.SourceExclusion{{ID: 1, Source: "soroswap", From: time.Unix(1_000_000_000, 0)}}
	csvFlags := func(rows []canonical.Trade) string {
		srv := v1.New(v1.Options{
			History:    &pagedHistoryReader{rows: rows},
			Exclusions: pricingguard.NewSourceExclusionGate(exclTable{rows: soroswapWide}, pricingguard.SourceExclusionGateOptions{}),
		})
		ts := startHTTPTest(t, srv.Handler())
		req, _ := http.NewRequest(http.MethodGet, ts.URL+"/v1/history?base=native&quote=fiat:USD&from=2026-01-01T00:00:00Z&to=2027-01-01T00:00:00Z", nil)
		req.Header.Set("Accept", "text/csv")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("csv status %d", resp.StatusCode)
		}
		return resp.Header.Get("X-StellarIndex-Flags")
	}
	if got := csvFlags(histFills(3, 1)); got != "degraded, fills_withheld_reason=source_excluded" {
		t.Fatalf("X-StellarIndex-Flags = %q, want the fills-withheld reason listed", got)
	}
	if got := csvFlags(histFills(3)); got != "" {
		t.Fatalf("untouched page X-StellarIndex-Flags = %q, want none", got)
	}
}

func equalLedgers(got []uint32, want ...uint32) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
