package frankfurter

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestRangeUSDRates_parsesPayload(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
		  "base":"USD",
		  "start_date":"2024-01-02",
		  "end_date":"2024-01-04",
		  "rates":{
		    "2024-01-02":{"EUR":0.91,"CNY":7.12,"JPY":141.5},
		    "2024-01-03":{"EUR":0.92,"CNY":7.15,"JPY":143.0},
		    "2024-01-04":{"EUR":0.92,"CNY":-1,"JPY":143.2}
		  }
		}`))
	}))
	defer srv.Close()

	c := NewClient().WithBase(srv.URL)
	from, _ := time.Parse("2006-01-02", "2024-01-02")
	to, _ := time.Parse("2006-01-02", "2024-01-04")

	days, err := c.RangeUSDRates(context.Background(), from, to)
	if err != nil {
		t.Fatalf("RangeUSDRates: %v", err)
	}
	if len(days) != 3 {
		t.Fatalf("want 3 days, got %d", len(days))
	}
	// Sorted ascending
	if !days[0].Date.Equal(from) {
		t.Errorf("days[0]=%v, want %v", days[0].Date, from)
	}
	// Skips non-positive rates (CNY -1 on 2024-01-04 dropped)
	if _, ok := days[2].Rates["CNY"]; ok {
		t.Errorf("days[2] should have dropped non-positive CNY")
	}
	// Upper-cased codes
	if v := days[1].Rates["EUR"]; v != 0.92 {
		t.Errorf("days[1].EUR=%v, want 0.92", v)
	}
}
