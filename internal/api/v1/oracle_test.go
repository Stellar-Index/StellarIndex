package v1_test

import (
	"context"
	"errors"
	"math/big"
	"net/http"
	"testing"
	"time"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/currency"
)

type stubOracleReader struct {
	updates    []canonical.OracleUpdate
	lastAsset  string
	lastAssets []string
	lastSource string
	err        error
}

func (r *stubOracleReader) LatestOracleUpdatesForAsset(_ context.Context, asset canonical.Asset, src string) ([]canonical.OracleUpdate, error) {
	r.lastAsset = asset.String()
	r.lastSource = src
	if r.err != nil {
		return nil, r.err
	}
	return r.updates, nil
}

func (r *stubOracleReader) LatestOracleUpdatesForAssets(_ context.Context, assets []canonical.Asset, src string) ([]canonical.OracleUpdate, error) {
	r.lastAssets = make([]string, len(assets))
	for i, a := range assets {
		r.lastAssets[i] = a.String()
	}
	if len(assets) > 0 {
		r.lastAsset = assets[0].String()
	}
	r.lastSource = src
	if r.err != nil {
		return nil, r.err
	}
	return r.updates, nil
}

func (r *stubOracleReader) LatestOracleStreams(_ context.Context) ([]canonical.OracleUpdate, error) {
	if r.err != nil {
		return nil, r.err
	}
	return r.updates, nil
}

func mkReflectorUpdate(source string, priceRaw string, decimals uint8) canonical.OracleUpdate {
	usdc, _ := canonical.ParseAsset("fiat:USD")
	price, _ := new(big.Int).SetString(priceRaw, 10)
	return canonical.OracleUpdate{
		Source:     source,
		ContractID: "CAS3J7GYLGXMF6TDJBBYYSE3HQ6BBSMLNUQ34T6TZMYMW2EVH34XOWMA",
		Ledger:     52_430_001,
		TxHash:     "cafebabecafebabecafebabecafebabecafebabecafebabecafebabecafebabe",
		OpIndex:    0,
		Timestamp:  time.Unix(1_772_000_000, 0).UTC(),
		Asset:      canonical.NativeAsset(),
		Quote:      usdc,
		Price:      canonical.NewAmount(price),
		Decimals:   decimals,
		Confidence: 0.95,
		Observer:   "GRELAYER123",
	}
}

func TestOracleLatest_503WhenReaderNil(t *testing.T) {
	srv := v1.New(v1.Options{})
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/oracle/latest?asset=native")
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
}

