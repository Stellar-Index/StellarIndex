package v1_test

// GET /v1/rwa/assets — the tokenized-real-world-asset surface (#352).
//
// The tests below are driven by the shape of the production data
// measured on 2026-09-05, because that shape is what makes the
// definition load-bearing rather than decorative:
//
//   - 130 issuers publish a domain-bound SEP-1 anchor_asset_type naming
//     a real-world instrument. 128 of them carry the `malicious`
//     directory tag, publishing from lookalike domains
//     (nasdaq.com.co, cboe.com.co, spglobal.com.co, blackrock.co.com).
//   - The codes an RWA oracle prices are issued many times over: BENJI
//     exists under a Franklin-Templeton-shaped domain AND under a
//     `malicious`-tagged one; XAU exists under a dozen swisscustody
//     lookalikes.
//
// Anything that admits either population is a phishing amplifier, so
// each requirement gets a test that pins the refusal.

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

const (
	// A recognised, non-flagged issuer that declares a real-world
	// anchor type on a bound SEP-1 entry.
	rwaGoodIssuer = "GCRYUGD5NVARGXT56XEZI5CIFCQETYHAPQQTHO2O3IQZTHDH4LATMYWC"
	// An issuer that declares the SAME instrument codes from a
	// lookalike domain and carries scam-class directory tags.
	rwaScamIssuer = "GCUG7ARUFEEUMSL56K7245YCPXPZPOXAY6TSRXZB2JZFBI4DOBVOTUSA"
	// An issuer that declares a real-world anchor type but that no
	// independent party has recognised.
	rwaUnknownIssuer = "GAXSPCTVGFIVYGHT7JLJZV57HCN5KUYDJ6DMPLNWUKL7A5A3HKCNW7JW"
)

// stubSep1BoundReader serves canned issuer-bound SEP-1 entries. It also
// satisfies v1.Sep1CachedReader so it can be wired as Options.Sep1Cache.
type stubSep1BoundReader struct {
	bound []timescale.Sep1BoundCurrency
	err   error
	calls int
}

func (s *stubSep1BoundReader) GetIssuerSep1Cached(context.Context, string) (*timescale.IssuerSep1Cached, error) {
	return nil, nil
}

func (s *stubSep1BoundReader) BoundSep1Currencies(
	_ context.Context, keep timescale.Sep1CurrencyFilter,
) ([]timescale.Sep1BoundCurrency, int, error) {
	s.calls++
	if s.err != nil {
		return nil, 0, s.err
	}
	out := make([]timescale.Sep1BoundCurrency, 0, len(s.bound))
	dropped := 0
	for _, c := range s.bound {
		if keep == nil || keep(c) {
			out = append(out, c)
			continue
		}
		dropped++
	}
	return out, dropped, nil
}

// rwaListStub answers ListAssetsExt from a per-issuer row map, the way
// the real store answers the Issuer-filtered listing query.
type rwaListStub struct {
	*stubAssetsReaderExt
	byIssuer map[string][]timescale.AssetRow
	supply   map[string]string
}

func (l *rwaListStub) ListAssetsExt(
	_ context.Context, opts timescale.ListAssetsOptions,
) ([]timescale.AssetRow, error) {
	return l.byIssuer[opts.Issuer], nil
}

// LatestCirculatingSupply satisfies the optional supply seam
// fillMarketCapsFromSupply type-asserts for, so these tests exercise
// the real market-cap fill rather than a path where every row is
// unvalued for want of a supply reader.
func (l *rwaListStub) LatestCirculatingSupply(context.Context) (map[string]string, error) {
	return l.supply, nil
}

func rwaRow(code, issuer string, price *string, obs int64) timescale.AssetRow {
	return timescale.AssetRow{
		Slug:             code + "-" + issuer,
		AssetID:          code + "-" + issuer,
		Code:             code,
		IssuerGStrkey:    issuer,
		FirstSeenLedger:  55008233,
		LastSeenLedger:   63410221,
		ObservationCount: obs,
		PriceUSD:         price,
		// Comfortably above the dust-liquidity floor, so a suppressed
		// market cap in these tests is always the condition under test
		// and never an incidental dust verdict.
		Volume24hUSD: sptr("8214.55"),
	}
}

