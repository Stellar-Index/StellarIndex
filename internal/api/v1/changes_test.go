package v1_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// stubChangeSummaryReader is the in-memory test seam.
type stubChangeSummaryReader struct {
	row timescale.ChangeSummaryRow
	err error

	lastEntityType, lastEntityID string
}

func (r *stubChangeSummaryReader) GetChangeSummary(_ context.Context, entityType, entityID string) (timescale.ChangeSummaryRow, error) {
	r.lastEntityType = entityType
	r.lastEntityID = entityID
	if r.err != nil {
		return timescale.ChangeSummaryRow{}, r.err
	}
	return r.row, nil
}

// keyedChangeSummaryReader returns a row only for entity_ids present
// in its map; every other id yields sql.ErrNoRows. Lets a test prove
// the handler's candidate expansion reaches a specific canonical form.
type keyedChangeSummaryReader struct {
	rows map[string]timescale.ChangeSummaryRow
	seen []string // entity_ids queried, in order
}

func (r *keyedChangeSummaryReader) GetChangeSummary(_ context.Context, _, entityID string) (timescale.ChangeSummaryRow, error) {
	r.seen = append(r.seen, entityID)
	if row, ok := r.rows[entityID]; ok {
		return row, nil
	}
	return timescale.ChangeSummaryRow{}, sql.ErrNoRows
}

// TestHandleChangeSummary_ResolvesXLMSACForm is the proven-red
// guard: when the change-summary worker has written the XLM rollup
// only under the SAC C-address (a Soroban-sourced row), a caller
// asking for /v1/changes/coin/native must still resolve it. Without the SAC form
// the candidate set for `native` would be [native, crypto:XLM] — the SAC
// form omitted, so the lookup would 404.
func TestHandleChangeSummary_ResolvesXLMSACForm(t *testing.T) {
	reader := &keyedChangeSummaryReader{
		rows: map[string]timescale.ChangeSummaryRow{
			canonical.XLMSacContractID: {
				EntityType:   "coin",
				EntityID:     canonical.XLMSacContractID,
				RefreshedAt:  time.Date(2026, 7, 3, 22, 38, 0, 0, time.UTC),
				CurrentValue: "0.1234",
			},
		},
	}
	srv := v1.New(v1.Options{ChangeSummary: reader})
	ts := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, ts.URL+"/v1/changes/coin/native")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (the SAC form must be a candidate)", resp.StatusCode)
	}
	var env struct {
		Data v1.ChangeSummaryResponse `json:"data"`
	}
	body, _ := readAll(resp)
	if err := json.NewDecoder(strings.NewReader(body)).Decode(&env); err != nil {
		t.Fatalf("decode: %v (body=%s)", err, body)
	}
	if env.Data.CurrentValue != "0.1234" {
		t.Errorf("CurrentValue = %q, want \"0.1234\" (served from the SAC-keyed rollup)", env.Data.CurrentValue)
	}
	// Money-safety: the SAC form must be the LAST candidate tried, so a
	// thin Soroban rollup never shadows a native/crypto:XLM row.
	if len(reader.seen) == 0 || reader.seen[len(reader.seen)-1] != canonical.XLMSacContractID {
		t.Errorf("candidate order = %v, want the SAC form tried last", reader.seen)
	}
}

// TestHandleChangeSummary_503WhenReaderNil — feature-gated reader.
// Returns 503 with `change-summary-unavailable` so the explorer
// can hide the change-summary panel rather than render zeroes.
func TestHandleChangeSummary_503WhenReaderNil(t *testing.T) {
	srv := v1.New(v1.Options{})
	ts := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, ts.URL+"/v1/changes/coin/XLM")
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", resp.StatusCode)
	}
	body, _ := readAll(resp)
	if !strings.Contains(body, "change-summary-unavailable") {
		t.Errorf("expected error type tag in body: %s", body)
	}
}

// TestHandleChangeSummary_InvalidEntityType400 — reject anything
// outside the served {coin,pair} families upstream of the storage
// layer, so an operator typo gets a clean 400.
func TestHandleChangeSummary_InvalidEntityType400(t *testing.T) {
	srv := v1.New(v1.Options{ChangeSummary: &stubChangeSummaryReader{}})
	ts := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, ts.URL+"/v1/changes/banana/XLM")
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
	body, _ := readAll(resp)
	if !strings.Contains(body, "invalid-entity-type") {
		t.Errorf("expected invalid-entity-type tag: %s", body)
	}
}