func TestOracleLatest_MissingAsset400(t *testing.T) {
	srv := v1.New(v1.Options{Oracle: &stubOracleReader{}})
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/oracle/latest")
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

func TestOracleLatest_InvalidAsset400(t *testing.T) {
	srv := v1.New(v1.Options{Oracle: &stubOracleReader{}})
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/oracle/latest?asset=not-an-asset")
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

func TestOracleLatest_ReturnsReadings(t *testing.T) {
	reader := &stubOracleReader{
		updates: []canonical.OracleUpdate{
			// 14-decimal price — Reflector's canonical scale.
			// 12000000000000 at 14 decimals → 0.12000000000000
			mkReflectorUpdate("reflector-dex", "12000000000000", 14),
			mkReflectorUpdate("reflector-cex", "12500000000000", 14),
		},
	}
	srv := v1.New(v1.Options{Oracle: reader})
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/oracle/latest?asset=native")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var env struct {
		Data []v1.OracleReading `json:"data"`
	}
	mustDecode(t, resp, &env)

	if len(env.Data) != 2 {
		t.Fatalf("got %d readings, want 2", len(env.Data))
	}
	r := env.Data[0]
	if r.Source != "reflector-dex" {
		t.Errorf("source = %q", r.Source)
	}
	if r.Price != "0.12000000000000" {
		t.Errorf("price = %q, want 0.12000000000000 (14-decimal scaling)", r.Price)
	}
	if r.PriceRaw != "12000000000000" {
		t.Errorf("price_raw = %q, want the integer value", r.PriceRaw)
	}
	if r.Decimals != 14 {
		t.Errorf("decimals = %d, want 14", r.Decimals)
	}
}

// TestOracleLatest_OmitsRetiredSource: the retired Ethereum chainlink feed's last row is not served beside live feeds.
func TestOracleLatest_OmitsRetiredSource(t *testing.T) {
	reader := &stubOracleReader{updates: []canonical.OracleUpdate{
		mkReflectorUpdate("chainlink", "12000000000000", 14),
		mkReflectorUpdate("chainlink-stellar", "12500000000000", 14),
	}}
	ts := httpTestServer(t, v1.New(v1.Options{Oracle: reader}))
	var page struct {
		Data []v1.OracleReading `json:"data"`
	}
	mustDecode(t, mustGet(t, ts.URL+"/v1/oracle/latest?asset=native"), &page)
	if len(page.Data) != 1 || page.Data[0].Source != "chainlink-stellar" {
		t.Fatalf("unfiltered readings = %+v, want only chainlink-stellar", page.Data)
	}
}

func TestOracleLatest_SourceFilterThreaded(t *testing.T) {
	reader := &stubOracleReader{
		updates: []canonical.OracleUpdate{mkReflectorUpdate("reflector-dex", "12000000000000", 14)},
	}
	srv := v1.New(v1.Options{Oracle: reader})
	ts := httpTestServer(t, srv)

	_ = mustGet(t, ts.URL+"/v1/oracle/latest?asset=native&source=reflector-dex")

	if reader.lastSource != "reflector-dex" {
		t.Errorf("source filter = %q, want reflector-dex", reader.lastSource)
	}
	if reader.lastAsset != "native" {
		t.Errorf("asset = %q, want native", reader.lastAsset)
	}
}

// TestOracleLatest_UnknownSource400: `?source=` naming a source not in external.Registry returns 400, not a silent empty
// page (same fix as /v1/markets and /v1/observations).
func TestOracleLatest_UnknownSource400(t *testing.T) {
	srv := v1.New(v1.Options{Oracle: &stubOracleReader{}})
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/oracle/latest?asset=native&source=fake-venue")
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	var p v1.Problem
	mustDecode(t, resp, &p)
	if p.Type != "https://api.stellarindex.io/errors/unknown-source" {
		t.Errorf("Type = %q", p.Type)
	}
}

func TestOracleLatest_EmptyIsEmptyArray(t *testing.T) {
	srv := v1.New(v1.Options{Oracle: &stubOracleReader{updates: nil}})
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/oracle/latest?asset=native")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (empty != error)", resp.StatusCode)
	}
	var env struct {
		Data []v1.OracleReading `json:"data"`
	}
	mustDecode(t, resp, &env)
	if env.Data == nil {
		t.Error("empty should serialise as [] not null")
	}
}

func TestOracleLatest_ReaderError500(t *testing.T) {
	reader := &stubOracleReader{err: errors.New("storage broke")}
	srv := v1.New(v1.Options{Oracle: reader})
	tsrv := httpTestServer(t, srv)

	resp := mustGet(t, tsrv.URL+"/v1/oracle/latest?asset=native")
	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", resp.StatusCode)
	}
}

// Negative-price rendering through the full HTTP handler pins the sign-preserving scaledDecimalString path, which is
// reached only indirectly since oracleReadingFrom is unexported.
func TestOracleLatest_negativePricePreservesSign(t *testing.T) {
	reader := &stubOracleReader{
		updates: []canonical.OracleUpdate{
			mkReflectorUpdate("reflector-cex", "-12420000000000", 14),
		},
	}
	srv := v1.New(v1.Options{Oracle: reader})
	tsrv := httpTestServer(t, srv)

	resp := mustGet(t, tsrv.URL+"/v1/oracle/latest?asset=native")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var env struct {
		Data []v1.OracleReading `json:"data"`
	}
	mustDecode(t, resp, &env)
	if len(env.Data) != 1 {
		t.Fatalf("got %d rows, want 1", len(env.Data))
	}
	if env.Data[0].Price[0] != '-' {
		t.Errorf("Price = %q, want leading \"-\"", env.Data[0].Price)
	}
	if env.Data[0].Decimals != 14 {
		t.Errorf("Decimals = %d, want 14", env.Data[0].Decimals)
	}
}

