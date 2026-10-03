package v1

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/holds"
)

const holdTestContract = "CAS3J7GYLGXMF6TDJBBYYSE3HQ6BBSMLNUQ34T6TZMYMW2EVH34XOWMA"

func holdTestHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{
			"asset_id":     holdTestContract,
			"total_supply": "170141183460469231731687303715884105727",
			"as_of_ledger": 500,
		}, Flags{})
	}
}

func holdGet(t *testing.T, s *Server) map[string]json.RawMessage {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/assets/{asset_id}/supply", s.underReview(holdTestHandler()))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/assets/"+holdTestContract+"/supply", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	var env map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	return env
}

func marked(env map[string]json.RawMessage) bool {
	var f map[string]any
	_ = json.Unmarshal(env["flags"], &f)
	_, hasReason := env["under_review_reason"]
	return f["under_review"] == true && hasReason
}

func TestUnderReviewHoldAddedAndRemovedWithinReload(t *testing.T) {
	s := &Server{}
	path := filepath.Join(t.TempDir(), "holds.toml")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go holds.Watch(ctx, path, 10*time.Millisecond, s.SetHolds, slog.New(slog.DiscardHandler))

	if marked(holdGet(t, s)) {
		t.Fatal("marked with no hold file")
	}

	doc := "[[hold]]\ncontract_id = \"" + holdTestContract + "\"\nledger_from = 400\nledger_to = 600\nreason = \"issuer under investigation\"\n"
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	waitHold(t, s, true)
	env := holdGet(t, s)
	if string(env["under_review_reason"]) != `"issuer under investigation"` {
		t.Fatalf("reason = %s", env["under_review_reason"])
	}
	var data map[string]json.RawMessage
	_ = json.Unmarshal(env["data"], &data)
	if string(data["total_supply"]) != `"170141183460469231731687303715884105727"` {
		t.Fatalf("supply altered: %s", data["total_supply"])
	}

	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	waitHold(t, s, false)
}

func TestUnderReviewKeepsHoldOnInvalidFile(t *testing.T) {
	s := &Server{}
	path := filepath.Join(t.TempDir(), "holds.toml")
	good := "[[hold]]\nasset = \"" + holdTestContract + "\"\nreason = \"r\"\n"
	if err := os.WriteFile(path, []byte(good), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go holds.Watch(ctx, path, 10*time.Millisecond, s.SetHolds, slog.New(slog.DiscardHandler))
	waitHold(t, s, true)

	if err := os.WriteFile(path, []byte("[[hold]]\nreeason = \"typo\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	if !marked(holdGet(t, s)) {
		t.Fatal("invalid file lifted the hold")
	}
}

func TestUnderReviewLedgerRangeMiss(t *testing.T) {
	s := &Server{}
	s.SetHolds([]holds.Hold{{ContractID: holdTestContract, LedgerFrom: 600, Reason: "r"}})
	if marked(holdGet(t, s)) {
		t.Fatal("response at ledger 500 marked by a hold starting at 600")
	}
}

func TestUnderReviewNativeHoldCoversPathSpellings(t *testing.T) {
	s := &Server{}
	s.SetHolds([]holds.Hold{{Asset: "native", Reason: "r"}})
	h := s.underReview(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{"total_supply": "1", "as_of_ledger": 500}, Flags{})
	})
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/assets/{asset_id}/supply", h)
	for _, p := range []string{"XLM", "xlm", "Native", "native", "crypto:XLM", holdTestContract} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/assets/"+p+"/supply", nil))
		var env map[string]json.RawMessage
		if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
			t.Fatal(err)
		}
		if !marked(env) {
			t.Errorf("path %q served without under_review", p)
		}
	}
}

func waitHold(t *testing.T, s *Server, want bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if marked(holdGet(t, s)) == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("under_review never became %v", want)
}

func TestUnderReviewAssetDetailUsesSupplyLedger(t *testing.T) {
	const usdc = "USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
	list, err := holds.Parse([]byte("[[hold]]\nasset = \"" + usdc + "\"\nledger_from = 100\nledger_to = 900\nreason = \"r\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{}
	s.SetHolds(list)
	for _, tc := range []struct {
		ledger uint32
		want   bool
	}{{500, true}, {1000, false}} {
		l := tc.ledger
		detail := AssetDetail{AssetID: usdc, Type: "classic", Code: "USDC", SupplyAsOfLedger: &l}
		mux := http.NewServeMux()
		mux.HandleFunc("GET /v1/assets/{asset_id}", s.underReview(func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, detail, Flags{})
		}))
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/assets/"+usdc, nil))
		var body map[string]json.RawMessage
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if got := marked(body); got != tc.want {
			t.Errorf("supply_as_of_ledger=%d: marked=%v want %v", tc.ledger, got, tc.want)
		}
	}
}
