// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/currency"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// Regression suite for the /v1/assets row filters that were parsed,
// validated, and then dropped on the way to the store — two separate
// drops, one per serving path:
//
//   - `q` never reached ListAssetsOptions on the default (no
//     asset_class) path, so `?q=ZZZZNOSUCH` served the identical first
//     page every unfiltered request gets. The unified path had passed Q
//     since S-011; only this path's options bag was missing it.
//   - `type` / `code` / `issuer` never reached the unified path — the
//     handler dispatched on asset_class WITHOUT the parsed filters — so
//     `?asset_class=all&code=AQUA` served the unfiltered baseline.
//
// Same class as #355 (include=sparkline7d dropped in this same
// handler): the parameter is accepted, the answer is plausible, and
// nothing in the 200 reveals that the filter was never applied. These
// tests pin the request→store mapping directly, because that is the
// join the defect lived in — each layer worked on its own.

const (
	aquaCode   = "AQUA"
	aquaIssuer = "GBNZILSTVQZ4R7IKQDGHYGY2QXL5QOFJYQMXPKWRRM5PAV7Y4M67AQUA"
)

// filterCapturingAssets records every ListAssetsOptions the handler
// builds and serves no rows, so a test can assert both what the store
// was asked for and what reached the wire.
type filterCapturingAssets struct {
	AssetsReader
	calls []timescale.ListAssetsOptions
}

func (a *filterCapturingAssets) ListAssetsExt(
	_ context.Context, opts timescale.ListAssetsOptions,
) ([]timescale.AssetRow, error) {
	a.calls = append(a.calls, opts)
	return nil, nil
}

func (a *filterCapturingAssets) GetAssetsPriceHistory24hBatch(
	context.Context, []string,
) (map[string][]timescale.AssetPricePoint, error) {
	return nil, nil
}

func (a *filterCapturingAssets) GetAssetsATHBatch(
	context.Context, []string,
) (map[string]timescale.AssetATH, error) {
	return nil, nil
}

// listingCall returns the one call that fetched a LISTING page: the
// overfetch-by-one (limit+1) the paging handlers issue. The catalogue
// stats fill also calls ListAssetsExt (an exact-issuer twin lookup at
// its own fixed limit), so the test cannot just take calls[0].
func listingCall(t *testing.T, stub *filterCapturingAssets, limit int) timescale.ListAssetsOptions {
	t.Helper()
	for _, c := range stub.calls {
		if c.Limit == limit+1 {
			return c
		}
	}
	t.Fatalf("handler never asked the store for a listing page (calls: %+v)", stub.calls)
	return timescale.ListAssetsOptions{}
}