// TestOracleLatest_NativeExpandsToCryptoXLM pins that /v1/oracle/latest?asset=native also queries `crypto:XLM`
// (Reflector's key) and the XLM SAC C-address (reflector-dex); omitting either returns empty or hides that oracle.
func TestOracleLatest_NativeExpandsToCryptoXLM(t *testing.T) {
	reader := &stubOracleReader{}
	srv := v1.New(v1.Options{Oracle: reader})
	tsrv := httpTestServer(t, srv)

	resp := mustGet(t, tsrv.URL+"/v1/oracle/latest?asset=native")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	wantKeys := []string{"native", "crypto:XLM", canonical.XLMSacContractID}
	if len(reader.lastAssets) != len(wantKeys) {
		t.Fatalf("lastAssets = %+v, want %+v", reader.lastAssets, wantKeys)
	}
	for i, k := range wantKeys {
		if reader.lastAssets[i] != k {
			t.Errorf("lastAssets[%d] = %q, want %q", i, reader.lastAssets[i], k)
		}
	}
}

// TestOracleLatest_ClassicExpandsToCryptoTicker pins the same translation for stablecoin classic assets: Reflector
// publishes USDC under `crypto:USDC`. The id is Circle's real issuance because expansion is gated on the verified
// catalogue; TestOracleLatest_ImpersonatorGetsNoVerifiedTicker pins that nobody else gets it.
func TestOracleLatest_ClassicExpandsToCryptoTicker(t *testing.T) {
	cat, err := currency.LoadEmbedded()
	if err != nil {
		t.Fatalf("LoadEmbedded: %v", err)
	}
	reader := &stubOracleReader{}
	srv := v1.New(v1.Options{Oracle: reader, VerifiedCurrencies: cat})
	tsrv := httpTestServer(t, srv)

	resp := mustGet(t, tsrv.URL+"/v1/oracle/latest?asset=USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	wantKeys := []string{
		"USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN",
		"crypto:USDC",
	}
	if len(reader.lastAssets) != len(wantKeys) {
		t.Fatalf("lastAssets = %+v, want %+v", reader.lastAssets, wantKeys)
	}
	for i, k := range wantKeys {
		if reader.lastAssets[i] != k {
			t.Errorf("lastAssets[%d] = %q, want %q", i, reader.lastAssets[i], k)
		}
	}
}

// TestOracleLatest_VerifiedEURCReachesEUROCFeeds: the catalogue ticker is
// EURC but RedStone publishes the same coin as crypto:EUROC, so the
// ticker the verified issuer is granted must carry its rename alias.
func TestOracleLatest_VerifiedEURCReachesEUROCFeeds(t *testing.T) {
	cat, err := currency.LoadEmbedded()
	if err != nil {
		t.Fatalf("LoadEmbedded: %v", err)
	}
	reader := &stubOracleReader{}
	srv := v1.New(v1.Options{Oracle: reader, VerifiedCurrencies: cat})
	tsrv := httpTestServer(t, srv)

	const eurcClassic = "EURC-GDHU6WRG4IEQXM5NZ4BMPKOXHW76MZM4Y2IEMFDVXBSDP6SJY4ITNPP2"
	resp := mustGet(t, tsrv.URL+"/v1/oracle/latest?asset="+eurcClassic)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	wantKeys := []string{eurcClassic, "crypto:EURC", "crypto:EUROC"}
	if len(reader.lastAssets) != len(wantKeys) {
		t.Fatalf("lastAssets = %+v, want %+v", reader.lastAssets, wantKeys)
	}
	for i, k := range wantKeys {
		if reader.lastAssets[i] != k {
			t.Errorf("lastAssets[%d] = %q, want %q", i, reader.lastAssets[i], k)
		}
	}
}

