package v1_test

// GET /v1/rwa/assets — the oracle NAV reference and the premium or
// discount to it (#352).
//
// The gap between an instrument's independent valuation and what the
// Stellar market pays for the token is the figure a holder of a
// tokenized treasury actually needs, and it is the one number on this
// surface that neither the chain nor an oracle produces alone. The tests
// below drive it end to end, through the handler a caller reaches.
//
// The per-rule refusals are unit-tested in rwa_reference_internal_test.go.
// What these add is the wiring: that the refusals survive the handler,
// that the scam-flag suppression reaches the reference as well as the
// price, and that the stream read stays off the per-request path.

import (
	"context"
	"math/big"
	"net/http"
	"strings"
	"testing"
	"time"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// rwaOracleStub answers the oracle-stream read and counts the calls, so
// a test can prove the scan stays off the per-request path.
type rwaOracleStub struct {
	*stubOracleReader
	streams []canonical.OracleUpdate
	calls   int
}

func (r *rwaOracleStub) LatestOracleStreams(context.Context) ([]canonical.OracleUpdate, error) {
	r.calls++
	return r.streams, nil
}

// rwaOracleRow builds one oracle observation of an instrument.
func rwaOracleRow(t *testing.T, source, assetID, quoteID, raw string, decimals uint8) canonical.OracleUpdate {
	t.Helper()
	a, err := canonical.ParseAsset(assetID)
	if err != nil {
		t.Fatalf("ParseAsset(%q): %v", assetID, err)
	}
	q, err := canonical.ParseAsset(quoteID)
	if err != nil {
		t.Fatalf("ParseAsset(%q): %v", quoteID, err)
	}
	n, ok := new(big.Int).SetString(raw, 10)
	if !ok {
		t.Fatalf("bad raw price %q", raw)
	}
	return canonical.OracleUpdate{
		Source: source, Timestamp: time.Now().Add(-time.Minute),
		Asset: a, Quote: q, Price: canonical.NewAmount(n), Decimals: decimals,
	}
}

func rwaOracle(t *testing.T, rows ...canonical.OracleUpdate) *rwaOracleStub {
	t.Helper()
	return &rwaOracleStub{stubOracleReader: &stubOracleReader{}, streams: rows}
}

// rwaServerWithOracle is [rwaServer] plus the oracle seam the reference
// is read through.
func rwaServerWithOracle(
	t *testing.T,
	bound []timescale.Sep1BoundCurrency,
	dir map[string]timescale.DirectoryEntry,
	rows map[string][]timescale.AssetRow,
	oracle *rwaOracleStub,
) *v1.Server {
	t.Helper()
	return v1.New(v1.Options{
		Sep1Cache: &stubSep1BoundReader{bound: bound},
		Directory: &stubDirectoryReader{entries: dir},
		Oracle:    oracle,
		AssetsReader: &rwaListStub{
			stubAssetsReaderExt: &stubAssetsReaderExt{},
			byIssuer:            rows,
			supply:              rwaSupplyFor(rows),
		},
	})
}

// TestRWAAssets_ServesTheDiscountToTheInstrumentValuation is the
// end-to-end positive path: an admitted tokenized treasury, an
// independent oracle's valuation of the instrument, and the gap between
// that and what the Stellar market pays.
func TestRWAAssets_ServesTheDiscountToTheInstrumentValuation(t *testing.T) {
	srv := rwaServerWithOracle(t,
		[]timescale.Sep1BoundCurrency{rwaBound("USTRY", rwaGoodIssuer, "etherfuse.com", "bond")},
		map[string]timescale.DirectoryEntry{rwaGoodIssuer: recognisedIssuer(rwaGoodIssuer, "Etherfuse")},
		map[string][]timescale.AssetRow{
			// 1.02033610 against a 1.07403800 valuation — a 5% discount.
			rwaGoodIssuer: {rwaRow("USTRY", rwaGoodIssuer, sptr("1.02033610"), 346312)},
		},
		rwaOracle(t, rwaOracleRow(t, "redstone", "rwa:USTRY", "fiat:USD", "107403800", 8)),
	)
	v := getRWA(t, srv)
	if len(v.Assets) != 1 {
		t.Fatalf("assets = %v", rwaAssetIDs(v))
	}
	a := v.Assets[0]
	if a.Reference == nil {
		t.Fatal("no independent valuation served for an instrument an oracle prices")
	}
	if a.Reference.PriceUSD != "1.07403800" || a.Reference.Source != "redstone" {
		t.Errorf("reference = %+v, want the oracle figure verbatim from its publisher", a.Reference)
	}
	if a.Reference.Feed != "rwa:USTRY" || a.Reference.Quote != "fiat:USD" {
		t.Errorf("reference provenance = %s/%s — the instrument and its denominator travel with the figure",
			a.Reference.Feed, a.Reference.Quote)
	}
	if a.Premium.Status != v1.RWAPremiumPublished {
		t.Fatalf("premium status = %q, want published", a.Premium.Status)
	}
	if a.Premium.Pct == nil || *a.Premium.Pct != "-5.0000" {
		t.Errorf("premium pct = %v, want -5.0000", a.Premium.Pct)
	}
	if v.Summary.AssetsWithReference != 1 || v.Summary.AssetsCompared != 1 {
		t.Errorf("summary reference/compared = %d/%d, want 1/1",
			v.Summary.AssetsWithReference, v.Summary.AssetsCompared)
	}
	if !strings.Contains(v.Summary.Basis, "issuer declares") {
		t.Errorf("the basis does not state what the comparison rests on: %q", v.Summary.Basis)
	}
	if len(v.Definition.ComparableInstrumentCodes) == 0 {
		t.Error("the comparable-instrument vocabulary is not served with the rows")
	}
}

// TestRWAAssets_ImpersonatorGetsNoInstrumentValuation is the
// impersonation case in its sharpest form. An issuer flagged AFTER
// admission keeps its row — this surface hides nothing it admitted — but
// gets no valuation of any kind, INCLUDING a third party's. Handing an
// impersonator the real instrument's oracle NAV would publish a bigger
// claim than the one the flag suppressed.
func TestRWAAssets_ImpersonatorGetsNoInstrumentValuation(t *testing.T) {
	srv := v1.New(v1.Options{
		Sep1Cache: &stubSep1BoundReader{bound: []timescale.Sep1BoundCurrency{
			rwaBound("USTRY", rwaGoodIssuer, "etherfuse.com", "bond"),
		}},
		Directory: &rwaSkewedDirectory{
			membership: timescale.DirectoryEntry{
				Address: rwaGoodIssuer, Name: "Etherfuse",
				Tags: []string{"issuer"}, Source: "stellar-expert",
			},
			rowFill: timescale.DirectoryEntry{
				Address: rwaGoodIssuer, Name: "Etherfuse",
				Tags: []string{"issuer", "malicious"}, Source: "stellar-expert",
			},
		},
		Oracle: rwaOracle(t, rwaOracleRow(t, "redstone", "rwa:USTRY", "fiat:USD", "107403800", 8)),
		AssetsReader: &rwaListStub{
			stubAssetsReaderExt: &stubAssetsReaderExt{},
			byIssuer: map[string][]timescale.AssetRow{
				rwaGoodIssuer: {rwaRow("USTRY", rwaGoodIssuer, sptr("1.02033610"), 346312)},
			},
			supply: map[string]string{"USTRY-" + rwaGoodIssuer: "12336218000000"},
		},
	})
	v := getRWA(t, srv)
	if len(v.Assets) != 1 {
		t.Fatalf("assets = %v — an admitted row is not removed when the flag lands", rwaAssetIDs(v))
	}
	a := v.Assets[0]
	if a.Valuation.Status != "withheld_issuer_flagged" {
		t.Fatalf("valuation status = %q, want withheld_issuer_flagged", a.Valuation.Status)
	}
	if a.Reference != nil {
		t.Errorf("a flagged issuer's token was handed an independent instrument valuation: %+v", a.Reference)
	}
	if a.Premium.Status != v1.RWAPremiumIssuerFlagged {
		t.Errorf("premium status = %q, want %q", a.Premium.Status, v1.RWAPremiumIssuerFlagged)
	}
	if a.Premium.Pct != nil {
		t.Errorf("premium pct = %q on a flagged issuer", *a.Premium.Pct)
	}
	if v.Summary.AssetsWithReference != 0 || v.Summary.AssetsCompared != 0 {
		t.Errorf("summary counted a flagged row as valued: reference/compared = %d/%d",
			v.Summary.AssetsWithReference, v.Summary.AssetsCompared)
	}
}

// TestRWAAssets_UnpricedAssetIsNotComparedToZero. An instrument no
// Stellar market prices keeps its independent valuation and reports the
// comparison as unmade. A premium of "0" there would read as "trades at
// par", which is the one reading that is certainly wrong.
func TestRWAAssets_UnpricedAssetIsNotComparedToZero(t *testing.T) {
	srv := rwaServerWithOracle(t,
		[]timescale.Sep1BoundCurrency{rwaBound("TESOURO", rwaGoodIssuer, "etherfuse.com", "bond")},
		map[string]timescale.DirectoryEntry{rwaGoodIssuer: recognisedIssuer(rwaGoodIssuer, "Etherfuse")},
		map[string][]timescale.AssetRow{
			rwaGoodIssuer: {rwaRow("TESOURO", rwaGoodIssuer, nil, 13804)},
		},
		rwaOracle(t, rwaOracleRow(t, "redstone", "rwa:TESOURO", "fiat:USD", "24538100", 8)),
	)
	v := getRWA(t, srv)
	if len(v.Assets) != 1 {
		t.Fatalf("assets = %v", rwaAssetIDs(v))
	}
	a := v.Assets[0]
	if a.Valuation.Status != "unpriced" || a.Valuation.PriceUSD != nil {
		t.Fatalf("valuation = %+v, want unpriced with no figure", a.Valuation)
	}
	if a.Reference == nil || a.Reference.PriceUSD != "0.24538100" {
		t.Fatalf("reference = %+v — an unpriced token still has an independent valuation", a.Reference)
	}
	if a.Premium.Status != v1.RWAPremiumNoMarketPrice {
		t.Errorf("premium status = %q, want %q", a.Premium.Status, v1.RWAPremiumNoMarketPrice)
	}
	if a.Premium.Pct != nil {
		t.Errorf("premium pct = %q, want absent — an unmade comparison is not par", *a.Premium.Pct)
	}
	if v.Summary.AssetsWithReference != 1 || v.Summary.AssetsCompared != 0 {
		t.Errorf("summary reference/compared = %d/%d, want 1/0",
			v.Summary.AssetsWithReference, v.Summary.AssetsCompared)
	}
}

// TestRWAAssets_SpotFeedIsNotServedAsATokenValuation carries the
// unit-scope refusal through the handler. `rwa:XAU` is spot gold per
// troy ounce; a token coded XAU is a token of unstated size, and their
// ratio published as a percentage would read as a 99.99% discount on an
// ordinary token.
func TestRWAAssets_SpotFeedIsNotServedAsATokenValuation(t *testing.T) {
	srv := rwaServerWithOracle(t,
		[]timescale.Sep1BoundCurrency{rwaBound("XAU", rwaGoodIssuer, "etherfuse.com", "commodity")},
		map[string]timescale.DirectoryEntry{rwaGoodIssuer: recognisedIssuer(rwaGoodIssuer, "Etherfuse")},
		map[string][]timescale.AssetRow{
			rwaGoodIssuer: {rwaRow("XAU", rwaGoodIssuer, sptr("0.50000000"), 1438878)},
		},
		rwaOracle(t, rwaOracleRow(t, "reflector-fx", "rwa:XAU", "fiat:USD", "440086022830869146", 14)),
	)
	v := getRWA(t, srv)
	if len(v.Assets) != 1 {
		t.Fatalf("assets = %v", rwaAssetIDs(v))
	}
	a := v.Assets[0]
	if a.Reference != nil {
		t.Errorf("a per-troy-ounce spot price was served as a token's valuation: %+v", a.Reference)
	}
	if a.Premium.Status != v1.RWAPremiumNotInstrumentScoped {
		t.Errorf("premium status = %q, want %q", a.Premium.Status, v1.RWAPremiumNotInstrumentScoped)
	}
	if a.Premium.Pct != nil {
		t.Errorf("premium pct = %q — a unit conversion must never be served as a discount", *a.Premium.Pct)
	}
}

// TestRWAAssets_ReferenceSnapshotIsCachedNotRefetchedPerRequest — the
// stream read is a hypertable scan over every active oracle feed, so one
// snapshot must serve the whole set rather than one read per request.
func TestRWAAssets_ReferenceSnapshotIsCachedNotRefetchedPerRequest(t *testing.T) {
	oracle := rwaOracle(t, rwaOracleRow(t, "redstone", "rwa:USTRY", "fiat:USD", "107403800", 8))
	srv := rwaServerWithOracle(t,
		[]timescale.Sep1BoundCurrency{rwaBound("USTRY", rwaGoodIssuer, "etherfuse.com", "bond")},
		map[string]timescale.DirectoryEntry{rwaGoodIssuer: recognisedIssuer(rwaGoodIssuer, "Etherfuse")},
		map[string][]timescale.AssetRow{
			rwaGoodIssuer: {rwaRow("USTRY", rwaGoodIssuer, sptr("1.02033610"), 1)},
		},
		oracle,
	)
	ts := httpTestServer(t, srv)
	for range 3 {
		resp := mustGet(t, ts.URL+"/v1/rwa/assets")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d", resp.StatusCode)
		}
		_ = resp.Body.Close()
	}
	if oracle.calls != 1 {
		t.Errorf("LatestOracleStreams called %d times; the TTL cache should scan once", oracle.calls)
	}
}

