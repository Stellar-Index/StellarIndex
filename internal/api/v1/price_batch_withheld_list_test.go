package v1_test

import (
	"encoding/json"
	"io"
	"net/http"
	"reflect"
	"testing"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
)

// batchWithheld GETs one batch and returns its data length and withheld list.
func batchWithheld(t *testing.T, srv *v1.Server, ids string) (int, []string, bool) {
	t.Helper()
	ts := startHTTPTest(t, srv.Handler())
	resp := mustGet(t, ts.URL+"/v1/price/batch?asset_ids="+ids+"&quote=fiat:USD")
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, body)
	}
	var env map[string]json.RawMessage
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	var data []json.RawMessage
	_ = json.Unmarshal(env["data"], &data)
	raw, present := env["withheld"]
	var withheld []string
	if present {
		_ = json.Unmarshal(raw, &withheld)
	}
	return len(data), withheld, present
}

// TestPriceBatch_WithheldIDsNamedOnEnvelope: a withheld row is omitted
// from data (unchanged) AND named on `withheld`, so a batch caller can
// tell "we decline to publish" from "we have nothing" — before, both
// were a silent omission.
func TestPriceBatch_WithheldIDsNamedOnEnvelope(t *testing.T) {
	srv := v1.New(v1.Options{Prices: &stubPriceReader{err: v1.ErrPriceWithheld}})
	n, withheld, _ := batchWithheld(t, srv, "native")
	if n != 0 {
		t.Errorf("withheld row served in data (%d rows)", n)
	}
	if !reflect.DeepEqual(withheld, []string{"native"}) {
		t.Errorf("withheld = %v, want [native]", withheld)
	}
}

// TestPriceBatch_FallbackWithheldIDsNamed: a verdict reached inside the
// fallback chain (flagged issuer answering from the VWAP cache) is
// reported the same way, not folded into "no data".
func TestPriceBatch_FallbackWithheldIDsNamed(t *testing.T) {
	base := fallbackFlaggedBase(t)
	srv := v1.New(v1.Options{
		Prices:       &stubPriceReader{err: v1.ErrPriceNotFound},
		Triangulated: &cachedVWAPLooker{value: "0.00723"},
		Scam:         &fallbackScamGate{withheld: map[string]bool{base.String(): true}},
	})
	n, withheld, _ := batchWithheld(t, srv, base.String())
	if n != 0 || !reflect.DeepEqual(withheld, []string{base.String()}) {
		t.Errorf("rows = %d withheld = %v, want 0 rows and [%s]", n, withheld, base)
	}
}

// TestPriceBatch_PlainMissNotWithheld: the discriminator must not fire
// on a genuine miss — no data means no `withheld` member at all.
func TestPriceBatch_PlainMissNotWithheld(t *testing.T) {
	srv := v1.New(v1.Options{Prices: &stubPriceReader{err: v1.ErrPriceNotFound}})
	n, withheld, present := batchWithheld(t, srv, "native")
	if n != 0 || present {
		t.Errorf("rows = %d withheld present = %v (%v), want 0 rows and no withheld member", n, present, withheld)
	}
}
