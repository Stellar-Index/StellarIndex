package v1_test

import (
	"encoding/json"
	"io"
	"net/http"
	"testing"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

var testNets = []string{"testnet", "futurenet"}

// networkGet fetches path from a server built for network and returns status
// and body.
func networkGet(t *testing.T, opts v1.Options, network, path string) (int, string) {
	t.Helper()
	opts.Network = network
	ts := httpTestServer(t, v1.New(opts))
	resp := mustGet(t, ts.URL+path)
	b, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatalf("%s %s: read body: %v", network, path, err)
	}
	return resp.StatusCode, string(b)
}

// envelopeData returns the envelope's raw data payload and its row count.
func envelopeData(t *testing.T, body string) (string, int) {
	t.Helper()
	var env struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal([]byte(body), &env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	var rows []json.RawMessage
	if err := json.Unmarshal(env.Data, &rows); err != nil {
		t.Fatalf("decode data: %v", err)
	}
	return string(env.Data), len(rows)
}

// TestReferenceListings_NetworkScoped pins that the pubnet-anchored
// reference listings are served in full on pubnet (identical to the empty
// default) and empty on a test net.
func TestReferenceListings_NetworkScoped(t *testing.T) {
	reader := &stubAggregatorsReader{rows: []timescale.AggregatorRollupRow{{
		ContractID:   "CAG5LRYQ5JVEUI5TEID72EYOVX44TTUJT5BQR2J6J77FH65PCCFAJDDH",
		Name:         "soroswap-router",
		Kind:         "router",
		ProtocolSlug: "soroswap",
	}}}
	opts := v1.Options{VerifiedCurrencies: newTestCatalogue(t), Aggregators: reader}

	for _, path := range []string{"/v1/assets/verified", "/v1/external/assets", "/v1/aggregators"} {
		defStatus, def := networkGet(t, opts, "", path)
		pubStatus, pub := networkGet(t, opts, "pubnet", path)
		if defStatus != http.StatusOK || pubStatus != http.StatusOK {
			t.Fatalf("%s: status default=%d pubnet=%d, want 200", path, defStatus, pubStatus)
		}
		defData, _ := envelopeData(t, def)
		pubData, pubRows := envelopeData(t, pub)
		if defData != pubData {
			t.Errorf("%s: pubnet data differs from the default network's", path)
		}
		if pubRows == 0 {
			t.Errorf("%s: pubnet serves no rows", path)
		}
		for _, network := range testNets {
			status, body := networkGet(t, opts, network, path)
			if status != http.StatusOK {
				t.Errorf("%s %s: status = %d, want 200", network, path, status)
			}
			if _, n := envelopeData(t, body); n != 0 {
				t.Errorf("%s %s: %d pubnet reference rows served, want 0", network, path, n)
			}
		}
	}
}

// TestExternalAssetGet_NetworkScoped pins that the detail agrees with the
// listing: a catalogue fiat resolves on pubnet and 404s on a test net.
func TestExternalAssetGet_NetworkScoped(t *testing.T) {
	opts := v1.Options{VerifiedCurrencies: newTestCatalogue(t)}
	if status, _ := networkGet(t, opts, "pubnet", "/v1/external/assets/usd"); status != http.StatusOK {
		t.Fatalf("pubnet: status = %d, want 200", status)
	}
	for _, network := range testNets {
		if status, _ := networkGet(t, opts, network, "/v1/external/assets/usd"); status != http.StatusNotFound {
			t.Errorf("%s: status = %d, want 404", network, status)
		}
	}
}
