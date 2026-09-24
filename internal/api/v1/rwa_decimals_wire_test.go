package v1_test

import (
	"encoding/json"
	"net/http"
	"strconv"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// decimalsText renders a served scale for a failure message.
func decimalsText(d *int) string {
	if d == nil {
		return "null"
	}
	return strconv.Itoa(*d)
}

// rwaWireDecimals fetches /v1/rwa/assets and returns the raw `decimals`
// value served on the contract row, read from the bytes rather than a Go
// struct so the assertion is about the wire and not a decode default.
func rwaWireDecimals(t *testing.T, decimals map[string]uint32) (json.RawMessage, bool) {
	t.Helper()
	srv := rwaListingServer(t,
		&stubRWAListings{rows: []timescale.ListingEntry{
			listed(rwaListedBoundEUTBL, "eutbl", "eutbl", "1.22"),
		}},
		map[string]timescale.AssetRow{
			rwaListedBoundEUTBL: rwaContractRow(rwaListedBoundEUTBL, sptr("1.22")),
		},
		map[string]string{rwaListedBoundEUTBL: "28327867109034"},
		decimals,
		map[string]timescale.DirectoryEntry{},
	)
	resp := mustGet(t, httpTestServer(t, srv).URL+"/v1/rwa/assets")
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var env struct {
		Data struct {
			Assets []map[string]json.RawMessage `json:"assets"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, a := range env.Data.Assets {
		var id string
		if err := json.Unmarshal(a["contract_id"], &id); err != nil || id != rwaListedBoundEUTBL {
			continue
		}
		raw, ok := a["decimals"]
		return raw, ok
	}
	t.Fatalf("contract row %s not served", rwaListedBoundEUTBL)
	return nil, false
}

// TestRWAListing_UnreadDecimalsServeNullScale: a contract row whose scale
// was never read must not serve the catalogue's default 7 as `decimals`.
// Beside a raw circulating_supply that number invites exactly the
// supply/10^decimals division both valuation bases refuse — for this
// 5-decimal fund, a whole-token float one hundredth of the real one.
func TestRWAListing_UnreadDecimalsServeNullScale(t *testing.T) {
	raw, ok := rwaWireDecimals(t, map[string]uint32{})
	if !ok {
		t.Fatal("decimals key absent; the schema requires it (null when unread)")
	}
	if string(raw) != "null" {
		t.Errorf("decimals = %s on a row whose scale was never read, want null", raw)
	}
}

// TestRWAListing_ReadDecimalsServeTheReading is the other half: a scale
// that WAS read is served as that number, not nulled with the rest.
func TestRWAListing_ReadDecimalsServeTheReading(t *testing.T) {
	raw, ok := rwaWireDecimals(t, map[string]uint32{rwaListedBoundEUTBL: 5})
	if !ok || string(raw) != "5" {
		t.Errorf("decimals = %s (present %v), want the contract's declared 5", raw, ok)
	}
}