// TestHandleChangeSummary_UnservedEntityType400 — `protocol` and
// `source` are reserved by the change_summary_5m CHECK but no worker
// computes them (a protocol sums across pools, a source across
// assets; neither reduces to the single canonical pair the rollup
// keys on). Accepting them returned 404 "the worker hasn't computed
// a row for this entity yet" — indistinguishable from worker lag, on
// families that will never have a row. They must be rejected at the
// boundary, and the 400 detail must not advertise them back.
func TestHandleChangeSummary_UnservedEntityType400(t *testing.T) {
	cases := map[string]string{"protocol": "soroswap", "source": "binance"}
	for entityType, entityID := range cases {
		t.Run(entityType, func(t *testing.T) {
			reader := &stubChangeSummaryReader{err: sql.ErrNoRows}
			srv := v1.New(v1.Options{ChangeSummary: reader})
			ts := startHTTPTest(t, srv.Handler())

			resp := mustGet(t, ts.URL+"/v1/changes/"+entityType+"/"+entityID)
			if resp.StatusCode != http.StatusBadRequest {
				t.Errorf("status = %d, want 400", resp.StatusCode)
			}
			body, _ := readAll(resp)
			var problem struct {
				Type   string `json:"type"`
				Detail string `json:"detail"`
			}
			if err := json.Unmarshal([]byte(body), &problem); err != nil {
				t.Fatalf("decode problem: %v (body=%s)", err, body)
			}
			if problem.Type != "https://api.stellarindex.io/errors/invalid-entity-type" {
				t.Errorf("problem type = %q, want invalid-entity-type", problem.Type)
			}
			// The detail is the only place a caller learns the served
			// set; naming a family with no producer sends them back
			// down the same dead end.
			if strings.Contains(problem.Detail, "protocol") || strings.Contains(problem.Detail, "source") {
				t.Errorf("400 detail advertises an unserved family: %q", problem.Detail)
			}
			if reader.lastEntityType != "" {
				t.Errorf("storage was queried for an unserved family: %q", reader.lastEntityType)
			}
		})
	}
}

// TestHandleChangeSummary_PairIDPercentEncoded — a `pair` entity_id
// carries a `/` inside it (`base/quote`), so the whole id has to
// arrive percent-encoded in one path segment. Pins that the router
// hands the handler the DECODED id, which is what the worker keyed
// the row under; without the encoding the request would split across
// two segments and match no route, and the SDK relies on this by
// escaping the id for callers.
func TestHandleChangeSummary_PairIDPercentEncoded(t *testing.T) {
	reader := &stubChangeSummaryReader{
		row: timescale.ChangeSummaryRow{
			EntityType:   "pair",
			EntityID:     "crypto:XLM/fiat:USD",
			RefreshedAt:  time.Now().UTC(),
			CurrentValue: "0.1675",
		},
	}
	srv := v1.New(v1.Options{ChangeSummary: reader})
	ts := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, ts.URL+"/v1/changes/pair/crypto%3AXLM%2Ffiat%3AUSD")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if reader.lastEntityType != "pair" || reader.lastEntityID != "crypto:XLM/fiat:USD" {
		t.Errorf("reader saw (%q, %q), want (pair, crypto:XLM/fiat:USD)",
			reader.lastEntityType, reader.lastEntityID)
	}
}

// TestHandleChangeSummary_NotFound404 — the worker hasn't computed
// a row yet (or the entity was added after the last refresh).
// Surfaces as 404 so the explorer renders an empty state rather
// than a confusing 500.
func TestHandleChangeSummary_NotFound404(t *testing.T) {
	reader := &stubChangeSummaryReader{err: sql.ErrNoRows}
	srv := v1.New(v1.Options{ChangeSummary: reader})
	ts := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, ts.URL+"/v1/changes/coin/XLM")
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
	body, _ := readAll(resp)
	if !strings.Contains(body, "change-summary-not-found") {
		t.Errorf("expected change-summary-not-found tag: %s", body)
	}
}

