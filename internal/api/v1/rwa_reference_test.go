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
	"errors"
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
	err     error
	calls   int
}

func (r *rwaOracleStub) LatestOracleStreams(context.Context) ([]canonical.OracleUpdate, error) {
	r.calls++
	if r.err != nil {
		return nil, r.err
	}
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
	// The curated bindings travel with the rows so a consumer can audit
	// every pair this surface is willing to compare.
	var bound bool
	for _, b := range v.Definition.BoundInstruments {
		if b.Code == "USTRY" && b.Issuer == rwaGoodIssuer && b.Feed == "rwa:USTRY" {
			bound = true
		}
	}
	if !bound {
		t.Errorf("the binding behind the served figure is not in definition.bound_instruments: %+v",
			v.Definition.BoundInstruments)
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
	if a.Premium.Status != v1.RWAPremiumReferenceUnavailable {
		t.Errorf("premium status = %q, want %q — a deployment that cannot read the oracles "+
			"has not learned that no oracle publishes this instrument",
			a.Premium.Status, v1.RWAPremiumReferenceUnavailable)
	}
	if a.Premium.Pct != nil {
		t.Errorf("premium pct = %q with no oracle wired", *a.Premium.Pct)
	}
	if a.Valuation.Status != "published" {
		t.Errorf("valuation status = %q — the set does not depend on the oracle", a.Valuation.Status)
	}
}

// TestRWAAssets_FailedOracleReadIsNotServedAsAnAbsence is D2 through the
// real handler. A reader wired but erroring is the ordinary production
// failure — a refused connection, a timed-out scan — and it must not
// publish "no oracle publishes a valuation for this instrument" on every
// row. From process start until the first successful read there is
// nothing to carry forward, so this is exactly the window in which the
// wrong status would be served.
func TestRWAAssets_FailedOracleReadIsNotServedAsAnAbsence(t *testing.T) {
	failing := &rwaOracleStub{
		stubOracleReader: &stubOracleReader{},
		err:              errors.New("dial tcp 127.0.0.1:5432: connection refused"),
	}
	srv := rwaServerWithOracle(t,
		[]timescale.Sep1BoundCurrency{rwaBound("USTRY", rwaGoodIssuer, "etherfuse.com", "bond")},
		map[string]timescale.DirectoryEntry{rwaGoodIssuer: recognisedIssuer(rwaGoodIssuer, "Etherfuse")},
		map[string][]timescale.AssetRow{
			rwaGoodIssuer: {rwaRow("USTRY", rwaGoodIssuer, sptr("1.0412"), 346312)},
		},
		failing,
	)
	v := getRWA(t, srv)
	if len(v.Assets) != 1 {
		t.Fatalf("assets = %v — the set is served whatever the oracles do", rwaAssetIDs(v))
	}
	a := v.Assets[0]
	if a.Premium.Status != v1.RWAPremiumReferenceUnavailable {
		t.Errorf("premium status = %q, want %q", a.Premium.Status, v1.RWAPremiumReferenceUnavailable)
	}
	if a.Premium.Status == v1.RWAPremiumNoReference {
		t.Error("a failed read was published as a finding about what the oracles carry")
	}
	if a.Reference != nil || a.Premium.Pct != nil {
		t.Errorf("a figure was served from a failed read: ref=%+v pct=%v", a.Reference, a.Premium.Pct)
	}
	if v.Summary.AssetsWithReference != 0 || v.Summary.AssetsCompared != 0 {
		t.Errorf("summary reference/compared = %d/%d on a failed read",
			v.Summary.AssetsWithReference, v.Summary.AssetsCompared)
	}
}

// ─── D3: the reference must be bound to (code, issuer), never a code ──

// rwaOtherRecognisedIssuer is a SECOND directory-recognised issuer that
// also publishes a domain-bound SEP-1 entry for a code an oracle prices.
// Nothing about it is exotic: asset codes are not unique on Stellar, and
// the network holds many accounts issuing tokens called USTRY, BENJI or
// XAU. It exists here because a code-keyed join cannot tell it apart
// from the issuer whose instrument the feed actually tracks.
const rwaOtherRecognisedIssuer = "GAXSPCTVGFIVYGHT7JLJZV57HCN5KUYDJ6DMPLNWUKL7A5A3HKCNW7JW"

// TestRWAAssets_ReferenceIsBoundToTheIssuerNotTheCode is the identity
// rule this whole surface is built on, applied to the figure the last
// change added.
//
// Two recognised issuers each publish a domain-bound SEP-1 entry for
// USTRY. One is the issuer whose instrument the oracle feed tracks; the
// other is an unrelated token that happens to share the ticker. A join
// on the code alone answers BOTH with the same treasury valuation, and
// the unrelated token — trading at $0.20 — is published at an 81%
// discount to a security it has nothing to do with.
//
// That is the attacker-authored-pricing class in a new coordinate:
// identity is (code, issuer), never the code alone.
func TestRWAAssets_ReferenceIsBoundToTheIssuerNotTheCode(t *testing.T) {
	srv := rwaServerWithOracle(t,
		[]timescale.Sep1BoundCurrency{
			rwaBound("USTRY", rwaGoodIssuer, "etherfuse.com", "bond"),
			rwaBound("USTRY", rwaOtherRecognisedIssuer, "example.test", "bond"),
		},
		map[string]timescale.DirectoryEntry{
			rwaGoodIssuer:            recognisedIssuer(rwaGoodIssuer, "Etherfuse"),
			rwaOtherRecognisedIssuer: recognisedIssuer(rwaOtherRecognisedIssuer, "Someone Else"),
		},
		map[string][]timescale.AssetRow{
			rwaGoodIssuer:            {rwaRow("USTRY", rwaGoodIssuer, sptr("1.02033610"), 346312)},
			rwaOtherRecognisedIssuer: {rwaRow("USTRY", rwaOtherRecognisedIssuer, sptr("0.20000000"), 91)},
		},
		rwaOracle(t, rwaOracleRow(t, "redstone", "rwa:USTRY", "fiat:USD", "107403800", 8)),
	)
	v := getRWA(t, srv)
	if len(v.Assets) != 2 {
		t.Fatalf("assets = %v, want both issuers' tokens", rwaAssetIDs(v))
	}
	var other *v1.RWAAsset
	for i := range v.Assets {
		if v.Assets[i].Issuer == rwaOtherRecognisedIssuer {
			other = &v.Assets[i]
		}
	}
	if other == nil {
		t.Fatalf("the second issuer's token is missing: %v", rwaAssetIDs(v))
	}
	if other.Reference != nil {
		t.Errorf("an unrelated issuer's token was given the instrument's valuation on a code match: %+v",
			other.Reference)
	}
	if other.Premium.Pct != nil {
		t.Errorf("premium pct = %q — a false claim about a security this token has nothing to do with",
			*other.Premium.Pct)
	}
	if other.Premium.Status != v1.RWAPremiumNotBound {
		t.Errorf("premium status = %q, want %q", other.Premium.Status, v1.RWAPremiumNotBound)
	}
}

// TestRWAAssets_ReferenceBindingIsNotCaseFolded — the code-keyed join
// folded case, so XAUM matched the XAUm feed. A binding names the exact
// (code, issuer) the chain carries; a case variant under an unbound
// issuer is a different token.
func TestRWAAssets_ReferenceBindingIsNotCaseFolded(t *testing.T) {
	srv := rwaServerWithOracle(t,
		[]timescale.Sep1BoundCurrency{
			rwaBound("CETES", rwaOtherRecognisedIssuer, "example.test", "bond"),
		},
		map[string]timescale.DirectoryEntry{
			rwaOtherRecognisedIssuer: recognisedIssuer(rwaOtherRecognisedIssuer, "Someone Else"),
		},
		map[string][]timescale.AssetRow{
			rwaOtherRecognisedIssuer: {rwaRow("CETES", rwaOtherRecognisedIssuer, sptr("0.20000000"), 91)},
		},
		rwaOracle(t, rwaOracleRow(t, "redstone", "rwa:CETES", "fiat:USD", "6988900", 8)),
	)
	v := getRWA(t, srv)
	if len(v.Assets) != 1 {
		t.Fatalf("assets = %v", rwaAssetIDs(v))
	}
	if v.Assets[0].Reference != nil {
		t.Errorf("an unbound issuer received a bound instrument's valuation: %+v", v.Assets[0].Reference)
	}
	if v.Assets[0].Premium.Status != v1.RWAPremiumNotBound {
		t.Errorf("premium status = %q, want %q", v.Assets[0].Premium.Status, v1.RWAPremiumNotBound)
	}
}
