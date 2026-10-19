package v1

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// stubSEP41TransfersReader is a no-op reader for exercising request-side validation without a database; a case
// that fails to short-circuit reaches it and the test sees calledN > 0.
type stubSEP41TransfersReader struct {
	calledN int
}

func (r *stubSEP41TransfersReader) ListSEP41Transfers(
	_ context.Context, _, _, _ string, _ int,
) ([]timescale.SEP41TransferRow, time.Time, error) {
	r.calledN++
	return nil, time.Time{}, nil
}

func serverWithSEP41Reader(reader SEP41TransfersReader) *Server {
	s := &Server{}
	s.SEP41Transfers = reader
	return s
}

// TestSEP41Transfers_LadderBudgetLeavesTheFallbackAtLeastHalf pins the store's lookback-ladder budget premise: the
// handler deadline is the read's only budget, the ladder may spend at most half, the rest is the full-history
// fallback's. The store can't import this package, so the two constants meet only here.
func TestSEP41Transfers_LadderBudgetLeavesTheFallbackAtLeastHalf(t *testing.T) {
	if 2*timescale.SEP41TransferLadderBudget > sep41TransfersReadTimeout {
		t.Fatalf("timescale.SEP41TransferLadderBudget=%s is more than half of sep41TransfersReadTimeout=%s — the fallback would run on less time than the ladder that failed to answer",
			timescale.SEP41TransferLadderBudget, sep41TransfersReadTimeout)
	}
}

// Valid strkeys for testing — both real-looking. The contract is
// an actual mainnet contract (DeFindex USDC vault); the account is
// from the asset_registry test fixtures.
const (
	validContractID = "CDB2WMKQQNVZMEBY7Q7GZ5C7E7IAFSNMZ7GGVD6WKTCEWK7XOIAVZSAP"
	validAccountG   = "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
)

func runSEP41Request(t *testing.T, contractPath, query string) *httptest.ResponseRecorder {
	t.Helper()
	reader := &stubSEP41TransfersReader{}
	srv := serverWithSEP41Reader(reader)

	// Use Go 1.22's path-value mux so PathValue("contract_id")
	// resolves the same way it does in production.
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/contracts/{contract_id}/transfers", srv.handleSEP41Transfers)

	url := "/v1/contracts/" + contractPath + "/transfers"
	if query != "" {
		url += "?" + query
	}
	req := httptest.NewRequest(http.MethodGet, url, nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func TestSEP41Transfers_InvalidContractID_Returns400(t *testing.T) {
	cases := []struct {
		name    string
		path    string
		wantSub string
	}{
		{"plain garbage", "FOO", "must be a 56-char C-strkey"},
		{"G-strkey instead of C-", validAccountG, "must be a 56-char C-strkey"},
		{"correct length but bad CRC", "CDB2WMKQQNVZMEBY7Q7GZ5C7E7IAFSNMZ7GGVD6WKTCEWK7XOIAVZSAQ", "must be a 56-char C-strkey"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := runSEP41Request(t, tc.path, "")
			if rec.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400 (body: %s)", rec.Code, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), tc.wantSub) {
				t.Errorf("body missing %q; got: %s", tc.wantSub, rec.Body.String())
			}
		})
	}
}

func TestSEP41Transfers_InvalidFromOrTo_Returns400(t *testing.T) {
	// Post-CAP-67 the SEP-41 from/to accepts every valid holder strkey (G/C/M/B/L), so only
	// genuinely-invalid strings are bad input; a C-strkey is a legitimate Soroban-contract
	// destination.
	cases := []struct {
		name string
		q    string
		want string
	}{
		{"from garbage", "from=NOTAGSTRKEY", "from must be a valid Stellar address"},
		{"to garbage", "to=NOTAGSTRKEY", "to must be a valid Stellar address"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := runSEP41Request(t, validContractID, tc.q)
			if rec.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400 (body: %s)", rec.Code, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), tc.want) {
				t.Errorf("body missing %q; got: %s", tc.want, rec.Body.String())
			}
		})
	}
}

