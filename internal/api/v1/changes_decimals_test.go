package v1_test

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

func f64(v float64) *float64 { return &v }

func moneyStrPtr(v string) *string { return &v }

// rawChangeSummaryRow is what the rollup worker stores: prices_1m's RAW
// ratio copied through, for a market whose raw ratio sits around 1.15.
func rawChangeSummaryRow(entityType, entityID string) timescale.ChangeSummaryRow {
	ath := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	atl := time.Date(2026, 8, 25, 3, 0, 0, 0, time.UTC)
	return timescale.ChangeSummaryRow{
		EntityType:      entityType,
		EntityID:        entityID,
		RefreshedAt:     time.Date(2026, 9, 18, 10, 0, 0, 0, time.UTC),
		CurrentValue:    "1.15",
		H1Value:         moneyStrPtr("1.14"),
		H1DeltaPct:      f64(0.8771929824561403),
		H24Value:        moneyStrPtr("41.32"),
		H24DeltaPct:     f64(-97.21684414327202),
		D7Value:         moneyStrPtr("0.07"),
		D7DeltaPct:      f64(1542.857142857143),
		D30Value:        nil,
		D30DeltaPct:     nil,
		ATHValue:        moneyStrPtr("4.35"),
		ATHAt:           &ath,
		ATLValue:        moneyStrPtr("0.07"),
		ATLAt:           &atl,
		StreakDirection: "up",
		Acceleration:    "accelerating",
	}
}

func getChangeSummary(t *testing.T, srv *v1.Server, entityType, entityID string) v1.ChangeSummaryResponse {
	t.Helper()
	ts := startHTTPTest(t, srv.Handler())
	resp := mustGet(t, ts.URL+"/v1/changes/"+entityType+"/"+url.PathEscape(entityID))
	body, _ := readAll(resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", resp.StatusCode, body)
	}
	var env struct {
		Data v1.ChangeSummaryResponse `json:"data"`
	}
	if err := json.NewDecoder(strings.NewReader(body)).Decode(&env); err != nil {
		t.Fatalf("decode: %v (body=%s)", err, body)
	}
	return env.Data
}

func wantMoney(t *testing.T, field string, got *string, want string) {
	t.Helper()
	if got == nil {
		t.Errorf("%s absent, want %s", field, want)
		return
	}
	if *got != want {
		t.Errorf("%s = %q, want %q", field, *got, want)
	}
}
