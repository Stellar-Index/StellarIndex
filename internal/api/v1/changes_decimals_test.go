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

// /v1/changes published the rollup's RAW ratios as absolute money values.
// For a confirmed 9-decimals base every one of them was 100x low, beside a
// /v1/price that normalises the very same bucket. The percentages are
// scale-free (both legs of each come from one raw series) and must NOT
// move.
//
// The inputs are chosen so a naive float multiply would be caught too:
// 1.15×100, 0.07×100 and 4.35×100 are all inexact in binary floating
// point (114.99999999999999, 7.000000000000001, 434.99999999999994).
func TestHandleChangeSummary_NormalisesNonstandardDecimals(t *testing.T) {
	pairID := flaggedAsset + "/native"
	for _, tc := range []struct{ entityType, entityID string }{
		{"pair", pairID},
		{"coin", flaggedAsset},
	} {
		t.Run(tc.entityType, func(t *testing.T) {
			row := rawChangeSummaryRow(tc.entityType, tc.entityID)
			srv := v1.New(v1.Options{
				ChangeSummary:       &stubChangeSummaryReader{row: row},
				NonstandardDecimals: nonstandardDecimalsCacheWith(t, flaggedAsset, 9),
			})
			got := getChangeSummary(t, srv, tc.entityType, tc.entityID)

			if got.CurrentValue != "115" {
				t.Errorf("current_value = %q, want \"115\"", got.CurrentValue)
			}
			wantMoney(t, "h1_value", got.H1Value, "114")
			wantMoney(t, "h24_value", got.H24Value, "4132")
			wantMoney(t, "d7_value", got.D7Value, "7")
			wantMoney(t, "ath_value", got.ATHValue, "435")
			wantMoney(t, "atl_value", got.ATLValue, "7")
			if got.D30Value != nil {
				t.Errorf("d30_value = %q, want it to stay absent", *got.D30Value)
			}

			// Scale-free fields pass through untouched.
			if got.H1DeltaPct == nil || *got.H1DeltaPct != *row.H1DeltaPct {
				t.Errorf("h1_delta_pct = %v, want %v unchanged", got.H1DeltaPct, *row.H1DeltaPct)
			}
			if got.H24DeltaPct == nil || *got.H24DeltaPct != *row.H24DeltaPct {
				t.Errorf("h24_delta_pct = %v, want %v unchanged", got.H24DeltaPct, *row.H24DeltaPct)
			}
			if got.D7DeltaPct == nil || *got.D7DeltaPct != *row.D7DeltaPct {
				t.Errorf("d7_delta_pct = %v, want %v unchanged", got.D7DeltaPct, *row.D7DeltaPct)
			}
			if got.ATHAt != "2026-09-01T12:00:00Z" || got.ATLAt != "2026-08-25T03:00:00Z" {
				t.Errorf("ath_at/atl_at = %q/%q, want them carried through", got.ATHAt, got.ATLAt)
			}
			if got.StreakDirection != "up" || got.Acceleration != "accelerating" {
				t.Errorf("streak/acceleration = %q/%q, want them carried through", got.StreakDirection, got.Acceleration)
			}
		})
	}
}

// A flagged QUOTE leg scales the other way, and the factor must come from
// the pair the row was computed on.
func TestHandleChangeSummary_NormalisesFlaggedQuoteLeg(t *testing.T) {
	pairID := "native/" + flaggedAsset
	srv := v1.New(v1.Options{
		ChangeSummary:       &stubChangeSummaryReader{row: rawChangeSummaryRow("pair", pairID)},
		NonstandardDecimals: nonstandardDecimalsCacheWith(t, flaggedAsset, 9),
	})
	got := getChangeSummary(t, srv, "pair", pairID)
	if got.CurrentValue != "0.0115" {
		t.Errorf("current_value = %q, want \"0.0115\"", got.CurrentValue)
	}
	wantMoney(t, "h24_value", got.H24Value, "0.4132")
	wantMoney(t, "ath_value", got.ATHValue, "0.0435")
}

// With the table populated for a DIFFERENT asset, an ordinary row is
// byte-identical to what it has always been.
func TestHandleChangeSummary_SevenDecimalsByteIdentical(t *testing.T) {
	const pairID = "crypto:XLM/fiat:USD"
	for _, tc := range []struct{ entityType, entityID string }{
		{"pair", pairID},
		{"coin", "crypto:XLM"},
	} {
		t.Run(tc.entityType, func(t *testing.T) {
			srv := v1.New(v1.Options{
				ChangeSummary:       &stubChangeSummaryReader{row: rawChangeSummaryRow(tc.entityType, tc.entityID)},
				NonstandardDecimals: nonstandardDecimalsCacheWith(t, flaggedAsset, 9),
			})
			got := getChangeSummary(t, srv, tc.entityType, tc.entityID)
			if got.CurrentValue != "1.15" {
				t.Errorf("current_value = %q, want \"1.15\"", got.CurrentValue)
			}
			wantMoney(t, "h1_value", got.H1Value, "1.14")
			wantMoney(t, "h24_value", got.H24Value, "41.32")
			wantMoney(t, "d7_value", got.D7Value, "0.07")
			wantMoney(t, "ath_value", got.ATHValue, "4.35")
			wantMoney(t, "atl_value", got.ATLValue, "0.07")
		})
	}
}
