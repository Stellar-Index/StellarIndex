package v1_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	v1 "github.com/StellarIndex/stellar-index/internal/api/v1"
	"github.com/StellarIndex/stellar-index/internal/canonical"
	"github.com/StellarIndex/stellar-index/internal/storage/timescale"
)

// stubProtocolPoolTokensReader is a fake PoolTokens map for the roster
// pair-enrichment test.
type stubProtocolPoolTokensReader struct {
	bySource map[string]map[string][]string
	err      error
}

func (s *stubProtocolPoolTokensReader) PoolTokens(_ context.Context, source string) (map[string][]string, error) {
	return s.bySource[source], s.err
}

func testSAC(t *testing.T, assetID string) string {
	t.Helper()
	var a canonical.Asset
	var err error
	if assetID == "native" {
		a = canonical.NativeAsset()
	} else {
		a, err = canonical.ParseAsset(assetID)
		if err != nil {
			t.Fatalf("ParseAsset(%s): %v", assetID, err)
		}
	}
	sac, err := a.SacContractID()
	if err != nil {
		t.Fatalf("SacContractID(%s): %v", assetID, err)
	}
	return sac
}

func decodeProtocolDetail(t *testing.T, url string) v1.ProtocolDetailView {
	t.Helper()
	resp := mustGet(t, url)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var env struct {
		Data v1.ProtocolDetailView `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return env.Data
}

func findContract(t *testing.T, view v1.ProtocolDetailView, id string) v1.ProtocolContractView {
	t.Helper()
	for _, c := range view.Contracts {
		if c.ContractID == id {
			return c
		}
	}
	t.Fatalf("contract %q not in roster", id)
	return v1.ProtocolContractView{}
}

// The roster renders a human asset pair — "XLM/USDC" — for pool contracts of
// every pool-based protocol: soroswap (from soroswap_pairs token0/token1),
// comet + aquarius (from the PoolTokens map), including a 3-token stableswap.
// A pool whose tokens don't resolve stays present, just without a pair.
func TestHandleProtocolDetail_HumanPairs(t *testing.T) {
	xlmSAC := testSAC(t, "native")
	usdcSAC := testSAC(t, "USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")
	const unresolvable = "CUNKNOWNTOKEN00000000000000000000000000000000000000000ZZ"

	srv := v1.New(v1.Options{
		VerifiedCurrencies: newTestCatalogue(t),
		SoroswapPairs: &stubSoroswapPairsReader{pairs: []timescale.SoroswapPair{
			{PairStrkey: "CPAIRUSDC", Token0Strkey: xlmSAC, Token1Strkey: usdcSAC},
		}},
		ProtocolContracts: &stubProtocolContractsReader{projBySource: map[string][]string{
			// comet + aquarius rosters come from the projected-table fallback.
			"comet":    {"CCOMET1", "CCOMETNOTOK"},
			"aquarius": {"CAQUA1"},
		}},
		ProtocolPoolTokens: &stubProtocolPoolTokensReader{bySource: map[string]map[string][]string{
			"comet": {
				"CCOMET1": {xlmSAC, usdcSAC},
				// CCOMETNOTOK intentionally absent → no pair, but the row renders.
			},
			"aquarius": {
				// 3-token stableswap, one leg an unresolvable Soroban-native token.
				"CAQUA1": {xlmSAC, usdcSAC, unresolvable},
			},
		}},
	})
	ts := httpTestServer(t, srv)

	// ── soroswap: token0/token1 → "XLM/USDC" ──
	sw := decodeProtocolDetail(t, ts.URL+"/v1/protocols/soroswap")
	pair := findContract(t, sw, "CPAIRUSDC")
	if pair.Pair != "XLM/USDC" {
		t.Errorf("soroswap pair = %q, want XLM/USDC", pair.Pair)
	}
	if len(pair.TokenSymbols) != 2 || pair.TokenSymbols[0] != "XLM" || pair.TokenSymbols[1] != "USDC" {
		t.Errorf("soroswap token_symbols = %v, want [XLM USDC]", pair.TokenSymbols)
	}
	// The raw contracts are retained alongside the human labels.
	if len(pair.Tokens) != 2 || pair.Tokens[0] != xlmSAC || pair.Tokens[1] != usdcSAC {
		t.Errorf("soroswap tokens = %v, want the raw SAC pair", pair.Tokens)
	}
	if pair.Token0 != xlmSAC || pair.Token1 != usdcSAC {
		t.Errorf("soroswap token0/token1 dropped: %q %q", pair.Token0, pair.Token1)
	}

	// ── comet: PoolTokens map → "XLM/USDC"; a token-less pool stays bare ──
	cm := decodeProtocolDetail(t, ts.URL+"/v1/protocols/comet")
	if got := findContract(t, cm, "CCOMET1").Pair; got != "XLM/USDC" {
		t.Errorf("comet pair = %q, want XLM/USDC", got)
	}
	bare := findContract(t, cm, "CCOMETNOTOK")
	if bare.Pair != "" || len(bare.TokenSymbols) != 0 {
		t.Errorf("token-less comet pool carried a pair: %+v", bare)
	}

	// ── aquarius: 3-token stableswap, unresolvable leg → truncated symbol ──
	aq := decodeProtocolDetail(t, ts.URL+"/v1/protocols/aquarius")
	pool := findContract(t, aq, "CAQUA1")
	wantPair := "XLM/USDC/" + shortTokenLabel(unresolvable)
	if pool.Pair != wantPair {
		t.Errorf("aquarius pair = %q, want %q", pool.Pair, wantPair)
	}
	if len(pool.Tokens) != 3 {
		t.Errorf("aquarius tokens = %v, want 3 raw contracts", pool.Tokens)
	}
}

// shortTokenLabel mirrors the server's truncContract fallback for the
// assertion above (first 4 + ellipsis + last 4).
func shortTokenLabel(c string) string {
	if len(c) <= 8 {
		return c
	}
	return c[:4] + "…" + c[len(c)-4:]
}
