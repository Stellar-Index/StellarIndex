package v1_test

import (
	"net/http"
	"testing"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// mkRawUpdate is a fixture `raw:<symbol>` oracle row — an unmapped
// oracle symbol recorded verbatim (canonical.AssetOracleRaw).
func mkRawUpdate(t *testing.T, source, symbol string) canonical.OracleUpdate {
	t.Helper()
	u := mkReflectorUpdate(source, "12000000000000", 14)
	raw, err := canonical.NewOracleRawAsset(symbol)
	if err != nil {
		t.Fatalf("NewOracleRawAsset: %v", err)
	}
	u.Asset = raw
	u.OpIndex = 1
	return u
}

func getStreams(t *testing.T, url string) []v1.OracleReading {
	t.Helper()
	resp := mustGet(t, url)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var env struct {
		Data []v1.OracleReading `json:"data"`
	}
	mustDecode(t, resp, &env)
	return env.Data
}

// TestOracleStreams_UnmappedRowsOptIn pins the /v1/oracle/streams
// contract under oracle capture-totality: the storage read is
// unfiltered, so a raw row reaches the handler; by default it is
// omitted (public row set unchanged for API consumers), and
// include_unmapped=true — the explorer's opt-in — lists it with
// mapped:false while mapped rows carry mapped:true.
func TestOracleStreams_UnmappedRowsOptIn(t *testing.T) {
	reader := &stubOracleReader{
		updates: []canonical.OracleUpdate{
			mkReflectorUpdate("reflector-cex", "12000000000000", 14),
			mkRawUpdate(t, "reflector-cex", "NOTACOIN"),
			mkRawUpdate(t, "redstone", "SolvBTC.BBN_FUNDAMENTAL/USD"),
		},
	}
	srv := v1.New(v1.Options{Oracle: reader})
	ts := httpTestServer(t, srv)

	// Default: mapped rows only.
	def := getStreams(t, ts.URL+"/v1/oracle/streams")
	if len(def) != 1 {
		t.Fatalf("default len(data) = %d, want 1 (raw rows omitted): %+v", len(def), def)
	}
	if def[0].Asset != "native" || !def[0].Mapped {
		t.Errorf("default row = %+v, want native with mapped:true", def[0])
	}

	// Anything but the literal "true" is the default.
	if got := getStreams(t, ts.URL+"/v1/oracle/streams?include_unmapped=1"); len(got) != 1 {
		t.Errorf("include_unmapped=1 len(data) = %d, want 1 (only the literal true opts in)", len(got))
	}

	// Opt-in: every row, raw ones flagged mapped:false, symbol verbatim.
	all := getStreams(t, ts.URL+"/v1/oracle/streams?include_unmapped=true")
	if len(all) != 3 {
		t.Fatalf("include_unmapped=true len(data) = %d, want 3: %+v", len(all), all)
	}
	byAsset := map[string]v1.OracleReading{}
	for _, r := range all {
		byAsset[r.Asset] = r
	}
	if r, ok := byAsset["native"]; !ok || !r.Mapped {
		t.Errorf("native row = %+v, want mapped:true", r)
	}
	for _, want := range []string{"raw:NOTACOIN", "raw:SolvBTC.BBN_FUNDAMENTAL/USD"} {
		r, ok := byAsset[want]
		if !ok {
			t.Errorf("missing raw row %q in opt-in response: %+v", want, all)
			continue
		}
		if r.Mapped {
			t.Errorf("%s reported mapped:true, want false", want)
		}
		if r.Quote != "fiat:USD" || r.Price != "0.12000000000000" {
			t.Errorf("%s = %+v, want the row's quote and price rendered as any other", want, r)
		}
	}
}

// TestOracleLatest_EmitsMapped pins the `mapped` field on the keyed
// endpoint: a mapped asset reads mapped:true, and an explicit
// `asset=raw:<symbol>` query returns the raw row with mapped:false.
func TestOracleLatest_EmitsMapped(t *testing.T) {
	reader := &stubOracleReader{updates: []canonical.OracleUpdate{mkReflectorUpdate("reflector-cex", "12000000000000", 14)}}
	srv := v1.New(v1.Options{Oracle: reader})
	ts := httpTestServer(t, srv)

	got := getStreams(t, ts.URL+"/v1/oracle/latest?asset=native")
	if len(got) != 1 || !got[0].Mapped {
		t.Fatalf("latest(native) = %+v, want one row with mapped:true", got)
	}

	reader.updates = []canonical.OracleUpdate{mkRawUpdate(t, "reflector-cex", "NOTACOIN")}
	got = getStreams(t, ts.URL+"/v1/oracle/latest?asset=raw:NOTACOIN")
	if len(got) != 1 || got[0].Mapped || got[0].Asset != "raw:NOTACOIN" {
		t.Fatalf("latest(raw:NOTACOIN) = %+v, want the raw row with mapped:false", got)
	}
	if reader.lastAsset != "raw:NOTACOIN" || len(reader.lastAssets) != 1 {
		t.Errorf("raw query expanded to %v, want the exact raw key only", reader.lastAssets)
	}
}
