package v1_test

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/api/streaming"
	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// flippableSubstanceGate allows until refuse is set, then withholds —
// a pair crossing the serve floor after its stream connected.
type flippableSubstanceGate struct{ refuse atomic.Bool }

func (g *flippableSubstanceGate) Allowed(context.Context, canonical.Asset, canonical.Asset, string) bool {
	return !g.refuse.Load()
}

func (g *flippableSubstanceGate) Verdict(ctx context.Context, base, quote canonical.Asset, surface string) (allowed, measured bool) {
	return g.Allowed(ctx, base, quote, surface), true
}

// readSSEFrame reads one SSE frame and returns its event type and data,
// skipping comment lines. Returns empty strings on EOF or timeout.
func readSSEFrame(br *bufio.Reader, timeout time.Duration) (event, data string) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		line, err := br.ReadString('\n')
		if err != nil {
			return event, data
		}
		switch {
		case strings.HasPrefix(line, ":"):
		case line == "\n":
			if data != "" {
				return event, data
			}
		case strings.HasPrefix(line, "event: "):
			event = strings.TrimSpace(strings.TrimPrefix(line, "event: "))
		case strings.HasPrefix(line, "data: "):
			data = strings.TrimSuffix(strings.TrimPrefix(line, "data: "), "\n")
		}
	}
	return event, data
}

// TestPriceTipStream_WithheldMidStreamEmitsMarker — a pair withheld
// after the stream opened must emit a named price_withheld event on
// both producers (per-connection and Hub-shared), not fall silent while
// keepalives make the stream look like a quiet market.
func TestPriceTipStream_WithheldMidStreamEmitsMarker(t *testing.T) {
	for _, tc := range []struct {
		name string
		hub  *streaming.Hub
	}{
		{"per_connection", nil},
		{"hub_shared", streaming.NewHub(0)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prices := &stubPriceReader{
				snapshots: map[string]v1.PriceSnapshot{
					"native/fiat:USD": {Price: "0.5", PriceType: "last_trade"},
				},
			}
			gate := &flippableSubstanceGate{}
			srv := v1.New(v1.Options{Prices: prices, Substance: gate, Hub: tc.hub})
			ts := httptest.NewServer(srv.Handler())
			t.Cleanup(ts.Close)

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			req, _ := http.NewRequestWithContext(ctx, http.MethodGet,
				ts.URL+"/v1/price/tip/stream?asset=native&quote=fiat:USD&window_seconds=1", nil)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("GET: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want 200 (the gate allows at connect)", resp.StatusCode)
			}

			br := bufio.NewReader(resp.Body)
			if ev, _ := readSSEFrame(br, 2*time.Second); ev != "tip_update" {
				t.Fatalf("first event = %q, want tip_update", ev)
			}
			gate.refuse.Store(true)

			var ev, data string
			for i := 0; i < 3 && ev != "price_withheld"; i++ {
				ev, data = readSSEFrame(br, 2500*time.Millisecond)
			}
			if ev != "price_withheld" {
				t.Fatalf("no price_withheld event after the pair was withheld (last event %q) — "+
					"the stream went silent instead", ev)
			}
			var got struct {
				AssetID string `json:"asset_id"`
				Quote   string `json:"quote"`
				Reason  string `json:"reason"`
				AsOf    string `json:"as_of"`
			}
			if err := json.Unmarshal([]byte(data), &got); err != nil {
				t.Fatalf("price_withheld data is not JSON: %v (%s)", err, data)
			}
			if got.AssetID != "native" || got.Quote != "fiat:USD" || got.Reason != string(v1.PriceWithheldSubstance) || got.AsOf == "" {
				t.Fatalf("price_withheld payload = %+v, want native / fiat:USD, reason %q, as_of set",
					got, v1.PriceWithheldSubstance)
			}
		})
	}
}