// TestRWAAssets_NoOracleReaderStillServesTheSet. The reference is an
// addition to the surface, not a precondition for it: a deployment
// without an oracle reader serves the set with the comparison reported
// as unavailable — never as a zero, never as an error.
func TestRWAAssets_NoOracleReaderStillServesTheSet(t *testing.T) {
	srv := rwaServer(t,
		[]timescale.Sep1BoundCurrency{rwaBound("USTRY", rwaGoodIssuer, "etherfuse.com", "bond")},
		map[string]timescale.DirectoryEntry{rwaGoodIssuer: recognisedIssuer(rwaGoodIssuer, "Etherfuse")},
		map[string][]timescale.AssetRow{
			rwaGoodIssuer: {rwaRow("USTRY", rwaGoodIssuer, sptr("1.0412"), 346312)},
		},
	)
	v := getRWA(t, srv)
	if len(v.Assets) != 1 {
		t.Fatalf("assets = %v", rwaAssetIDs(v))
	}
	a := v.Assets[0]
	if a.Reference != nil {
		t.Errorf("reference served with no oracle wired: %+v", a.Reference)
	}
	if a.Premium.Status == "" {
		t.Error("premium carries no status — an absent comparison must still say so")
	}
	if a.Premium.Pct != nil {
		t.Errorf("premium pct = %q with no oracle wired", *a.Premium.Pct)
	}
	if a.Valuation.Status != "published" {
		t.Errorf("valuation status = %q — the set does not depend on the oracle", a.Valuation.Status)
	}
}
