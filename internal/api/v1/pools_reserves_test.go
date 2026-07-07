package v1_test

import (
	"math/big"
	"net/http"
	"testing"
	"time"

	"github.com/stellar/go-stellar-sdk/strkey"

	v1 "github.com/StellarIndex/stellar-index/internal/api/v1"
	"github.com/StellarIndex/stellar-index/internal/storage/clickhouse"
	"github.com/StellarIndex/stellar-index/internal/storage/timescale"
)

// mkCStrkey builds a valid deterministic C-strkey from a seed byte —
// the handler validates pool ids with canonical.IsContractID, so test
// fixtures must be real contract strkeys.
func mkCStrkey(t *testing.T, seed byte) string {
	t.Helper()
	raw := make([]byte, 32)
	raw[0] = seed
	s, err := strkey.Encode(strkey.VersionByteContract, raw)
	if err != nil {
		t.Fatalf("encode strkey: %v", err)
	}
	return s
}

func poolReservesTestServer(t *testing.T, explorer v1.ExplorerReader, pairs []timescale.SoroswapPair) string {
	t.Helper()
	srv := v1.New(v1.Options{
		Explorer:      explorer,
		SoroswapPairs: &stubSoroswapPairsReader{pairs: pairs},
	})
	return httpTestServer(t, srv).URL
}

func TestPoolReserves_Listing(t *testing.T) {
	pairA := mkCStrkey(t, 1)
	pairB := mkCStrkey(t, 2)
	tok0 := mkCStrkey(t, 10)
	tok1 := mkCStrkey(t, 11)

	reader := &stubExplorerReader{
		pairStates: map[string]clickhouse.SoroswapPairState{
			pairA: {
				Pair: pairA, Token0: tok0, Token1: tok1,
				// Reserve0 > 2^63 — must survive as an exact string.
				Reserve0: mustBig(t, "18446744073709551621"), // 2^64 + 5
				Reserve1: big.NewInt(603_291_773_585),
				Ledger:   62_941_880,
			},
			// pairB has NO captured state → must be absent, never zero.
		},
		tokenDisplays: map[string]clickhouse.TokenDisplayMeta{
			tok0: {Symbol: "YBX", Name: "YieldBlox", Decimals: 7, HasMeta: true},
			tok1: {}, // no metadata → defaults (decimals 7, no symbol)
		},
	}
	base := poolReservesTestServer(t, reader, []timescale.SoroswapPair{
		{PairStrkey: pairA, Token0Strkey: tok0, Token1Strkey: tok1},
		{PairStrkey: pairB, Token0Strkey: tok0, Token1Strkey: tok1},
	})

	resp := mustGet(t, base+"/v1/pools/reserves")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var body struct {
		Data []v1.PoolReservesRow `json:"data"`
	}
	mustDecode(t, resp, &body)

	if len(body.Data) != 1 {
		t.Fatalf("want 1 row (pairB uncaptured must be omitted), got %d", len(body.Data))
	}
	row := body.Data[0]
	if row.Pool != pairA || row.Source != "soroswap" || row.Model != "constant_product" || row.FeeBps != 30 {
		t.Fatalf("row header = %+v", row)
	}
	if row.AsOfLedger != 62_941_880 {
		t.Fatalf("as_of_ledger = %d", row.AsOfLedger)
	}
	// ADR-0003: the >2^63 reserve is an exact decimal string.
	if row.Token0.Reserve != "18446744073709551621" {
		t.Fatalf("token0.reserve = %q, want exact i128 string", row.Token0.Reserve)
	}
	if row.Token0.Symbol != "YBX" || row.Token0.Decimals != 7 {
		t.Fatalf("token0 display = %+v", row.Token0)
	}
	if row.Token1.Symbol != "" || row.Token1.Decimals != 7 {
		t.Fatalf("token1 must fall back to defaults, got %+v", row.Token1)
	}
	if row.MidPrice0In1 == nil || row.MidPrice1In0 == nil {
		t.Fatal("mid prices must be present for a funded pool")
	}
	if len(row.Depth) != 3 {
		t.Fatalf("want 3 depth tiers, got %d", len(row.Depth))
	}
	if row.Depth[0].SlippagePct != "0.5" || row.Depth[2].SlippagePct != "2" {
		t.Fatalf("depth tiers = %+v", row.Depth)
	}
	if row.Depth[0].Token0In.MaxInput == "0" || row.Depth[0].Token1In.MaxInput == "0" {
		t.Fatalf("funded pool must have non-zero depth, got %+v", row.Depth[0])
	}
}

