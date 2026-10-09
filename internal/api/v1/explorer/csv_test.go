package explorer

import (
	"encoding/csv"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/api/v1/middleware"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

func TestPrefersCSV(t *testing.T) {
	cases := map[string]bool{
		"":                 false,
		"application/json": false,
		"*/*":              false,
		"text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8": false,
		"text/csv":                         true,
		"text/csv; charset=utf-8":          true,
		"text/csv, */*":                    true,
		"text/csv;q=0.5, */*":              false,
		"text/csv, application/json":       false,
		"text/csv, application/json;q=0.9": true,
		"application/json;q=0.5, text/csv": true,
		"text/csv;q=0":                     false,
		"text/csv;q=bogus":                 false,
	}
	for accept, want := range cases {
		if got := prefersCSV(accept); got != want {
			t.Errorf("prefersCSV(%q) = %v, want %v", accept, got, want)
		}
	}
}

func TestCSVSafeCell(t *testing.T) {
	cases := map[string]string{
		"=HYPERLINK(\"http://x\",\"y\")": "'=HYPERLINK(\"http://x\",\"y\")",
		"+1+cmd|' /C calc'!A0":           "'+1+cmd|' /C calc'!A0",
		"-2+3":                           "'-2+3",
		"@SUM(A1)":                       "'@SUM(A1)",
		"\t=1":                           "'\t=1",
		"\r=1":                           "'\r=1",
		"＝1+1":                           "'＝1+1",
		"-170141183460469231731687303715884105728": "-170141183460469231731687303715884105728",
		"1267650600228229401496703205376":          "1267650600228229401496703205376",
		"-1.5":                                     "-1.5",
		"":                                         "",
		"native":                                   "native",
		"transfer":                                 "transfer",
		`{"memo":"=1+1"}`:                          `{"memo":"=1+1"}`,
	}
	for in, want := range cases {
		if got := csvSafeCell(in); got != want {
			t.Errorf("csvSafeCell(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestHeaderSafe(t *testing.T) {
	cases := map[string]string{
		"plain note; ledger 5":                               "plain note; ledger 5",
		"P23 on — transfers only":                            "P23 on - transfers only",
		"x\r\nSet-Cookie: y":                                 "x Set-Cookie: y",
		"a\tb\x00c\x7fd\u0085e":                              "a b c d e",
		"\u201cq\u201d \u2018s\u2019 \u2026 \u22651 \u22642": `"q" 's' ... >=1 <=2`,
		"bad\xffutf8 €":                                      "bad?utf8 ?",
		"\u2028line\u2029sep\u00a0nbsp ":                     "?line?sep nbsp",
		"\r\n":                                               "",
	}
	for in, want := range cases {
		got := headerSafe(in)
		if got != want {
			t.Errorf("headerSafe(%q) = %q, want %q", in, got, want)
		}
		for i := 0; i < len(got); i++ {
			if got[i] < 0x20 || got[i] > 0x7e {
				t.Errorf("headerSafe(%q) byte %d = %#x, not printable ASCII", in, i, got[i])
			}
		}
	}
}

// csvMovementsHandler serves AssetMovements behind the real Cache-Control
// middleware so the test sees the headers a CDN would.
func csvMovementsHandler(reader *assetArmReader, limit int, jsonBody *AssetMovementsView) http.Handler {
	h := newProbeHandler(reader, nil)
	h.ParseLimit = func(http.ResponseWriter, *http.Request, int, int) (int, bool) { return limit, true }
	writeJSON := func(w http.ResponseWriter, v any) {
		*jsonBody = v.(AssetMovementsView)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
	}
	h.WriteJSON = func(w http.ResponseWriter, v any, _ bool) { writeJSON(w, v) }
	h.WriteJSONAt = func(w http.ResponseWriter, v any, _, _ bool, _ time.Time) { writeJSON(w, v) }
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/assets/{asset_id}/movements", h.AssetMovements)
	return middleware.CacheControlWithCDN(true)(mux)
}

func csvTestReader() (*assetArmReader, *big.Int) {
	wm := timescale.MovementsFloor() + 500
	huge := new(big.Int).Lsh(big.NewInt(1), 100) // > 2^63 and > 2^64
	when := time.Unix(1_700_000_000, 0).UTC()
	reader := newAssetArmReader(wm)
	reader.rows = []clickhouse.AssetMovementRow{
		{
			Ledger: wm, TxHash: "aa", OpIndex: 1, LegIndex: 2, MovementKind: "=cmd|' /C calc'!A0", Provenance: "cap67_derived",
			From: "GFROM", To: "GTO", Amount: huge, LedgerCloseTime: when,
			Attributes: map[string]any{"memo": "@evil"},
		},
		{Ledger: wm - 1, TxHash: "bb", MovementKind: "transfer", Amount: big.NewInt(3), LedgerCloseTime: when},
	}
	return reader, huge
}

func TestAssetMovementsCSV(t *testing.T) {
	reader, huge := csvTestReader()
	var unused AssetMovementsView
	srv := csvMovementsHandler(reader, 1, &unused)
	req := httptest.NewRequest(http.MethodGet, "/v1/assets/native/movements?limit=1", nil)
	req.Header.Set("Accept", "text/csv")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "text/csv; charset=utf-8" {
		t.Fatalf("code=%d content-type=%q, want 200 text/csv", rec.Code, rec.Header().Get("Content-Type"))
	}
	wm := timescale.MovementsFloor() + 500
	wantLink := fmt.Sprintf("</v1/assets/native/movements?cursor=%s&limit=1>; rel=\"next\"", url.QueryEscape(fmt.Sprintf("%d.aa.1.2", wm)))
	if got := rec.Header().Get("Link"); got != wantLink {
		t.Errorf("Link = %q, want %q", got, wantLink)
	}
	if got := rec.Header().Get("X-StellarIndex-Flags"); got != "lower_bound" {
		t.Errorf("X-StellarIndex-Flags = %q, want lower_bound (no backfill marker)", got)
	}
	if got := rec.Header().Get("X-StellarIndex-Through-Ledger"); got != fmt.Sprint(wm) {
		t.Errorf("X-StellarIndex-Through-Ledger = %q, want %d", got, wm)
	}
	// The lower bound names what it excludes: the JSON's coverage_note,
	// folded to ASCII (the note carries an em dash).
	var page AssetMovementsView
	jsonRec := httptest.NewRecorder()
	csvMovementsHandler(reader, 1, &page).ServeHTTP(jsonRec, httptest.NewRequest(http.MethodGet, "/v1/assets/native/movements?limit=1", nil))
	if !strings.Contains(page.CoverageNote, "—") {
		t.Fatalf("fixture note has no non-ASCII rune to fold: %q", page.CoverageNote)
	}
	if got, want := rec.Header().Get("X-StellarIndex-Coverage-Note"), strings.ReplaceAll(page.CoverageNote, "—", "-"); got != want {
		t.Errorf("X-StellarIndex-Coverage-Note = %q, want %q", got, want)
	}

	records, err := csv.NewReader(strings.NewReader(rec.Body.String())).ReadAll()
	if err != nil {
		t.Fatalf("body is not CSV: %v\n%s", err, rec.Body.String())
	}
	if len(records) != 2 || !reflect.DeepEqual(records[0], assetMovementsCSVColumns) {
		t.Fatalf("records=%q, want header + 1 row (row cap = limit)", records)
	}
	row := map[string]string{}
	for i, c := range records[0] {
		row[c] = records[1][i]
	}
	if row["amount"] != huge.String() {
		t.Errorf("amount = %q, want exact %s", row["amount"], huge)
	}
	if amt, ok := new(big.Int).SetString(row["amount"], 10); !ok || amt.Cmp(huge) != 0 {
		t.Errorf("amount %q does not round-trip to %s", row["amount"], huge)
	}
	want := map[string]string{
		"asset": "native", "tx_hash": "aa", "op_index": "1", "leg_index": "2", "decimals": "7",
		"from": "GFROM", "to": "GTO", "ledger_close_time": "2023-11-14T22:13:20Z",
		"movement_kind": "'=cmd|' /C calc'!A0", "attributes": `{"memo":"@evil"}`,
	}
	for k, v := range want {
		if row[k] != v {
			t.Errorf("%s = %q, want %q", k, row[k], v)
		}
	}
}

// JSON and CSV share one URL, so both must say they vary on Accept and the
// CSV must never enter a shared cache; JSON stays the default.
func TestAssetMovementsCSVAndJSONDoNotCrossServe(t *testing.T) {
	reader, _ := csvTestReader()
	reader.backfilledThru = 1
	var view AssetMovementsView
	srv := csvMovementsHandler(reader, 1, &view)
	get := func(accept string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/v1/assets/native/movements?limit=1", nil)
		if accept != "" {
			req.Header.Set("Accept", accept)
		}
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		return rec
	}

	var views []AssetMovementsView
	for _, accept := range []string{"", "application/json", "*/*"} {
		view = AssetMovementsView{}
		rec := get(accept)
		if rec.Header().Get("Content-Type") != "application/json" || view.Asset != "native" {
			t.Fatalf("Accept %q: content-type=%q view=%+v, want the JSON writer", accept, rec.Header().Get("Content-Type"), view)
		}
		if rec.Header().Get("Vary") != "Accept" {
			t.Errorf("Accept %q: JSON Vary = %q, want Accept", accept, rec.Header().Get("Vary"))
		}
		if !strings.HasPrefix(rec.Header().Get("Cache-Control"), "public") {
			t.Errorf("Accept %q: JSON Cache-Control = %q, want the route's public band unchanged", accept, rec.Header().Get("Cache-Control"))
		}
		views = append(views, view)
	}
	for _, v := range views[1:] {
		if !reflect.DeepEqual(v, views[0]) {
			t.Fatalf("JSON view differs by Accept: %+v vs %+v", v, views[0])
		}
	}

	rec := get("text/csv")
	if rec.Header().Get("Vary") != "Accept" || rec.Header().Get("Cache-Control") != "private, no-store" {
		t.Fatalf("CSV Vary=%q Cache-Control=%q, want Accept and private, no-store", rec.Header().Get("Vary"), rec.Header().Get("Cache-Control"))
	}
	if rec.Header().Get("X-StellarIndex-Flags") != "" {
		t.Errorf("X-StellarIndex-Flags = %q with the backfill marker set, want none", rec.Header().Get("X-StellarIndex-Flags"))
	}
}

func TestAssetHoldersCSV(t *testing.T) {
	reader := &holdersKeyReader{capReader: &capReader{probe: &deadlineProbe{}}}
	h := newProbeHandler(reader, nil)
	h.WriteJSONAt = func(http.ResponseWriter, any, bool, bool, time.Time) { t.Fatal("CSV request reached the JSON writer") }
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/assets/{asset_id}/holders", h.AssetHolders)
	req := httptest.NewRequest(http.MethodGet, "/v1/assets/native/holders", nil)
	req.Header.Set("Accept", "text/csv")
	rec := httptest.NewRecorder()
	middleware.CacheControlWithCDN(true)(mux).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "text/csv; charset=utf-8" {
		t.Fatalf("code=%d content-type=%q, want 200 text/csv", rec.Code, rec.Header().Get("Content-Type"))
	}
	if rec.Header().Get("Vary") != "Accept" || rec.Header().Get("Cache-Control") != "private, no-store" {
		t.Errorf("Vary=%q Cache-Control=%q, want Accept and private, no-store", rec.Header().Get("Vary"), rec.Header().Get("Cache-Control"))
	}
	if got := rec.Header().Get("X-StellarIndex-Holder-Count"); got != "9915982" {
		t.Errorf("X-StellarIndex-Holder-Count = %q, want 9915982", got)
	}
	records, err := csv.NewReader(strings.NewReader(rec.Body.String())).ReadAll()
	if err != nil {
		t.Fatalf("body is not CSV: %v\n%s", err, rec.Body.String())
	}
	want := [][]string{
		{"asset", "account_id", "balance"},
		// 554421152474348098 > 2^53: a float64 cell would read 554421152474348100.
		{"native", validTestAccount, "554421152474348098"},
	}
	if !reflect.DeepEqual(records, want) {
		t.Fatalf("records = %q, want %q", records, want)
	}
}
