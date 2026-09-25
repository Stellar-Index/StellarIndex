package mev

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// The evidence legs tell readers to verify a candidate on chain, so the
// op_index they publish must be the transaction's operation index, not
// the packed trades.op_index key (opIndex<<16 | eventIndex for the
// Soroban AMMs, opIndex*1024 + claim for SDEX).
func TestDetectArbitrage_LegsPublishOnChainOpIndex(t *testing.T) {
	a := trade(t, "soroswap", canonical.FanoutOpIndex(2, 1), "GARB", "native", usdc)
	b := trade(t, "sdex", 3*sdexOpIndexStride+4, "GARB", usdc, "native")
	got := DetectArbitrage([]canonical.Trade{a, b}, nil)
	if len(got) != 1 {
		t.Fatalf("got %d candidates, want 1", len(got))
	}
	ev, err := storedFrom(got[0])
	if err != nil {
		t.Fatalf("storedFrom: %v", err)
	}
	var d struct {
		Legs []map[string]any `json:"legs"`
	}
	if err := json.Unmarshal(ev.DetailJSON, &d); err != nil {
		t.Fatalf("detail: %v", err)
	}
	want := []map[string]float64{
		{"op_index": 2, "sub_index": 1, "trade_op_index": float64(canonical.FanoutOpIndex(2, 1))},
		{"op_index": 3, "sub_index": 4, "trade_op_index": 3*sdexOpIndexStride + 4},
	}
	if len(d.Legs) != 2 {
		t.Fatalf("legs = %v", d.Legs)
	}
	for i, w := range want {
		for k, v := range w {
			if d.Legs[i][k] != v {
				t.Errorf("leg %d %s = %v, want %v", i, k, d.Legs[i][k], v)
			}
		}
	}
}

// A source whose op_index encoding is unknown publishes no decoded
// op_index rather than a number that may not be an operation index.
func TestOpRefOf_UnknownSourceOmitsDecodedIndex(t *testing.T) {
	ref := opRefOf(canonical.Trade{Source: "somedex", OpIndex: 70_000})
	b, err := json.Marshal(ref)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if got := string(b); got != `{"trade_op_index":70000}` {
		t.Errorf("json = %s, want only trade_op_index", got)
	}
}

// sdexOpIndexStride must stay equal to the sdex decoder's stride; it is
// unexported there, so pin it at the source.
func TestSDEXOpIndexStrideMatchesSource(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("..", "..", "sources", "sdex", "decode.go"))
	if err != nil {
		t.Fatalf("read sdex decoder: %v", err)
	}
	if !strings.Contains(string(src), "const opIndexFanoutStride = 1024") || sdexOpIndexStride != 1024 {
		t.Fatal("sdex op_index stride changed: update sdexOpIndexStride and opRefOf")
	}
}
