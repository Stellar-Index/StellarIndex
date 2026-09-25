package client_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Stellar-Index/StellarIndex/pkg/client"
)

// TestIsWithheld_DiscriminatesTheTwo404s: /v1/price answers 404 both for
// "no data" and for "price withheld"; IsNotFound cannot tell them apart,
// so the SDK must expose the problem type's verdict directly.
func TestIsWithheld_DiscriminatesTheTwo404s(t *testing.T) {
	for _, tc := range []struct {
		typ          string
		wantWithheld bool
	}{
		{client.ProblemTypePriceWithheld, true},
		{client.ProblemTypePriceNotFound, false},
	} {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/problem+json")
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"type":"` + tc.typ + `","title":"t","status":404}`))
		}))
		_, err := client.New(client.Options{BaseURL: ts.URL}).Price(context.Background(),
			client.PriceQuery{Asset: "native", Quote: "fiat:USD"})
		ts.Close()
		var apiErr *client.APIError
		if !errors.As(err, &apiErr) {
			t.Fatalf("%s: err = %v, want *APIError", tc.typ, err)
		}
		if got := apiErr.IsWithheld(); got != tc.wantWithheld {
			t.Errorf("%s: IsWithheld = %v, want %v", tc.typ, got, tc.wantWithheld)
		}
		if !apiErr.IsNotFound() {
			t.Errorf("%s: IsNotFound = false; both are 404s", tc.typ)
		}
	}
}

// TestEnvelope_DecodesWithheld: the batch envelope's withheld list must
// reach the caller, or the server-side discriminator is invisible.
func TestEnvelope_DecodesWithheld(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[],"withheld":["native"],"as_of":"2026-04-28T10:00:00Z","flags":{}}`))
	}))
	t.Cleanup(ts.Close)
	env, err := client.New(client.Options{BaseURL: ts.URL}).PriceBatch(context.Background(),
		client.PriceBatchQuery{AssetIDs: []string{"native"}})
	if err != nil {
		t.Fatalf("PriceBatch: %v", err)
	}
	if len(env.Withheld) != 1 || env.Withheld[0] != "native" {
		t.Errorf("Withheld = %v, want [native]", env.Withheld)
	}
}