// TestOracleLatest_EveryAliasFamilyMemberCoversItsFamily guards against a hand-rolled expander
// drifting from the alias registry: with a classic and SAC pair installed, querying ANY member of
// ANY family must bind the whole family, whichever form the oracle stored it under.
func TestOracleLatest_EveryAliasFamilyMemberCoversItsFamily(t *testing.T) {
	const usdcClassic = "USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
	const usdcSACContract = "CCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUZPUTHXSTZLEO7SJMI75"
	reg, err := canonical.NewAliasRegistry(canonical.PubnetPassphrase, map[string]string{usdcSACContract: usdcClassic})
	if err != nil {
		t.Fatalf("NewAliasRegistry: %v", err)
	}
	canonical.InstallAliasRegistry(reg)
	t.Cleanup(func() { canonical.InstallAliasRegistry(nil) })

	cat, err := currency.LoadEmbedded()
	if err != nil {
		t.Fatalf("LoadEmbedded: %v", err)
	}
	reader := &stubOracleReader{}
	srv := v1.New(v1.Options{Oracle: reader, VerifiedCurrencies: cat})
	tsrv := httpTestServer(t, srv)

	families := [][]string{
		{"native", "crypto:XLM", canonical.XLMSacContractID},
		{usdcClassic, usdcSACContract},
		{"crypto:EURC", "crypto:EUROC"},
	}
	for _, family := range families {
		for _, member := range family {
			resp := mustGet(t, tsrv.URL+"/v1/oracle/latest?asset="+member)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("?asset=%s status = %d", member, resp.StatusCode)
			}
			bound := make(map[string]bool, len(reader.lastAssets))
			for _, k := range reader.lastAssets {
				bound[k] = true
			}
			for _, want := range family {
				if !bound[want] {
					t.Errorf("?asset=%s bound %v, missing alias %s", member, reader.lastAssets, want)
				}
			}
		}
	}
}

// ─── /v1/oracle/streams handler tests ───

// TestOracleStreams_EmptyArrayWhenReaderNil pins the degradation posture: a nil OracleReader
// returns 200 with `data: []`, not 503, matching /v1/oracle/latest, so the explorer's /oracles
// page renders an empty table rather than an error state.
func TestOracleStreams_EmptyArrayWhenReaderNil(t *testing.T) {
	srv := v1.New(v1.Options{})
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/oracle/streams")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var env struct {
		Data []v1.OracleReading `json:"data"`
	}
	mustDecode(t, resp, &env)
	if len(env.Data) != 0 {
		t.Errorf("data = %+v, want empty array", env.Data)
	}
}

// TestOracleStreams_FiltersToOracleClass pins that rows from non-ClassOracle sources (CoinGecko, ECB) sharing
// oracle_updates must not appear on the public /oracles page; the class is a registry fact, so filtering is at the wire.
func TestOracleStreams_FiltersToOracleClass(t *testing.T) {
	reader := &stubOracleReader{
		updates: []canonical.OracleUpdate{
			mkReflectorUpdate("reflector-dex", "12000000000000", 14),
			mkReflectorUpdate("redstone", "12100000000000", 14),
			mkReflectorUpdate("band", "12200000000000", 14),
			// Non-oracle classes — must be filtered out.
			mkReflectorUpdate("coingecko", "12050000000000", 14),
			mkReflectorUpdate("ecb", "12060000000000", 14),
		},
	}
	srv := v1.New(v1.Options{Oracle: reader})
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/oracle/streams")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var env struct {
		Data []v1.OracleReading `json:"data"`
	}
	mustDecode(t, resp, &env)

	// 3 oracle sources, 2 non-oracle dropped.
	if len(env.Data) != 3 {
		t.Fatalf("len(data) = %d, want 3 oracle rows", len(env.Data))
	}
	seen := map[string]bool{}
	for _, r := range env.Data {
		seen[r.Source] = true
	}
	wantOracles := []string{"reflector-dex", "redstone", "band"}
	for _, want := range wantOracles {
		if !seen[want] {
			t.Errorf("missing expected oracle %q in response", want)
		}
	}
	for _, notWant := range []string{"coingecko", "ecb"} {
		if seen[notWant] {
			t.Errorf("non-oracle %q leaked into /v1/oracle/streams response", notWant)
		}
	}
}