func serveAssets(t *testing.T, s *Server, target string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	s.handleAssetList(rec, httptest.NewRequest(http.MethodGet, target, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	return rec
}

func decodeAssetRows(t *testing.T, rec *httptest.ResponseRecorder) []AssetDetail {
	t.Helper()
	var env struct {
		Data []AssetDetail `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode envelope: %v (body %s)", err, rec.Body.String())
	}
	return env.Data
}

// TestAssetsQReachesTheStoreOnDefaultPath pins drop (a): the default
// listing accepted `q` and searched nothing.
func TestAssetsQReachesTheStoreOnDefaultPath(t *testing.T) {
	stub := &filterCapturingAssets{}
	s := &Server{assetsReader: stub}
	serveAssets(t, s, "/v1/assets?q=ZZZZNOSUCH&limit=3")
	if got := listingCall(t, stub, 3).Q; got != "ZZZZNOSUCH" {
		t.Errorf("store saw Q = %q, want %q — `q` is parsed and dropped, so "+
			"the search box round-trips to the same unfiltered page", got, "ZZZZNOSUCH")
	}
}

// TestAssetsUnifiedClassicPhaseHonoursFilters pins drop (b) at the
// store boundary: the unified path's classic phase must receive the
// same code / issuer / q the caller sent.
func TestAssetsUnifiedClassicPhaseHonoursFilters(t *testing.T) {
	stub := &filterCapturingAssets{}
	// No catalogue wired → the catalogue phase hands straight over to
	// the classic phase, so the listing call under test is that phase's.
	s := &Server{assetsReader: stub}
	serveAssets(t, s, "/v1/assets?asset_class=all&code="+aquaCode+
		"&issuer="+aquaIssuer+"&q=aqu&limit=3")
	got := listingCall(t, stub, 3)
	if got.Code != aquaCode {
		t.Errorf("store saw Code = %q, want %q", got.Code, aquaCode)
	}
	if got.Issuer != aquaIssuer {
		t.Errorf("store saw Issuer = %q, want %q", got.Issuer, aquaIssuer)
	}
	// `q` off the REQUEST, not a hand-built filters value: this path has
	// always passed Q, and the mapping from the query string to the
	// options bag is the join every one of these filters was dropped in.
	if got.Q != "aqu" {
		t.Errorf("store saw Q = %q, want %q", got.Q, "aqu")
	}
	if got.Type != "" {
		t.Errorf("store saw Type = %q, want empty (no type filter was sent)", got.Type)
	}
}

// TestAssetsUnifiedTypeReachesTheSpine — `type` narrows the listing
// spine, which is classic_assets UNION the traded Soroban-native
// contracts. classic and soroban push down; native and fiat can match
// no spine row and must not cost a round-trip.
func TestAssetsUnifiedTypeReachesTheSpine(t *testing.T) {
	for _, tc := range []struct {
		typ      string
		wantType string
		wantCall bool
	}{
		{typ: "classic", wantType: "classic", wantCall: true},
		{typ: "soroban", wantType: "soroban", wantCall: true},
		{typ: "native", wantCall: false},
		{typ: "fiat", wantCall: false},
	} {
		t.Run(tc.typ, func(t *testing.T) {
			stub := &filterCapturingAssets{}
			s := &Server{assetsReader: stub}
			serveAssets(t, s, "/v1/assets?asset_class=all&type="+tc.typ+"&limit=3")
			var listing *timescale.ListAssetsOptions
			for i, c := range stub.calls {
				if c.Limit == 4 {
					listing = &stub.calls[i]
				}
			}
			switch {
			case tc.wantCall && listing == nil:
				t.Fatalf("type=%s never reached the store", tc.typ)
			case tc.wantCall && listing.Type != tc.wantType:
				t.Errorf("store saw Type = %q, want %q", listing.Type, tc.wantType)
			case !tc.wantCall && listing != nil:
				t.Errorf("type=%s hit the listing spine (%+v); no spine row can "+
					"carry it, so the phase must fold without a round-trip", tc.typ, *listing)
			}
		})
	}
}

// TestAssetsUnifiedCataloguePhaseHonoursFilters — the catalogue phase
// serves the curated rows that OPEN the unified page (they suppress
// their classic twins), so a filter that stops at the classic phase
// still returns a page full of rows the caller excluded.
func TestAssetsUnifiedCataloguePhaseHonoursFilters(t *testing.T) {
	cat, err := currency.LoadEmbedded()
	if err != nil {
		t.Fatalf("LoadEmbedded: %v", err)
	}
	stub := &filterCapturingAssets{}
	s := &Server{assetsReader: stub, verifiedCurrencies: cat}

	t.Run("code", func(t *testing.T) {
		rows := decodeAssetRows(t, serveAssets(t, s, "/v1/assets?asset_class=all&code="+aquaCode+"&limit=25"))
		for _, row := range rows {
			if row.Code != aquaCode {
				t.Fatalf("code=%s served %q (slug %q) — the catalogue phase ignored the filter",
					aquaCode, row.Code, row.Slug)
			}
		}
	})

	t.Run("issuer", func(t *testing.T) {
		rows := decodeAssetRows(t, serveAssets(t, s, "/v1/assets?asset_class=all&issuer="+aquaIssuer+"&limit=25"))
		for _, row := range rows {
			if row.Slug != "aqua" {
				t.Fatalf("issuer=%s served slug %q — the catalogue phase ignored the filter",
					aquaIssuer, row.Slug)
			}
		}
	})

	t.Run("native is XLM alone", func(t *testing.T) {
		// The catalogue's only native entry is XLM (asset_id "native");
		// every other Stellar-issued entry is a classic credit.
		rows := decodeAssetRows(t, serveAssets(t, s, "/v1/assets?asset_class=all&type=native&limit=25"))
		if len(rows) != 1 || rows[0].Slug != "xlm" {
			slugs := make([]string, 0, len(rows))
			for _, row := range rows {
				slugs = append(slugs, row.Slug)
			}
			t.Fatalf("type=native served %v, want [xlm]", slugs)
		}
	})
}

// TestAssetsUnifiedFilterMatchingNothingServesEmptyArray — a `type` the
// listing spine cannot carry ends the classic phase before the reader,
// and that empty phase is what becomes the envelope's `data`. It must
// reach the wire as `[]`: AssetListEnvelope.data is `type: array` under
// `required`, so a nil slice would publish `"data": null` on a 200 —
// the same shape of harm as the dropped filter this suite pins, a
// response the caller cannot tell is wrong.
//
// Asserted on the RAW BODY, never through a []AssetDetail decode: JSON
// null unmarshals into a nil slice without error, so a decode reads
// `null` and `[]` identically.
func TestAssetsUnifiedFilterMatchingNothingServesEmptyArray(t *testing.T) {
	cat, err := currency.LoadEmbedded()
	if err != nil {
		t.Fatalf("LoadEmbedded: %v", err)
	}
	for _, tc := range []struct {
		name      string
		target    string
		catalogue *currency.Catalogue
	}{
		// With a catalogue wired: the catalogue phase narrows to zero
		// entries and hands over to the classic phase.
		{name: "fiat", target: "/v1/assets?asset_class=all&type=fiat&limit=25", catalogue: cat},
		// Without one: the classic phase serves the whole request.
		{name: "native no catalogue", target: "/v1/assets?asset_class=all&type=native&limit=25"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := New(Options{AssetsReader: &filterCapturingAssets{}, VerifiedCurrencies: tc.catalogue})
			body := serveAssets(t, s, tc.target).Body.String()
			if !strings.Contains(body, `"data":[]`) {
				t.Errorf("body must carry \"data\":[] for a filter that matches nothing; got %s", body)
			}
			if strings.Contains(body, `"data":null`) {
				t.Errorf("\"data\":null violates the AssetListEnvelope required-array contract; got %s", body)
			}
		})
	}
}

// armFilteringAssets is an in-memory listing spine that narrows exactly
// as buildAssetsQuery does: `classic` keeps the rows with a G-issuer,
// `soroban` the rows without one (the two arms are told apart by that
// column and nothing else), and `issuer` is exact equality — which is
// the predicate lookupCatalogueTwin leans on, so a catalogue row's
// stats fill resolves here the way it does against the store. It exists
// so a test can observe what a type-filtered page REPORTS, not just
// what the store was asked for.
type armFilteringAssets struct {
	AssetsReader
	rows []timescale.AssetRow
}

func (a *armFilteringAssets) ListAssetsExt(
	_ context.Context, opts timescale.ListAssetsOptions,
) ([]timescale.AssetRow, error) {
	out := make([]timescale.AssetRow, 0, len(a.rows))
	for _, row := range a.rows {
		switch {
		case opts.Type == "classic" && row.IssuerGStrkey == "":
			continue
		case opts.Type == "soroban" && row.IssuerGStrkey != "":
			continue
		case opts.Issuer != "" && row.IssuerGStrkey != opts.Issuer:
			continue
		}
		out = append(out, row)
	}
	return out, nil
}

// The per-row enrichments the catalogue phase fans out (price, native
// row, batched history / ATH) are best-effort and irrelevant here, but
// they are METHODS: left to the embedded nil interface they panic into
// the fan-out's recover and bury the assertion in a stack trace. An
// error is the honest "this store has no such row".
func (a *armFilteringAssets) GetAssetByAssetID(
	_ context.Context, assetID string,
) (timescale.AssetRow, error) {
	for _, row := range a.rows {
		if row.AssetID == assetID {
			return row, nil
		}
	}
	return timescale.AssetRow{}, sql.ErrNoRows
}

func (a *armFilteringAssets) GetNativeAssetRow(context.Context) (timescale.AssetRow, error) {
	return timescale.AssetRow{}, sql.ErrNoRows
}

func (a *armFilteringAssets) GetAssetsPriceHistory24hBatch(
	context.Context, []string,
) (map[string][]timescale.AssetPricePoint, error) {
	return nil, nil
}

func (a *armFilteringAssets) GetAssetsATHBatch(
	context.Context, []string,
) (map[string]timescale.AssetATH, error) {
	return nil, nil
}

// A SAC-wrapped asset the verified catalogue does NOT carry — the
// ordinary case on this listing, since the catalogue curates a couple
// of dozen entries against a ~199K-row spine. Its classic twin
// therefore reaches the wire from the classic phase, where the alias
// fold runs, rather than from the catalogue phase, where it does not.
const (
	spineIssuer  = "GBRPYHIL2CI3FNQ4BXLFMNDLFJUNPU2HY3ZMFSHONUCEOASW7QC7OX2H"
	spineClassic = "TESTX-" + spineIssuer
	spineSAC     = "CA22XCXSINOJTOQGKTLTEUXNKE7Z4IEBO5XNHPX7MRAEPOD3OAJMHY5R"
)

// installSpineFoldRegistry installs a process AliasRegistry carrying the
// uncatalogued classic↔SAC family above. NOT parallel: the registry is
// process-global (see canonical.InstallAliasRegistry).
func installSpineFoldRegistry(t *testing.T) {
	t.Helper()
	reg, err := canonical.NewAliasRegistry(map[string]string{spineSAC: "TESTX:" + spineIssuer})
	if err != nil {
		t.Fatalf("NewAliasRegistry: %v", err)
	}
	canonical.InstallAliasRegistry(reg)
	t.Cleanup(func() { canonical.InstallAliasRegistry(nil) })
}

// shippedServer builds the server the way cmd/stellarindex-api/main.go
// does — WITH the embedded verified-currency catalogue. Every fold /
// filter contract below is asserted in that configuration and no other:
// the catalogue phase OPENS the unified listing and suppresses the
// classic twins of the rows it serves, so a server built without it
// exercises a code path the binary never runs.
func shippedServer(t *testing.T, spine AssetsReader) *Server {
	t.Helper()
	cat, err := currency.LoadEmbedded()
	if err != nil {
		t.Fatalf("LoadEmbedded: %v", err)
	}
	return New(Options{AssetsReader: spine, VerifiedCurrencies: cat})
}

// rowByAssetID returns the served row with the given asset_id.
func rowByAssetID(t *testing.T, rows []AssetDetail, assetID string) AssetDetail {
	t.Helper()
	for _, row := range rows {
		if row.AssetID == assetID {
			return row
		}
	}
	t.Fatalf("asset_id %q never reached the wire; page carried %v", assetID, assetIDsOf(rows))
	return AssetDetail{}
}

// assetIDsOf renders a page as its asset ids. A failure message wants
// the identities, not 50 nil fields per row.
func assetIDsOf(rows []AssetDetail) []string {
	ids := make([]string, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, row.AssetID)
	}
	return ids
}

// volumeOf renders a row's volume_24h_usd for an assertion message.
func volumeOf(row AssetDetail) string {
	if row.VolumeUSD24h == nil {
		return "<nil>"
	}
	return *row.VolumeUSD24h
}

// TestAssetsUnifiedTypeClassicReportsClassicArmVolume pins the decided
// contract for a SPINE-served SAC-wrapped asset's money under
// `type=classic`, in the shipped configuration (catalogue wired).
//
// The unified listing folds a SAC wrapper's trailing-24h volume onto its
// canonical classic row, so the unfiltered page reports the SUM. Every
// row filter narrows the spine BEFORE that fold, and the SAC twin lives
// in the contract arm, so `type=classic` reports the classic arm alone —
// a materially lower number for the same asset, deliberately, and
// documented on the four row filters in the OpenAPI spec.
//
// Pushdown, not fold-then-filter: the spine is ~199K rows walked on a
// keyset cursor, so filtering after the fold means fetching the
// UNFILTERED page and dropping rows in Go — a request-path scan of the
// shape #43 moved out of handlers. `code` / `issuer` / `q` are indexed
// classic_assets columns and have no other option regardless.
func TestAssetsUnifiedTypeClassicReportsClassicArmVolume(t *testing.T) {
	installSpineFoldRegistry(t)
	// Volume-desc order, which is the order the classic phase reads in:
	// the canonical row must precede its alias for the fold to merge.
	// limit=200, comfortably past the curated catalogue: the catalogue
	// phase OPENS the page, so a limit near its size would leave no room
	// for the classic phase this test is about.
	s := shippedServer(t, &armFilteringAssets{rows: []timescale.AssetRow{
		{AssetID: spineClassic, Code: "TESTX", IssuerGStrkey: spineIssuer, Volume24hUSD: strp("35600000")},
		{AssetID: spineSAC, Volume24hUSD: strp("9000000")},
	}})

	for _, tc := range []struct {
		name   string
		target string
		want   string
	}{
		{
			name:   "unfiltered folds the SAC volume in",
			target: "/v1/assets?asset_class=all&limit=200",
			want:   "44600000",
		},
		{
			name:   "type=classic reports the classic arm alone",
			target: "/v1/assets?asset_class=all&type=classic&limit=200",
			want:   "35600000",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			row := rowByAssetID(t, decodeAssetRows(t, serveAssets(t, s, tc.target)), spineClassic)
			if got := volumeOf(row); got != tc.want {
				t.Errorf("volume_24h_usd = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestAssetsUnifiedTypeSorobanServesTheContractArm — `asset_class=all&
// type=soroban` must serve the SAC rows the spine holds, with their own
// volume, and must serve the SAME rows as the identical filter without
// `asset_class=all`.
//
// The alias fold suppresses an alias row whose canonical primary is
// absent from the page ("otherwise suppress the stray twin"), which
// assumes the canonical row could have been on it. `type=soroban`
// excludes every canonical classic row by the caller's own predicate, so
// with the fold running unconditionally EVERY row of the page qualified
// as a stray and the phase answered `{"data":[]}` over the exact rows
// the store returned — the drop this whole suite exists to prevent,
// reintroduced one layer further in.
func TestAssetsUnifiedTypeSorobanServesTheContractArm(t *testing.T) {
	installSpineFoldRegistry(t)
	s := shippedServer(t, &armFilteringAssets{rows: []timescale.AssetRow{
		{AssetID: spineClassic, Code: "TESTX", IssuerGStrkey: spineIssuer, Volume24hUSD: strp("35600000")},
		{AssetID: spineSAC, Volume24hUSD: strp("9000000")},
	}})

	unified := decodeAssetRows(t, serveAssets(t, s, "/v1/assets?asset_class=all&type=soroban&limit=200"))
	// Parity first: asset_class is the major dispatch, not a row filter,
	// so it must not change which rows `type=soroban` admits. The default
	// path never folds, so a divergence here IS the fold eating the page.
	plain := decodeAssetRows(t, serveAssets(t, s, "/v1/assets?type=soroban&limit=200"))
	if len(plain) != len(unified) {
		t.Fatalf("type=soroban served %d rows and asset_class=all&type=soroban served %d — "+
			"the same filter must admit the same rows on both paths (%v vs %v)",
			len(plain), len(unified), assetIDsOf(plain), assetIDsOf(unified))
	}
	for i := range plain {
		if plain[i].AssetID != unified[i].AssetID || volumeOf(plain[i]) != volumeOf(unified[i]) {
			t.Errorf("row %d differs between the two paths: %q/%s vs %q/%s",
				i, plain[i].AssetID, volumeOf(plain[i]), unified[i].AssetID, volumeOf(unified[i]))
		}
	}

	if len(unified) != 1 {
		t.Fatalf("asset_class=all&type=soroban served %d rows, want the 1 contract-arm row: %v",
			len(unified), assetIDsOf(unified))
	}
	if got := volumeOf(rowByAssetID(t, unified, spineSAC)); got != "9000000" {
		t.Errorf("volume_24h_usd = %q, want the wrapper's own %q", got, "9000000")
	}
}

// TestAssetsCatalogueRowVolumeIsItsClassicArm pins the OTHER half of the
// published volume contract — the half that only exists when the
// catalogue is wired, which is every configuration the binary ships.
//
// A verified-catalogue slug's unified row is served by the CATALOGUE
// phase, whose analytics come from lookupCatalogueTwin: an exact-ISSUER
// lookup of the classic twin. A SAC wrapper has no issuer, so that
// lookup can only ever return the classic arm, and foldAliasTwins is not
// on the path at all (it runs on the classic phase, whose copy of the
// same twin suppressCatalogueTwins then drops). So a catalogue row's
// volume_24h_usd is its classic arm on a filtered request and an
// unfiltered one alike — NOT the cross-arm sum a spine-served asset
// reports unfiltered. The spec says so on all four row filters; this is
// what makes that sentence true rather than aspirational.
func TestAssetsCatalogueRowVolumeIsItsClassicArm(t *testing.T) {
	installFoldRegistry(t)
	// foldUSDCIssuer IS the catalogue's USDC issuance, so this asset is
	// served by the catalogue phase — the case the spine-served test
	// above cannot reach.
	s := shippedServer(t, &armFilteringAssets{rows: []timescale.AssetRow{
		{AssetID: foldUSDCClassic, Code: "USDC", IssuerGStrkey: foldUSDCIssuer, Volume24hUSD: strp("35600000")},
		{AssetID: foldUSDCSAC, Volume24hUSD: strp("9000000")},
	}})

	// The classic arm alone. NOT "44600000": that is the cross-arm sum,
	// which this endpoint publishes only for a SPINE-served asset on an
	// unfiltered request.
	const wantClassicArm = "35600000"
	// limit=200, comfortably past the curated catalogue, so the page
	// still reaches the classic phase as the seed grows.
	for _, target := range []string{
		"/v1/assets?asset_class=all&limit=200",
		"/v1/assets?asset_class=all&type=classic&limit=200",
	} {
		t.Run(target, func(t *testing.T) {
			row := rowByAssetID(t, decodeAssetRows(t, serveAssets(t, s, target)), "usdc")
			if got := volumeOf(row); got != wantClassicArm {
				t.Errorf("catalogue row volume_24h_usd = %q, want %q — a catalogue row's "+
					"stats come from an exact-issuer twin lookup that cannot reach the "+
					"contract arm, filtered or not", got, wantClassicArm)
			}
		})
	}
}

// TestAssetsUnknownTypeIsRefusedNotServedUnfiltered — `?type=bogus` must
// 400 and never reach the store. The listing answers every request with
// a plausible 200 over ~199k rows, so a `type` that fell through would
// be served as a narrowed page the caller has no way to question. The
// store refuses the same value on its own account (see
// TestBuildAssetsQuery_UnknownTypeIsRefused); this pins the edge that
// keeps it from ever getting there.
func TestAssetsUnknownTypeIsRefusedNotServedUnfiltered(t *testing.T) {
	for _, target := range []string{
		"/v1/assets?type=bogus&limit=3",
		"/v1/assets?asset_class=all&type=bogus&limit=3",
	} {
		t.Run(target, func(t *testing.T) {
			stub := &filterCapturingAssets{}
			s := New(Options{AssetsReader: stub})
			rec := httptest.NewRecorder()
			s.handleAssetList(rec, httptest.NewRequest(http.MethodGet, target, nil))
			if rec.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400 (body %s)", rec.Code, rec.Body.String())
			}
			if len(stub.calls) != 0 {
				t.Errorf("an unrecognised type reached the store: %+v", stub.calls)
			}
		})
	}
}