// rwaSupplyFor gives every row a circulating supply so the market-cap
// fill has both of its inputs. Membership never reads it — it is
// valuation, and valuation is decided after membership.
func rwaSupplyFor(rows map[string][]timescale.AssetRow) map[string]string {
	out := map[string]string{}
	for _, list := range rows {
		for _, r := range list {
			out[r.AssetID] = "12336218000000"
		}
	}
	return out
}

func rwaBound(code, issuer, domain, anchorType string) timescale.Sep1BoundCurrency {
	return timescale.Sep1BoundCurrency{
		Code:            code,
		Issuer:          issuer,
		HomeDomain:      domain,
		OrgName:         domain,
		Name:            code + " token",
		AnchorAsset:     "US Treasury Notes",
		AnchorAssetType: anchorType,
	}
}

// rwaServer builds a Server with the three seams the surface reads.
func rwaServer(
	t *testing.T,
	bound []timescale.Sep1BoundCurrency,
	dir map[string]timescale.DirectoryEntry,
	rows map[string][]timescale.AssetRow,
) *v1.Server {
	t.Helper()
	return v1.New(v1.Options{
		Sep1Cache: &stubSep1BoundReader{bound: bound},
		Directory: &stubDirectoryReader{entries: dir},
		AssetsReader: &rwaListStub{
			stubAssetsReaderExt: &stubAssetsReaderExt{},
			byIssuer:            rows,
			supply:              rwaSupplyFor(rows),
		},
	})
}

