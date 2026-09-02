// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1_test

import (
	"context"
	"math/big"
	"net/http"
	"testing"
	"time"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/currency"
)

// Identity fixtures for the /v1/oracle/latest ticker gate (#336).
//
// circleUSDCAssetID is the verified catalogue's own USDC issuance
// (internal/currency/data/seed.yaml). impersonatorUSDCAssetID wears the
// same CODE over AQUA's issuer — an asset that does not exist on
// Stellar, and the exact id the 2026-09-02 verifier used to pull
// Circle's four oracle rows out of the un-gated helper.
const (
	circleUSDCAssetID       = "USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
	impersonatorUSDCAssetID = "USDC-GBNZILSTVQZ4R7IKQDGHYGY2QXL5QOFJYQMXPKWRRM5PAV7Y4M67AQUA"
	// The pubnet Stellar Asset Contract of circleUSDCAssetID — pinned
	// by TestResolveSACToClassic_Genuine against the same derivation.
	circleUSDCSAC = "CCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUZPUTHXSTZLEO7SJMI75"
	// A well-formed contract id that is NOT any asset's SAC.
	unrelatedContractID = "CAFJZQWSED6YAWZU3GWRTOCNPPCGBN32L7QV43XX5LZLFTK6JLN34DLN"
)

// keyedOracleReader answers only for the asset keys it is ASKED for —
// the property `WHERE asset = ANY($1)` has in storage. Recording the
// candidate set alone would let a test pass on a mock's bookkeeping; a
// keyed reader makes the assertion behavioural: an id that is never
// translated to `crypto:USDC` cannot be answered with a crypto:USDC row.
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

// oracleGateFixture builds a reader holding one reading under
// `crypto:USDC` (what band / redstone / reflector-cex actually key
// Circle's USDC readings by) and a server with the real embedded
// catalogue + the pubnet passphrase, matching production wiring.
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

// TestOracleLatest_ImpersonatorGetsNoVerifiedTicker is the #336
// regression. Before the identity gate, oracleAssetCandidates appended
// `crypto:<CODE>` for ANY classic asset, so a USDC-coded token issued by
// AQUA's issuer was served Circle's oracle prices — attacker-authored
// pricing on a "priced by Band / Reflector / RedStone" surface.
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

// TestOracleLatest_ReferenceOnlyTickerIsNeverGranted covers the second
// impersonation shape: USDT is a `reference_only` catalogue entry with no
// Stellar issuance at all, so EVERY classic `USDT-G…` is by construction
// an impersonator (currency.indexTickerOnlyEntry). The pre-fix helper
// translated all of them.
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

// TestOracleLatest_VerifiedSACInheritsItsClassicIdentity pins the SAC
// leg: a contract that IS the derived Stellar Asset Contract of a
// verified asset denotes that asset, so it inherits the classic key and
// the ticker. An arbitrary contract inherits neither.
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

// TestOracleLatest_NoCatalogueFailsClosed pins the nil-catalogue
// direction. A deployment with no verified catalogue has no basis on
// which to grant a global ticker, so it grants none — the opposite of
// the pre-fix helper, which granted one to everybody.
func TestOracleLatest_NoCatalogueFailsClosed(t *testing.T) {
	reader := &keyedOracleReader{rows: map[string][]canonical.OracleUpdate{}}
	srv := v1.New(v1.Options{Oracle: reader, NetworkPassphrase: canonical.PubnetPassphrase})
	ts := httpTestServer(t, srv)

	oracleReadings(t, ts.URL+"/v1/oracle/latest?asset="+circleUSDCAssetID)
	if contains(t, reader.asked, "crypto:USDC") {
		t.Errorf("candidate keys = %v; without a catalogue no asset may claim a ticker", reader.asked)
	}
}