// TestOracleStreams_RendersPriceAtDeclaredDecimals pins the /v1/oracle/latest scaling contract:
// `price_raw` carries the integer and `price` the decimal-scaled string. Bypassing
// scaledDecimalString on the streams path would serve a price off by the decimals factor.
func TestOracleStreams_RendersPriceAtDeclaredDecimals(t *testing.T) {
	reader := &stubOracleReader{
		updates: []canonical.OracleUpdate{
			mkReflectorUpdate("reflector-dex", "12000000000000", 14),
		},
	}
	srv := v1.New(v1.Options{Oracle: reader})
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/oracle/streams")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var env struct {
		Data []v1.OracleReading `json:"data"`
	}
	mustDecode(t, resp, &env)
	if len(env.Data) != 1 {
		t.Fatalf("len(data) = %d, want 1", len(env.Data))
	}
	r := env.Data[0]
	if r.Price != "0.12000000000000" {
		t.Errorf("price = %q, want 0.12000000000000 (14-decimal scaling)", r.Price)
	}
	if r.PriceRaw != "12000000000000" {
		t.Errorf("price_raw = %q, want the integer value", r.PriceRaw)
	}
	if r.Decimals != 14 {
		t.Errorf("decimals = %d, want 14", r.Decimals)
	}
}

// TestOracleStreams_ReaderError500: an unexpected hypertable scan error surfaces as 500, as in
// /v1/oracle/latest. Only the generic error surface is asserted here; the timeout and
// clientAborted branches need a cancellable context and are covered by handler unit tests.
func TestOracleStreams_ReaderError500(t *testing.T) {
	reader := &stubOracleReader{err: errors.New("scan failed")}
	srv := v1.New(v1.Options{Oracle: reader})
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/oracle/streams")
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", resp.StatusCode)
	}
}

// TestOracleLatest_PriceOnchain pins the wire contract: price_raw
// stays the integer form of price, and an inverted feed's publisher
// integer rides alongside as price_onchain; a non-inverted row omits it.
func TestOracleLatest_PriceOnchain(t *testing.T) {
	inverted := mkReflectorUpdate("redstone", "5747126", 8)
	onchain := canonical.NewAmount(big.NewInt(1_740_000_000))
	inverted.PublishedPrice = &onchain
	plain := mkReflectorUpdate("reflector-cex", "12420000000000", 14)
	reader := &stubOracleReader{updates: []canonical.OracleUpdate{inverted, plain}}
	tsrv := httpTestServer(t, v1.New(v1.Options{Oracle: reader}))

	resp := mustGet(t, tsrv.URL+"/v1/oracle/latest?asset=native")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var body struct {
		Data []map[string]any `json:"data"`
	}
	mustDecode(t, resp, &body)
	if len(body.Data) != 2 {
		t.Fatalf("got %d rows, want 2", len(body.Data))
	}
	for _, row := range body.Data {
		switch row["source"] {
		case "redstone":
			if row["price_raw"] != "5747126" || row["price"] != "0.05747126" {
				t.Errorf("redstone price_raw=%v price=%v, want 5747126 / 0.05747126 (unchanged)", row["price_raw"], row["price"])
			}
			if row["price_onchain"] != "1740000000" {
				t.Errorf("redstone price_onchain = %v, want \"1740000000\"", row["price_onchain"])
			}
		case "reflector-cex":
			if v, ok := row["price_onchain"]; ok {
				t.Errorf("non-inverted row carries price_onchain %v; want absent", v)
			}
		default:
			t.Errorf("unexpected source %v", row["source"])
		}
	}
}