func getRWA(t *testing.T, srv *v1.Server) v1.RWAAssetsView {
	t.Helper()
	ts := httpTestServer(t, srv)
	resp := mustGet(t, ts.URL+"/v1/rwa/assets")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var env struct {
		Data v1.RWAAssetsView `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return env.Data
}

func rwaAssetIDs(v v1.RWAAssetsView) []string {
	out := make([]string, 0, len(v.Assets))
	for _, a := range v.Assets {
		out = append(out, a.AssetID)
	}
	return out
}

func recognisedIssuer(addr, name string) timescale.DirectoryEntry {
	return timescale.DirectoryEntry{
		Address: addr, Name: name, Domain: "etherfuse.com",
		Tags: []string{"issuer"}, Source: "stellar-expert",
	}
}

// TestRWAAssets_AdmitsADeclaredAndRecognisedAsset is the positive path:
// a classic asset whose issuer-bound SEP-1 entry declares `bond` and
// whose issuer the curated directory recognises is served with its
// valuation and the evidence that admitted it.
func TestRWAAssets_AdmitsADeclaredAndRecognisedAsset(t *testing.T) {
	srv := rwaServer(t,
		[]timescale.Sep1BoundCurrency{rwaBound("USTRY", rwaGoodIssuer, "etherfuse.com", "bond")},
		map[string]timescale.DirectoryEntry{rwaGoodIssuer: recognisedIssuer(rwaGoodIssuer, "Etherfuse")},
		map[string][]timescale.AssetRow{
			rwaGoodIssuer: {rwaRow("USTRY", rwaGoodIssuer, sptr("1.0412"), 346312)},
		},
	)
	v := getRWA(t, srv)
	if len(v.Assets) != 1 {
		t.Fatalf("assets = %v, want exactly USTRY", rwaAssetIDs(v))
	}
	a := v.Assets[0]
	if a.AssetID != "USTRY-"+rwaGoodIssuer {
		t.Errorf("asset_id = %q", a.AssetID)
	}
	if a.Issuer != rwaGoodIssuer {
		t.Errorf("issuer = %q — identity must carry the G-address, never the code alone", a.Issuer)
	}
	if a.Basis != "sep1_anchor_declaration" || a.AnchorClass != "bond" {
		t.Errorf("basis/class = %q/%q, want sep1_anchor_declaration/bond", a.Basis, a.AnchorClass)
	}
	if a.IssuerDirectoryName != "Etherfuse" {
		t.Errorf("issuer_directory_name = %q — the independent recognition must be shown", a.IssuerDirectoryName)
	}
	if a.HomeDomain != "etherfuse.com" {
		t.Errorf("home_domain = %q", a.HomeDomain)
	}
	if a.FirstSeenLedger != 55008233 {
		t.Errorf("first_seen_ledger = %d — the coverage claim needs the genesis-complete first sighting", a.FirstSeenLedger)
	}
	if v.Summary.Assets != 1 || v.Summary.Issuers != 1 {
		t.Errorf("summary assets/issuers = %d/%d, want 1/1", v.Summary.Assets, v.Summary.Issuers)
	}
	if v.Definition.DocumentationURL == "" || len(v.Definition.Requirements) != 4 {
		t.Errorf("definition not served with the rows: %+v", v.Definition)
	}
}

// TestRWAAssets_RefusesAssetsFailingTheDefinition is the regression that
// matters most. Four assets are attested; three fail a requirement and
// MUST be absent, not merely downranked:
//
//   - the same instrument code from a scam-flagged lookalike issuer,
//   - a real-world declaration from an issuer nobody recognises,
//   - a recognised issuer's token that declares `crypto` — outside the
//     closed real-world vocabulary — and whose code no oracle prices.
//
// Each was checked to fail on its own, so a single over-broad rule
// cannot make the whole assertion pass.
func TestRWAAssets_RefusesAssetsFailingTheDefinition(t *testing.T) {
	bound := []timescale.Sep1BoundCurrency{
		rwaBound("USTRY", rwaGoodIssuer, "etherfuse.com", "bond"),
		rwaBound("USTRY", rwaScamIssuer, "stellar.us.org", "bond"),
		rwaBound("BENJI", rwaUnknownIssuer, "franklintempleton.reallumens.com", "bond"),
		rwaBound("MEME", rwaGoodIssuer, "etherfuse.com", "crypto"),
	}
	dir := map[string]timescale.DirectoryEntry{
		rwaGoodIssuer: recognisedIssuer(rwaGoodIssuer, "Etherfuse"),
		rwaScamIssuer: {
			Address: rwaScamIssuer, Name: "Fake", Domain: "stellar.us.org",
			Tags: []string{"issuer", "malicious", "unsafe"}, Source: "stellar-expert",
		},
		// rwaUnknownIssuer is absent from the directory entirely.
	}
	rows := map[string][]timescale.AssetRow{
		rwaGoodIssuer: {
			rwaRow("USTRY", rwaGoodIssuer, sptr("1.0412"), 346312),
			rwaRow("MEME", rwaGoodIssuer, sptr("0.02"), 12),
		},
		rwaScamIssuer:    {rwaRow("USTRY", rwaScamIssuer, sptr("1.0412"), 13705)},
		rwaUnknownIssuer: {rwaRow("BENJI", rwaUnknownIssuer, sptr("1.1408"), 8299)},
	}
	v := getRWA(t, rwaServer(t, bound, dir, rows))

	got := rwaAssetIDs(v)
	if len(got) != 1 || got[0] != "USTRY-"+rwaGoodIssuer {
		t.Fatalf("assets = %v, want only USTRY-%s", got, rwaGoodIssuer)
	}
	for _, a := range v.Assets {
		if a.Issuer == rwaScamIssuer {
			t.Error("a scam-flagged issuer reached the RWA set — the surface is a phishing amplifier")
		}
		if a.Issuer == rwaUnknownIssuer {
			t.Error("an unrecognised issuer reached the set on its own say-so")
		}
		if a.Code == "MEME" {
			t.Error("a crypto-anchored token reached the real-world set")
		}
	}

	refused := map[string]int{}
	for _, r := range v.Refused {
		refused[r.Reason] = r.Assets
	}
	if refused["issuer_scam_flagged"] != 1 {
		t.Errorf("refused[issuer_scam_flagged] = %d, want 1 (%v)", refused["issuer_scam_flagged"], v.Refused)
	}
	if refused["issuer_not_independently_recognised"] != 1 {
		t.Errorf("refused[issuer_not_independently_recognised] = %d, want 1 (%v)",
			refused["issuer_not_independently_recognised"], v.Refused)
	}
	if refused["no_real_world_instrument_basis"] != 1 {
		t.Errorf("refused[no_real_world_instrument_basis] = %d, want 1 (%v)",
			refused["no_real_world_instrument_basis"], v.Refused)
	}
}

// TestRWAAssets_OracleBasisNeedsARecognisedIssuer pins the one arm that
// matches on a CODE. An RWA oracle prices an instrument called XAU;
// dozens of Stellar accounts issue a token called XAU. The oracle basis
// admits one only when an independent party has already recognised the
// issuing account, and it never invents an anchor class.
func TestRWAAssets_OracleBasisNeedsARecognisedIssuer(t *testing.T) {
	bound := []timescale.Sep1BoundCurrency{
		{Code: "XAU", Issuer: rwaGoodIssuer, HomeDomain: "xau.cl", Name: "XAU"},
		{Code: "XAU", Issuer: rwaScamIssuer, HomeDomain: "swisscustody.net", Name: "XAU"},
	}
	dir := map[string]timescale.DirectoryEntry{
		rwaGoodIssuer: recognisedIssuer(rwaGoodIssuer, "XAU CL"),
		rwaScamIssuer: {
			Address: rwaScamIssuer, Tags: []string{"malicious", "unsafe"}, Source: "stellar-expert",
		},
	}
	rows := map[string][]timescale.AssetRow{
		rwaGoodIssuer: {rwaRow("XAU", rwaGoodIssuer, sptr("4115.67"), 1436959)},
		rwaScamIssuer: {rwaRow("XAU", rwaScamIssuer, sptr("4115.67"), 57775)},
	}
	v := getRWA(t, rwaServer(t, bound, dir, rows))

	if got := rwaAssetIDs(v); len(got) != 1 || got[0] != "XAU-"+rwaGoodIssuer {
		t.Fatalf("assets = %v, want only XAU-%s", got, rwaGoodIssuer)
	}
	a := v.Assets[0]
	if a.Basis != "oracle_rwa_feed" {
		t.Errorf("basis = %q, want oracle_rwa_feed", a.Basis)
	}
	if a.AnchorClass != "" {
		t.Errorf("anchor_class = %q — an oracle feed names an instrument, not a class", a.AnchorClass)
	}
	for _, g := range v.ByClass {
		if g.Class == "unclassified" && g.Assets == 1 {
			return
		}
	}
	t.Errorf("by_class = %+v, want the oracle-basis asset grouped as unclassified", v.ByClass)
}

// TestRWAAssets_UnpricedAssetRendersWithheldNotZero — an asset with no
// served USD price publishes NO market cap and says why. The summary
// total counts it as unvalued and marks itself a lower bound rather
// than absorbing a zero.
func TestRWAAssets_UnpricedAssetRendersWithheldNotZero(t *testing.T) {
	bound := []timescale.Sep1BoundCurrency{
		rwaBound("USTRY", rwaGoodIssuer, "etherfuse.com", "bond"),
		rwaBound("TESOURO", rwaGoodIssuer, "etherfuse.com", "bond"),
	}
	rows := map[string][]timescale.AssetRow{rwaGoodIssuer: {
		rwaRow("USTRY", rwaGoodIssuer, sptr("1.0412"), 346312),
		// No price: the aggregator produced none, or the substance
		// gate withheld it as too thin to aggregate.
		rwaRow("TESOURO", rwaGoodIssuer, nil, 13318),
	}}
	v := getRWA(t, rwaServer(t, bound,
		map[string]timescale.DirectoryEntry{rwaGoodIssuer: recognisedIssuer(rwaGoodIssuer, "Etherfuse")},
		rows))

	if len(v.Assets) != 2 {
		t.Fatalf("assets = %v, want both members", rwaAssetIDs(v))
	}
	var unpriced *v1.RWAAsset
	for i := range v.Assets {
		if v.Assets[i].Code == "TESOURO" {
			unpriced = &v.Assets[i]
		}
	}
	if unpriced == nil {
		t.Fatal("the unpriced member was dropped; it must be served as unavailable, not hidden")
	}
	if unpriced.Valuation.Status != "unpriced" {
		t.Errorf("valuation.status = %q, want unpriced", unpriced.Valuation.Status)
	}
	if unpriced.Valuation.MarketCapUSD != nil {
		t.Errorf("market_cap_usd = %q — an unavailable valuation must be ABSENT, never a number",
			*unpriced.Valuation.MarketCapUSD)
	}
	if unpriced.Valuation.PriceUSD != nil {
		t.Errorf("price_usd = %q, want absent", *unpriced.Valuation.PriceUSD)
	}
	if v.Assets[0].Code != "USTRY" {
		t.Errorf("assets[0] = %q — an unvalued row must never rank above a valued one", v.Assets[0].Code)
	}
	if v.Summary.AssetsUnvalued != 1 || !v.Summary.LowerBound {
		t.Errorf("summary unvalued/lower_bound = %d/%v, want 1/true",
			v.Summary.AssetsUnvalued, v.Summary.LowerBound)
	}
}

// TestRWAAssets_NoPublishedValuationOmitsTheTotal — when nothing in the
// set publishes a market cap, the summary carries NO total. Serving
// "0.00" would read as a real total of zero dollars, which is the one
// reading that is certainly wrong.
func TestRWAAssets_NoPublishedValuationOmitsTheTotal(t *testing.T) {
	v := getRWA(t, rwaServer(t,
		[]timescale.Sep1BoundCurrency{rwaBound("USTRY", rwaGoodIssuer, "etherfuse.com", "bond")},
		map[string]timescale.DirectoryEntry{rwaGoodIssuer: recognisedIssuer(rwaGoodIssuer, "Etherfuse")},
		map[string][]timescale.AssetRow{rwaGoodIssuer: {rwaRow("USTRY", rwaGoodIssuer, nil, 1)}},
	))
	if v.Summary.MarketCapUSD != nil {
		t.Errorf("summary.market_cap_usd = %q, want absent", *v.Summary.MarketCapUSD)
	}
	if v.Summary.AssetsValued != 0 || !v.Summary.LowerBound {
		t.Errorf("summary valued/lower_bound = %d/%v, want 0/true", v.Summary.AssetsValued, v.Summary.LowerBound)
	}
	for _, g := range v.ByClass {
		if g.MarketCapUSD != nil {
			t.Errorf("by_class[%s].market_cap_usd = %q, want absent", g.Class, *g.MarketCapUSD)
		}
	}
}

// TestRWAAssets_IssuerFlaggedAfterAdmissionRendersWithheld — the
// membership set is cached for ten minutes, so an issuer can acquire a
// scam-class tag while a member row is still in it. The row is then
// served with the same suppression /v1/assets applies: the price and
// market cap are withheld and the status says why. Membership hides
// nothing it admitted; it withholds the number.
//
// The stub directory answers the BATCH lookup used by the row-fill
// overlay with the flag, and the single lookup the membership build
// consumed with a clean entry — reproducing the staleness window
// directly.
func TestRWAAssets_IssuerFlaggedAfterAdmissionRendersWithheld(t *testing.T) {
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
		AssetsReader: &rwaListStub{
			stubAssetsReaderExt: &stubAssetsReaderExt{},
			byIssuer: map[string][]timescale.AssetRow{
				rwaGoodIssuer: {rwaRow("USTRY", rwaGoodIssuer, sptr("1.0412"), 346312)},
			},
			supply: map[string]string{"USTRY-" + rwaGoodIssuer: "12336218000000"},
		},
	})
	v := getRWA(t, srv)
	if len(v.Assets) != 1 {
		t.Fatalf("assets = %v, want the admitted row still served", rwaAssetIDs(v))
	}
	a := v.Assets[0]
	if a.Valuation.Status != "withheld_issuer_flagged" {
		t.Errorf("valuation.status = %q, want withheld_issuer_flagged", a.Valuation.Status)
	}
	if a.Valuation.PriceUSD != nil || a.Valuation.MarketCapUSD != nil {
		t.Error("a flagged issuer published a price or a market cap on the RWA surface")
	}
	if v.Summary.MarketCapUSD != nil {
		t.Errorf("summary.market_cap_usd = %q — a withheld row must not reach the total", *v.Summary.MarketCapUSD)
	}
}

// rwaSkewedDirectory answers the membership build (single lookups are
// unused by it; the batch it makes is the FIRST batch) and the
// per-row overlay (every later batch) from different entries, so a test
// can reproduce a tag landing between the two.
type rwaSkewedDirectory struct {
	membership timescale.DirectoryEntry
	rowFill    timescale.DirectoryEntry
	batches    int
}

func (d *rwaSkewedDirectory) DirectoryEntryByAddress(
	_ context.Context, address string,
) (timescale.DirectoryEntry, bool, error) {
	if address != rwaGoodIssuer {
		return timescale.DirectoryEntry{}, false, nil
	}
	return d.rowFill, true, nil
}

func (d *rwaSkewedDirectory) DirectoryEntriesByAddresses(
	_ context.Context, addresses []string,
) (map[string]timescale.DirectoryEntry, error) {
	d.batches++
	e := d.rowFill
	if d.batches == 1 {
		e = d.membership
	}
	out := map[string]timescale.DirectoryEntry{}
	for _, a := range addresses {
		if a == rwaGoodIssuer {
			out[a] = e
		}
	}
	return out, nil
}

// TestRWAAssets_UnavailableWhenTheDirectoryCannotAnswer — requirement 3
// is what keeps impersonators out, so it fails CLOSED. With no
// directory wired the surface publishes an empty set and says why,
// rather than serving every self-declared candidate.
func TestRWAAssets_UnavailableWhenTheDirectoryCannotAnswer(t *testing.T) {
	srv := v1.New(v1.Options{
		Sep1Cache: &stubSep1BoundReader{bound: []timescale.Sep1BoundCurrency{
			rwaBound("USTRY", rwaScamIssuer, "stellar.us.org", "bond"),
			rwaBound("BENJI", rwaUnknownIssuer, "franklintempleton.reallumens.com", "bond"),
		}},
		AssetsReader: &rwaListStub{
			stubAssetsReaderExt: &stubAssetsReaderExt{},
			byIssuer: map[string][]timescale.AssetRow{
				rwaScamIssuer:    {rwaRow("USTRY", rwaScamIssuer, sptr("1.04"), 1)},
				rwaUnknownIssuer: {rwaRow("BENJI", rwaUnknownIssuer, sptr("1.14"), 1)},
			},
		},
	})
	v := getRWA(t, srv)
	if len(v.Assets) != 0 {
		t.Fatalf("assets = %v, want an empty set when recognition cannot be evaluated", rwaAssetIDs(v))
	}
	if v.Summary.MarketCapUSD != nil {
		t.Error("a total was published for a set that could not be established")
	}
	if v.Summary.Basis == "" {
		t.Error("an empty set was served with no statement of why")
	}
}

// TestRWAAssets_MembershipIsCachedNotRebuiltPerRequest — the scan behind
// membership walks every issuer carrying a SEP-1 payload, so it must
// stay off the request path.
func TestRWAAssets_MembershipIsCachedNotRebuiltPerRequest(t *testing.T) {
	sep1 := &stubSep1BoundReader{bound: []timescale.Sep1BoundCurrency{
		rwaBound("USTRY", rwaGoodIssuer, "etherfuse.com", "bond"),
	}}
	srv := v1.New(v1.Options{
		Sep1Cache: sep1,
		Directory: &stubDirectoryReader{entries: map[string]timescale.DirectoryEntry{
			rwaGoodIssuer: recognisedIssuer(rwaGoodIssuer, "Etherfuse"),
		}},
		AssetsReader: &rwaListStub{
			stubAssetsReaderExt: &stubAssetsReaderExt{},
			byIssuer: map[string][]timescale.AssetRow{
				rwaGoodIssuer: {rwaRow("USTRY", rwaGoodIssuer, sptr("1.0412"), 1)},
			},
		},
	})
	ts := httpTestServer(t, srv)
	for range 3 {
		resp := mustGet(t, ts.URL+"/v1/rwa/assets")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d", resp.StatusCode)
		}
		_ = resp.Body.Close()
	}
	if sep1.calls != 1 {
		t.Errorf("BoundSep1Currencies called %d times; the TTL cache should scan once", sep1.calls)
	}
}