// TestHandleChangeSummary_HappyPath_Coin — full row decode pin.
// All four time-window slots populate; ATH/ATL with At fields
// formatted as RFC3339; nullable pointer fields surface verbatim.
func TestHandleChangeSummary_HappyPath_Coin(t *testing.T) {
	athAt := time.Date(2026, 5, 4, 12, 0, 0, 0, time.UTC)
	atlAt := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	h1, h24, d7, d30 := "0.165", "0.158", "0.150", "0.142"
	hd1, hd24, dd7, dd30 := 1.5, 5.2, 10.1, 16.7
	ath, atl := "1.03", "0.10"
	streakDays := 3

	reader := &stubChangeSummaryReader{
		row: timescale.ChangeSummaryRow{
			EntityType:      "coin",
			EntityID:        "XLM",
			RefreshedAt:     time.Date(2026, 5, 9, 10, 0, 0, 0, time.UTC),
			CurrentValue:    "0.1675",
			H1Value:         &h1,
			H1DeltaPct:      &hd1,
			H24Value:        &h24,
			H24DeltaPct:     &hd24,
			D7Value:         &d7,
			D7DeltaPct:      &dd7,
			D30Value:        &d30,
			D30DeltaPct:     &dd30,
			ATHValue:        &ath,
			ATHAt:           &athAt,
			ATLValue:        &atl,
			ATLAt:           &atlAt,
			StreakDirection: "up",
			StreakDays:      &streakDays,
			Acceleration:    "steady",
		},
	}
	srv := v1.New(v1.Options{ChangeSummary: reader})
	ts := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, ts.URL+"/v1/changes/coin/XLM")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var env struct {
		Data v1.ChangeSummaryResponse `json:"data"`
	}
	body, _ := readAll(resp)
	if err := json.NewDecoder(strings.NewReader(body)).Decode(&env); err != nil {
		t.Fatalf("decode: %v (body=%s)", err, body)
	}
	d := env.Data
	if d.EntityType != "coin" || d.EntityID != "XLM" {
		t.Errorf("entity = (%q, %q)", d.EntityType, d.EntityID)
	}
	if d.CurrentValue != "0.1675" {
		t.Errorf("CurrentValue = %q, want \"0.1675\" (M7: money is a string)", d.CurrentValue)
	}
	if d.H24DeltaPct == nil || *d.H24DeltaPct != 5.2 {
		t.Errorf("H24DeltaPct = %v, want 5.2 (percentage stays a number)", d.H24DeltaPct)
	}
	if d.ATHValue == nil || *d.ATHValue != "1.03" {
		t.Errorf("ATHValue = %v, want \"1.03\"", d.ATHValue)
	}
	if d.ATHAt != "2026-05-04T12:00:00Z" {
		t.Errorf("ATHAt = %q (RFC3339 format pin)", d.ATHAt)
	}
	if d.StreakDirection != "up" || d.StreakDays == nil || *d.StreakDays != 3 {
		t.Errorf("streak = (%q, %v)", d.StreakDirection, d.StreakDays)
	}

	// Verify the handler threaded entity_type + id correctly to the
	// storage layer (regression: a swap would render the wrong row).
	if reader.lastEntityType != "coin" || reader.lastEntityID != "XLM" {
		t.Errorf("reader saw (%q, %q), want (coin, XLM)", reader.lastEntityType, reader.lastEntityID)
	}
}

// TestHandleChangeSummary_NullableFieldsOmitted — a young entity
// with <1h of history has every window pointer NULL. omitempty
// drops them from the wire so the explorer can branch on absence.
func TestHandleChangeSummary_NullableFieldsOmitted(t *testing.T) {
	reader := &stubChangeSummaryReader{
		row: timescale.ChangeSummaryRow{
			EntityType: "coin",
			// A canonical id, as the worker writes: an id naming no
			// market cannot be vetted and is withheld.
			EntityID:     "native",
			RefreshedAt:  time.Now().UTC(),
			CurrentValue: "1.0",
			// H1/H24/D7/D30/ATH/ATL all nil — fresh asset
		},
	}
	srv := v1.New(v1.Options{ChangeSummary: reader})
	ts := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, ts.URL+"/v1/changes/coin/FRESH")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	body, _ := readAll(resp)
	for _, forbidden := range []string{
		`"h1_delta_pct"`,
		`"h24_delta_pct"`,
		`"d7_delta_pct"`,
		`"d30_delta_pct"`,
		`"ath_value"`,
		`"ath_at"`,
		`"atl_value"`,
		`"atl_at"`,
		`"streak_days"`,
	} {
		if strings.Contains(body, forbidden) {
			t.Errorf("body should NOT contain %q (omitempty broken): %s", forbidden, body)
		}
	}
}