// TestOracleLatest_CacheUnavailable503 — Redis MISCONF surfaces as
// 503 + Retry-After.
func TestOracleLatest_CacheUnavailable503(t *testing.T) {
	reader := &stubOracleReader{err: miscOnfErr}
	srv := v1.New(v1.Options{Oracle: reader})
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/oracle/latest?asset=native")
	assertCacheUnavailable(t, resp)
}

// TestOracleStreams_CacheUnavailable503 — same MISCONF cascade as
// /v1/oracle/latest, on the streams variant.
func TestOracleStreams_CacheUnavailable503(t *testing.T) {
	reader := &stubOracleReader{err: miscOnfErr}
	srv := v1.New(v1.Options{Oracle: reader})
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/oracle/streams")
	assertCacheUnavailable(t, resp)
}

// TestOracleLatest_ImpersonatorGetsNoVerifiedTicker is the identity-gate regression: appending
// `crypto:<CODE>` for ANY classic asset would serve Circle's oracle prices to a USDC-coded token
// from another issuer, which is attacker-authored pricing on an oracle-attributed surface.
func TestOracleLatest_ImpersonatorGetsNoVerifiedTicker(t *testing.T) {
	reader, srv := oracleGateFixture(t)
	ts := httpTestServer(t, srv)

	got := oracleReadings(t, ts.URL+"/v1/oracle/latest?asset="+impersonatorUSDCAssetID)
	if len(got) != 0 {
		t.Fatalf("impersonator returned %d readings (%+v) — it must be answered with"+
			" its OWN rows only; identity is (code, issuer), never code alone", len(got), got)
	}
	if contains(t, reader.asked, "crypto:USDC") {
		t.Errorf("candidate keys = %v; an unverified issuer must never be translated to crypto:USDC", reader.asked)
	}
	if len(reader.asked) != 1 || reader.asked[0] != impersonatorUSDCAssetID {
		t.Errorf("candidate keys = %v, want exactly [%s]", reader.asked, impersonatorUSDCAssetID)
	}
}

// TestOracleLatest_VerifiedIssuerKeepsItsTicker pins the other half: the
// gate must not cost the REAL asset its readings.
func TestOracleLatest_VerifiedIssuerKeepsItsTicker(t *testing.T) {
	reader, srv := oracleGateFixture(t)
	ts := httpTestServer(t, srv)

	got := oracleReadings(t, ts.URL+"/v1/oracle/latest?asset="+circleUSDCAssetID)
	if len(got) != 1 {
		t.Fatalf("verified USDC returned %d readings, want 1", len(got))
	}
	if got[0].Source != "band" || got[0].Asset != "crypto:USDC" {
		t.Errorf("reading = %+v, want the band crypto:USDC row", got[0])
	}
	if got[0].Price != "0.99974000000000" {
		t.Errorf("price = %q, want 0.99974000000000", got[0].Price)
	}
	if !contains(t, reader.asked, "crypto:USDC") {
		t.Errorf("candidate keys = %v, want the crypto:USDC translation", reader.asked)
	}
}

// TestOracleLatest_ReferenceOnlyTickerIsNeverGranted covers the second impersonation shape: USDT
// is a `reference_only` catalogue entry with no Stellar issuance, so EVERY classic `USDT-G…` is an
// impersonator (currency.indexTickerOnlyEntry), and an ungated helper would translate all of them.
func TestOracleLatest_ReferenceOnlyTickerIsNeverGranted(t *testing.T) {
	reader, srv := oracleGateFixture(t)
	ts := httpTestServer(t, srv)

	const fakeUSDT = "USDT-GBNZILSTVQZ4R7IKQDGHYGY2QXL5QOFJYQMXPKWRRM5PAV7Y4M67AQUA"
	oracleReadings(t, ts.URL+"/v1/oracle/latest?asset="+fakeUSDT)
	if contains(t, reader.asked, "crypto:USDT") {
		t.Errorf("candidate keys = %v; a ticker the catalogue verifies as OFF-Stellar"+
			" must never be granted to a classic asset", reader.asked)
	}
}