// TestSEP41Transfers_AllHolderTypesAccepted_ReachReader ensures the post-CAP-67 widening
// (G/C/M/B/L) reaches the reader rather than 400-ing at validation. The stub returns empty;
// what matters is that each valid strkey gets to the reader.
func TestSEP41Transfers_AllHolderTypesAccepted_ReachReader(t *testing.T) {
	// One representative of each post-CAP-67 holder type — the
	// canonical.Is*-test fixtures pin the underlying strkey
	// validators; this test pins the handler-side acceptance.
	cases := []struct {
		name string
		addr string
	}{
		{"G account", validAccountG},
		{"C contract", validContractID},
		// M/B/L: SDK strkey-decode-tested fixtures so we know the CRC
		// is right. Compact base32 forms taken from the SDK's
		// strkey/decode_test.go.
		{"M muxed", "MA7QYNF7SOWQ3GLR2BGMZEHXAVIRZA4KVWLTJJFC7MGXUA74P7UJVAAAAAAAAAAAAAJLK"},
		{"L liquidity pool", "LA7QYNF7SOWQ3GLR2BGMZEHXAVIRZA4KVWLTJJFC7MGXUA74P7UJUPJN"},
		{"B claimable balance", "BAAD6DBUX6J22DMZOHIEZTEQ64CVCHEDRKWZONFEUL5Q26QD7R76RGR4TU"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reader := &stubSEP41TransfersReader{}
			srv := serverWithSEP41Reader(reader)
			mux := http.NewServeMux()
			mux.HandleFunc("GET /v1/contracts/{contract_id}/transfers", srv.handleSEP41Transfers)
			url := "/v1/contracts/" + validContractID + "/transfers?from=" + tc.addr + "&limit=10"
			req := httptest.NewRequest(http.MethodGet, url, nil)
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Errorf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
			}
			if reader.calledN != 1 {
				t.Errorf("reader called %d times, want 1", reader.calledN)
			}
		})
	}
}

func TestSEP41Transfers_ValidInputs_ReachReader(t *testing.T) {
	// Sanity: known-good inputs must not short-circuit at validation; they reach the reader (the stub returns an empty
	// list), proving validation isn't over-strict.
	reader := &stubSEP41TransfersReader{}
	srv := serverWithSEP41Reader(reader)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/contracts/{contract_id}/transfers", srv.handleSEP41Transfers)

	url := "/v1/contracts/" + validContractID + "/transfers?from=" + validAccountG + "&to=" + validAccountG + "&limit=10"
	req := httptest.NewRequest(http.MethodGet, url, nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	if reader.calledN != 1 {
		t.Errorf("reader called %d times, want 1 (validation should not short-circuit valid input)", reader.calledN)
	}
}

// TestSEP41Transfers_UnreachableStorageMapsTo503: with Postgres refusing connections, GET
// /v1/contracts/{id}/transfers must answer the retryable 503 reserved for transient storage failures, not 500
// (the `sep41-transfers-error` branch).
func TestSEP41Transfers_UnreachableStorageMapsTo503(t *testing.T) {
	srv := serverWithSEP41Reader(&unreachableTransfersReader{err: pgxDialRefused()})
	srv.logger = slog.New(slog.NewTextHandler(io.Discard, nil))

	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/contracts/{contract_id}/transfers", srv.handleSEP41Transfers)

	req := httptest.NewRequest(http.MethodGet, "/v1/contracts/"+validContractID+"/transfers", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d (body: %s)", rec.Code, http.StatusServiceUnavailable, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "sep41-transfers-transient") {
		t.Fatalf("problem type is not the transient-storage one; body: %s", rec.Body.String())
	}
}

type partialSEP41Transfers struct{ floor time.Time }

func (r partialSEP41Transfers) ListSEP41Transfers(
	_ context.Context, _, _, _ string, _ int,
) ([]timescale.SEP41TransferRow, time.Time, error) {
	return []timescale.SEP41TransferRow{{Kind: "transfer"}}, r.floor, nil
}

// TestSEP41Transfers_PartialWindowCarriesSearchedFrom: a read the store could answer only for a recent window is a 200 that names
// the window's start, so a short page isn't read as the whole history; an exact page omits the field.
func TestSEP41Transfers_PartialWindowCarriesSearchedFrom(t *testing.T) {
	floor := time.Date(2026, 10, 12, 9, 30, 0, 0, time.UTC)
	for _, tc := range []struct {
		name  string
		floor time.Time
		want  string
	}{
		{"partial", floor, "2026-10-12T09:30:00Z"},
		{"exact", time.Time{}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mux := http.NewServeMux()
			mux.HandleFunc("GET /v1/contracts/{contract_id}/transfers", serverWithSEP41Reader(partialSEP41Transfers{tc.floor}).handleSEP41Transfers)
			req := httptest.NewRequest(http.MethodGet, "/v1/contracts/"+validContractID+"/transfers?from="+validAccountG, nil)
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
			}
			var body struct {
				Data map[string]any `json:"data"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode: %v", err)
			}
			got, present := body.Data["searched_from"]
			if tc.want == "" {
				if present {
					t.Errorf("searched_from = %v on an exact page, want it omitted", got)
				}
				return
			}
			if got != tc.want {
				t.Errorf("searched_from = %v, want %q", got, tc.want)
			}
		})
	}
}