// TestHandleChangeSummary_ReaderError500 — unwrapped storage error
// surfaces as 500 with `change-summary-error`. Distinct from
// sql.ErrNoRows (which is the 404 path).
func TestHandleChangeSummary_ReaderError500(t *testing.T) {
	reader := &stubChangeSummaryReader{err: errors.New("storage broke")}
	srv := v1.New(v1.Options{ChangeSummary: reader})
	ts := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, ts.URL+"/v1/changes/coin/XLM")
	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", resp.StatusCode)
	}
	body, _ := readAll(resp)
	if !strings.Contains(body, "change-summary-error") {
		t.Errorf("expected change-summary-error tag: %s", body)
	}
}

// TestHandleChangeSummary_StaleFlag pins flags.stale to row age against
// the openapi contract (5-minute refresh, >10min lagging): a fresh row
// serves stale=false, a row well past the 10-minute line serves
// stale=true even though the data itself is otherwise valid.
func TestHandleChangeSummary_StaleFlag(t *testing.T) {
	cases := []struct {
		name        string
		refreshedAt time.Time
		wantStale   bool
	}{
		{"fresh", time.Now().UTC().Add(-1 * time.Minute), false},
		{"lagging", time.Now().UTC().Add(-30 * time.Minute), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reader := &stubChangeSummaryReader{
				row: timescale.ChangeSummaryRow{
					EntityType:   "coin",
					EntityID:     "XLM",
					RefreshedAt:  tc.refreshedAt,
					CurrentValue: "0.1675",
				},
			}
			srv := v1.New(v1.Options{ChangeSummary: reader})
			ts := startHTTPTest(t, srv.Handler())

			resp := mustGet(t, ts.URL+"/v1/changes/coin/XLM")
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want 200", resp.StatusCode)
			}
			var env struct {
				Flags struct {
					Stale bool `json:"stale"`
				} `json:"flags"`
			}
			body, _ := readAll(resp)
			if err := json.NewDecoder(strings.NewReader(body)).Decode(&env); err != nil {
				t.Fatalf("decode: %v (body=%s)", err, body)
			}
			if env.Flags.Stale != tc.wantStale {
				t.Errorf("flags.stale = %v, want %v (refreshed_at age)", env.Flags.Stale, tc.wantStale)
			}
		})
	}
}

