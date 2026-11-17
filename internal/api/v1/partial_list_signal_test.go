package v1_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

type partialFlags struct {
	Data  json.RawMessage `json:"data"`
	Flags struct {
		Truncated      bool     `json:"truncated"`
		IncludesUnread []string `json:"includes_unread"`
	} `json:"flags"`
}

func getPartialFlags(t *testing.T, srv *v1.Server, path string) partialFlags {
	t.Helper()
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("%s: status = %d: %s", path, rec.Code, rec.Body)
	}
	var out partialFlags
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

type failingBLNTLendingReader struct{ stubBLNTLendingReader }

func (r *failingBLNTLendingReader) ListBLNTPools(context.Context) ([]timescale.BlendPoolSummary, error) {
	return nil, errors.New("blnt read boom")
}

// TestLendingPools_FailedBLNTReadIsTruncatedOnTheWire: Blend rows alone are a lower bound, and the body must say so.
func TestLendingPools_FailedBLNTReadIsTruncatedOnTheWire(t *testing.T) {
	reader := &failingBLNTLendingReader{stubBLNTLendingReader{
		stubLendingReader: stubLendingReader{pools: []timescale.BlendPoolSummary{{Pool: mkCStrkey(t, 1), AuctionsTotal: 3}}},
	}}
	if got := getPartialFlags(t, v1.New(v1.Options{Lending: reader}), "/v1/lending/pools"); !got.Flags.Truncated {
		t.Errorf("flags.truncated = false with the BLNT pools dropped")
	}
	ok := &stubBLNTLendingReader{stubLendingReader: reader.stubLendingReader}
	if got := getPartialFlags(t, v1.New(v1.Options{Lending: ok}), "/v1/lending/pools"); got.Flags.Truncated {
		t.Errorf("flags.truncated = true on a complete list")
	}
}

// TestMarkets_FailedEnrichmentNamesTheUnreadInclude: a dropped sparkline batch is otherwise byte-identical to "no series".
func TestMarkets_FailedEnrichmentNamesTheUnreadInclude(t *testing.T) {
	reader := &stubMarketsReader{
		pairs:    []v1.Market{{Base: "native", Quote: "fiat:USD", TradeCount24h: 4}},
		sparkErr: errors.New("sparkline boom"),
	}
	got := getPartialFlags(t, v1.New(v1.Options{Markets: reader}), "/v1/markets?include=sparkline")
	if !reflect.DeepEqual(got.Flags.IncludesUnread, []string{"sparkline"}) {
		t.Errorf("flags.includes_unread = %v, want [sparkline]", got.Flags.IncludesUnread)
	}
}

// TestIssuersList_FullPageIsTruncated: with no cursor, a page holding `limit` rows cannot claim to be every issuer.
func TestIssuersList_FullPageIsTruncated(t *testing.T) {
	reader := &stubIssuersReader{list: []timescale.IssuerSummary{{GStrkey: testUSDCIssuer}, {GStrkey: testUSDCIssuer}}}
	srv := v1.New(v1.Options{Issuers: reader})
	if got := getPartialFlags(t, srv, "/v1/issuers?limit=2"); !got.Flags.Truncated {
		t.Errorf("limit=2 with 2 rows: flags.truncated = false")
	}
	if got := getPartialFlags(t, srv, "/v1/issuers?limit=3"); got.Flags.Truncated {
		t.Errorf("limit=3 with 2 rows: flags.truncated = true")
	}
}