// TestOracleLatest_NativeStillMapsToXLM guards the behaviour the helper
// exists for — the gate must not break the XLM dual-form expansion.
func TestOracleLatest_NativeStillMapsToXLM(t *testing.T) {
	reader, srv := oracleGateFixture(t)
	ts := httpTestServer(t, srv)

	oracleReadings(t, ts.URL+"/v1/oracle/latest?asset=native")
	if !contains(t, reader.asked, "crypto:XLM") {
		t.Errorf("candidate keys = %v, want native to still expand to crypto:XLM", reader.asked)
	}
}

// TestOracleLatest_VerifiedSACInheritsItsClassicIdentity pins that the derived SAC of a verified asset inherits its
// classic key and ticker; an arbitrary contract inherits neither.
func TestOracleLatest_VerifiedSACInheritsItsClassicIdentity(t *testing.T) {
	reader, srv := oracleGateFixture(t)
	ts := httpTestServer(t, srv)

	got := oracleReadings(t, ts.URL+"/v1/oracle/latest?asset="+circleUSDCSAC)
	if len(got) != 1 || got[0].Asset != "crypto:USDC" {
		t.Fatalf("USDC SAC returned %+v, want the crypto:USDC row", got)
	}
	if !contains(t, reader.asked, circleUSDCAssetID) || !contains(t, reader.asked, "crypto:USDC") {
		t.Errorf("candidate keys = %v, want the classic id and the ticker", reader.asked)
	}

	if got := oracleReadings(t, ts.URL+"/v1/oracle/latest?asset="+unrelatedContractID); len(got) != 0 {
		t.Errorf("unrelated contract returned %d readings, want 0", len(got))
	}
	if len(reader.asked) != 1 || reader.asked[0] != unrelatedContractID {
		t.Errorf("candidate keys = %v, want exactly [%s]", reader.asked, unrelatedContractID)
	}
}

// TestOracleLatest_NoCatalogueFailsClosed pins that with a nil verified catalogue no global ticker is granted, the
// opposite of an ungated helper that would grant one to everybody.
func TestOracleLatest_NoCatalogueFailsClosed(t *testing.T) {
	reader := &keyedOracleReader{rows: map[string][]canonical.OracleUpdate{}}
	srv := v1.New(v1.Options{Oracle: reader, NetworkPassphrase: canonical.PubnetPassphrase})
	ts := httpTestServer(t, srv)

	oracleReadings(t, ts.URL+"/v1/oracle/latest?asset="+circleUSDCAssetID)
	if contains(t, reader.asked, "crypto:USDC") {
		t.Errorf("candidate keys = %v; without a catalogue no asset may claim a ticker", reader.asked)
	}
}

// TestOracleStreams_UnmappedRowsOptIn pins that the unfiltered storage read's raw rows are omitted by default and listed
// with mapped:false under include_unmapped=true, while mapped rows carry mapped:true.
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

// Identity fixtures for the /v1/oracle/latest ticker gate: circleUSDCAssetID is the verified catalogue's USDC issuance;
// impersonatorUSDCAssetID wears the same CODE over AQUA's issuer, which an un-gated helper would let pull Circle's rows.
const (
	circleUSDCAssetID       = "USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
	impersonatorUSDCAssetID = "USDC-GBNZILSTVQZ4R7IKQDGHYGY2QXL5QOFJYQMXPKWRRM5PAV7Y4M67AQUA"
	// The pubnet Stellar Asset Contract of circleUSDCAssetID — pinned
	// by TestResolveSACToClassic_Genuine against the same derivation.
	circleUSDCSAC = "CCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUZPUTHXSTZLEO7SJMI75"
	// A well-formed contract id that is NOT any asset's SAC.
	unrelatedContractID = "CAFJZQWSED6YAWZU3GWRTOCNPPCGBN32L7QV43XX5LZLFTK6JLN34DLN"
)

// keyedOracleReader answers only for the asset keys it is asked for, as `WHERE asset = ANY($1)` does, so the test is
// behavioural: an id never translated to `crypto:USDC` cannot get a crypto:USDC row.
type keyedOracleReader struct {
	rows map[string][]canonical.OracleUpdate
	// asked records the candidate keys the handler expanded to, so a
	// test can also pin the exact translation (not merely its effect).
	asked []string
}