// TestChangeSummary_MoneyFieldsAreJSONStrings is the money-as-string guard: the
// /v1/changes *_value fields are MONEY and must serialize as JSON STRINGS
// (like every other money field the API serves), while the *_delta_pct
// PERCENTAGE fields stay JSON numbers. The stub feeds the rollup's exact
// decimal row values through the handler unchanged.
func TestChangeSummary_MoneyFieldsAreJSONStrings(t *testing.T) {
	h1, h24 := "0.20380247911865504", "0.19673099518995452"
	hd24 := 3.784588530467602
	ath := "0.29758550057923283"
	reader := &stubChangeSummaryReader{
		row: timescale.ChangeSummaryRow{
			EntityType:   "coin",
			EntityID:     "XLM",
			RefreshedAt:  time.Date(2026, 7, 3, 22, 38, 0, 0, time.UTC),
			CurrentValue: "0.2041764538697883",
			H1Value:      &h1,
			H24Value:     &h24,
			H24DeltaPct:  &hd24,
			ATHValue:     &ath,
		},
	}
	srv := v1.New(v1.Options{ChangeSummary: reader})
	ts := startHTTPTest(t, srv.Handler())
	resp := mustGet(t, ts.URL+"/v1/changes/coin/XLM")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, _ := readAll(resp)

	// Money → quoted strings (shortest round-tripping decimal).
	for _, want := range []string{
		`"current_value":"0.2041764538697883"`,
		`"h1_value":"0.20380247911865504"`,
		`"h24_value":"0.19673099518995452"`,
		`"ath_value":"0.29758550057923283"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing quoted money field %s\n(INV-2: money must be a JSON string)\nbody=%s", want, body)
		}
	}
	// Percentages stay unquoted numbers (NOT strings).
	if !strings.Contains(body, `"h24_delta_pct":3.784588530467602`) {
		t.Errorf("h24_delta_pct should be an unquoted number (percentage, not money)\nbody=%s", body)
	}
	if strings.Contains(body, `"h24_delta_pct":"`) {
		t.Errorf("h24_delta_pct was serialized as a string — percentages are not money\nbody=%s", body)
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

// /v1/changes published current_value, the window values and the 30-day
// ATH/ATL for a market /v1/price withholds: no scam gate and no substance
// gate stood between change_summary_5m and the wire. Every value
// on the row is an aggregated price claim for that market.
func TestHandleChangeSummary_WithholdsWhatPriceWithholds(t *testing.T) {
	flagged := chartFlaggedBase(t).String()
	cases := []struct {
		name, entityType, entityID string
		opts                       func() v1.Options
		wantSurface                string
		wantTitle                  string
	}{
		{
			name: "scam-flagged base, pair row", entityType: "pair", entityID: flagged + "/native",
			opts: func() v1.Options {
				return v1.Options{Scam: &chartScamGate{withheld: map[string]bool{flagged: true}}}
			},
			wantTitle: "issuer flagged",
		},
		{
			name: "scam-flagged base, coin row", entityType: "coin", entityID: flagged,
			opts: func() v1.Options {
				return v1.Options{Scam: &chartScamGate{withheld: map[string]bool{flagged: true}}}
			},
			wantTitle: "issuer flagged",
		},
		{
			name: "thin market, pair row", entityType: "pair", entityID: flagged + "/native",
			opts:        func() v1.Options { return v1.Options{Substance: &stubSubstanceGate{allow: false}} },
			wantSurface: "change_summary",
			wantTitle:   "market too thin",
		},
		{
			name: "thin market on every backing pair, coin row", entityType: "coin", entityID: flagged,
			opts:      func() v1.Options { return v1.Options{Substance: &stubSubstanceGate{allow: false}} },
			wantTitle: "market too thin",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			opts := tc.opts()
			opts.ChangeSummary = &stubChangeSummaryReader{row: rawChangeSummaryRow(tc.entityType, tc.entityID)}
			status, body := getChangeSummaryStatus(t, v1.New(opts), tc.entityType, tc.entityID)
			if status != http.StatusNotFound {
				t.Fatalf("status = %d, want 404 withheld (body=%s)", status, body)
			}
			if !strings.Contains(body, "errors/price-withheld") || !strings.Contains(body, tc.wantTitle) {
				t.Errorf("body = %s, want the price-withheld problem naming %q", body, tc.wantTitle)
			}
			if strings.Contains(body, "current_value") {
				t.Errorf("withheld body still carries the row: %s", body)
			}
			if tc.wantSurface != "" {
				gate := opts.Substance.(*stubSubstanceGate)
				if len(gate.surfaces) == 0 || gate.surfaces[0] != tc.wantSurface {
					t.Errorf("substance gate asked with surfaces %v, want %q", gate.surfaces, tc.wantSurface)
				}
			}
		})
	}
}

// Gates that clear the market leave the response exactly as before.
func TestHandleChangeSummary_ServesWhenGatesAllow(t *testing.T) {
	flagged := chartFlaggedBase(t).String()
	for _, entityType := range []string{"pair", "coin"} {
		id := flagged
		if entityType == "pair" {
			id += "/native"
		}
		srv := v1.New(v1.Options{
			ChangeSummary: &stubChangeSummaryReader{row: rawChangeSummaryRow(entityType, id)},
			Substance:     &stubSubstanceGate{allow: true},
			Scam:          &chartScamGate{withheld: map[string]bool{}},
		})
		got := getChangeSummary(t, srv, entityType, id)
		if got.CurrentValue != "1.15" {
			t.Errorf("%s: current_value = %q, want \"1.15\"", entityType, got.CurrentValue)
		}
	}
}