func TestPoolReserves_PoolFilter(t *testing.T) {
	pairA := mkCStrkey(t, 1)
	tok0, tok1 := mkCStrkey(t, 10), mkCStrkey(t, 11)
	reader := &stubExplorerReader{
		pairStates: map[string]clickhouse.SoroswapPairState{
			pairA: {
				Pair: pairA, Token0: tok0, Token1: tok1,
				Reserve0: big.NewInt(1_000), Reserve1: big.NewInt(2_000), Ledger: 1,
			},
		},
	}
	base := poolReservesTestServer(t, reader, []timescale.SoroswapPair{
		{PairStrkey: pairA, Token0Strkey: tok0, Token1Strkey: tok1},
	})

	t.Run("registered pair returns one row", func(t *testing.T) {
		resp := mustGet(t, base+"/v1/pools/reserves?pool="+pairA)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d", resp.StatusCode)
		}
		var body struct {
			Data []v1.PoolReservesRow `json:"data"`
		}
		mustDecode(t, resp, &body)
		if len(body.Data) != 1 || body.Data[0].Pool != pairA {
			t.Fatalf("data = %+v", body.Data)
		}
	})

	t.Run("valid but unregistered contract is an honest 404", func(t *testing.T) {
		resp := mustGet(t, base+"/v1/pools/reserves?pool="+mkCStrkey(t, 99))
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", resp.StatusCode)
		}
		if cc := resp.Header.Get("Cache-Control"); cc != "no-store" {
			t.Fatalf("problem Cache-Control = %q, want no-store", cc)
		}
	})

	t.Run("malformed pool id is a 400", func(t *testing.T) {
		resp := mustGet(t, base+"/v1/pools/reserves?pool=not-a-contract")
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", resp.StatusCode)
		}
	})

	t.Run("unsupported venue is a 400 naming coverage", func(t *testing.T) {
		resp := mustGet(t, base+"/v1/pools/reserves?source=phoenix")
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", resp.StatusCode)
		}
	})
}

func TestPoolReserves_EmptySideOmitsDepth(t *testing.T) {
	pairA := mkCStrkey(t, 1)
	tok0, tok1 := mkCStrkey(t, 10), mkCStrkey(t, 11)
	reader := &stubExplorerReader{
		pairStates: map[string]clickhouse.SoroswapPairState{
			pairA: {
				Pair: pairA, Token0: tok0, Token1: tok1,
				Reserve0: new(big.Int), Reserve1: big.NewInt(2_000), Ledger: 1,
			},
		},
	}
	base := poolReservesTestServer(t, reader, []timescale.SoroswapPair{
		{PairStrkey: pairA, Token0Strkey: tok0, Token1Strkey: tok1},
	})

	resp := mustGet(t, base+"/v1/pools/reserves")
	var body struct {
		Data []v1.PoolReservesRow `json:"data"`
	}
	mustDecode(t, resp, &body)
	if len(body.Data) != 1 {
		t.Fatalf("want 1 row, got %d", len(body.Data))
	}
	row := body.Data[0]
	if row.MidPrice0In1 != nil || row.MidPrice1In0 != nil {
		t.Fatal("empty side must not fabricate a mid price")
	}
	if len(row.Depth) != 0 {
		t.Fatalf("empty side must not fabricate depth, got %+v", row.Depth)
	}
	if row.Token0.Reserve != "0" || row.Token1.Reserve != "2000" {
		t.Fatalf("reserves = (%q, %q)", row.Token0.Reserve, row.Token1.Reserve)
	}
}

func TestPoolReserves_ExplorerUnavailable(t *testing.T) {
	srv := v1.New(v1.Options{
		SoroswapPairs: &stubSoroswapPairsReader{},
	})
	base := httpTestServer(t, srv).URL
	resp := mustGet(t, base+"/v1/pools/reserves")
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 when no lake reader is wired", resp.StatusCode)
	}
}

// TestPoolReserves_WatermarkStale pins ADR-0041 Decision 4 on the pool
// reserves read: each row keeps its exact per-pool `as_of_ledger` (the
// pool's last state-change ledger), but the envelope's `flags.stale`
// now reflects lake freshness — a wedged sink (10-min-old watermark
// here) flips it regardless of when the pool last changed.
func TestPoolReserves_WatermarkStale(t *testing.T) {
	pairA := mkCStrkey(t, 1)
	tok0, tok1 := mkCStrkey(t, 10), mkCStrkey(t, 11)
	reader := &stubExplorerReader{
		pairStates: map[string]clickhouse.SoroswapPairState{
			pairA: {
				Pair: pairA, Token0: tok0, Token1: tok1,
				Reserve0: big.NewInt(1_000), Reserve1: big.NewInt(2_000), Ledger: 62_900_000,
			},
		},
	}
	srv := v1.New(v1.Options{
		Explorer:      reader,
		SoroswapPairs: &stubSoroswapPairsReader{pairs: []timescale.SoroswapPair{{PairStrkey: pairA, Token0Strkey: tok0, Token1Strkey: tok1}}},
		LakeWatermark: &wmStub{ledger: 63_500_000, closedAt: time.Now().Add(-10 * time.Minute)},
	})
	base := httpTestServer(t, srv).URL

	resp := mustGet(t, base+"/v1/pools/reserves")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var body struct {
		Data  []v1.PoolReservesRow `json:"data"`
		Flags struct {
			Stale bool `json:"stale"`
		} `json:"flags"`
	}
	mustDecode(t, resp, &body)
	if !body.Flags.Stale {
		t.Error("flags.stale should fire for a 10-minute-old lake watermark")
	}
	// Per-pool as_of_ledger stays the pool's own last-change ledger, not
	// the watermark (they are deliberately distinct signals).
	if len(body.Data) != 1 || body.Data[0].AsOfLedger != 62_900_000 {
		t.Errorf("row as_of_ledger = %+v, want the pool's last-change ledger 62900000", body.Data)
	}
}

func mustBig(t *testing.T, s string) *big.Int {
	t.Helper()
	v, ok := new(big.Int).SetString(s, 10)
	if !ok {
		t.Fatalf("bad big.Int literal %q", s)
	}
	return v
}