func (r *keyedOracleReader) LatestOracleUpdatesForAsset(ctx context.Context, asset canonical.Asset, src string) ([]canonical.OracleUpdate, error) {
	return r.LatestOracleUpdatesForAssets(ctx, []canonical.Asset{asset}, src)
}

func (r *keyedOracleReader) LatestOracleUpdatesForAssets(_ context.Context, assets []canonical.Asset, _ string) ([]canonical.OracleUpdate, error) {
	r.asked = r.asked[:0]
	var out []canonical.OracleUpdate
	for _, a := range assets {
		r.asked = append(r.asked, a.String())
		out = append(out, r.rows[a.String()]...)
	}
	return out, nil
}

func (r *keyedOracleReader) LatestOracleStreams(context.Context) ([]canonical.OracleUpdate, error) {
	return nil, nil
}

// oracleGateFixture builds a reader with one reading under `crypto:USDC` (how band/redstone/reflector-cex key it) and a
// server with the real embedded catalogue and pubnet passphrase, matching production.
func oracleGateFixture(t *testing.T) (*keyedOracleReader, *v1.Server) {
	t.Helper()
	cat, err := currency.LoadEmbedded()
	if err != nil {
		t.Fatalf("LoadEmbedded: %v", err)
	}
	usd, err := canonical.NewFiatAsset("USD")
	if err != nil {
		t.Fatalf("fiat:USD: %v", err)
	}
	cryptoUSDC, err := canonical.ParseAsset("crypto:USDC")
	if err != nil {
		t.Fatalf("crypto:USDC: %v", err)
	}
	price, _ := new(big.Int).SetString("99974000000000", 10)
	reader := &keyedOracleReader{rows: map[string][]canonical.OracleUpdate{
		"crypto:USDC": {{
			Source:     "band",
			ContractID: "CAVLP5DH2GJPZMVO7IJY4CVOD5MWEFTJFVPD2YY2FQXOQHRGHK4D6HLP",
			Timestamp:  time.Unix(1_772_000_000, 0).UTC(),
			Asset:      cryptoUSDC,
			Quote:      usd,
			Price:      canonical.NewAmount(price),
			Decimals:   14,
		}},
	}}
	srv := v1.New(v1.Options{
		Oracle:             reader,
		VerifiedCurrencies: cat,
		NetworkPassphrase:  canonical.PubnetPassphrase,
	})
	return reader, srv
}

func oracleReadings(t *testing.T, url string) []v1.OracleReading {
	t.Helper()
	resp := mustGet(t, url)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", resp.StatusCode, url)
	}
	var env struct {
		Data []v1.OracleReading `json:"data"`
	}
	mustDecode(t, resp, &env)
	return env.Data
}

func contains(t *testing.T, keys []string, want string) bool {
	t.Helper()
	for _, k := range keys {
		if k == want {
			return true
		}
	}
	return false
}

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

// TestOracleLatest_CreditsLicensedVendor: a per-source row from a licensed aggregator names its vendor; an on-chain oracle
// row carries no credit key.
func TestOracleLatest_CreditsLicensedVendor(t *testing.T) {
	reader := &stubOracleReader{updates: []canonical.OracleUpdate{
		mkReflectorUpdate("coingecko", "12000000000000", 14),
		mkReflectorUpdate("reflector-dex", "12500000000000", 14),
	}}
	ts := httpTestServer(t, v1.New(v1.Options{Oracle: reader}))
	resp := mustGet(t, ts.URL+"/v1/oracle/latest?asset=native")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var page struct {
		Data []v1.OracleReading `json:"data"`
	}
	mustDecode(t, resp, &page)
	credits := map[string]string{}
	for _, r := range page.Data {
		credits[r.Source] = r.Credit
	}
	if credits["coingecko"] != "CoinGecko" || credits["reflector-dex"] != "" {
		t.Errorf("credits = %v, want coingecko credited and reflector-dex bare", credits)
	}
}
