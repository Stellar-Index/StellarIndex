package v1_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/pricingguard"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// The batch-vs-single equivalence harness: every id of a fixture universe is priced by /v1/price and by /v1/price/batch on
// one server, and the two answers must agree field for field. Batch read work (set queries, a prefetch memo) must keep it
// green: such work may change when reads happen, never what they return or how they are judged.

const (
	eqIssuer = "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
	eqXLMSAC = "CAS3J7GYLGXMF6TDJBBYYSE3HQ6BBSMLNUQ34T6TZMYMW2EVH34XOWMA"
)

var (
	// Fixed once so two requests (and two servers) read identical bytes.
	eqObservedAt = time.Now().UTC().Add(-2 * time.Minute).Truncate(time.Minute)
	eqBarEnd     = time.Now().UTC().Add(-timescale.FXFixingLag).Truncate(time.Hour)
	eqQuotes     = []string{"fiat:USD", "native", "fiat:EUR", "crypto:BTC"}
)

func eqID(code string) string { return code + "-" + eqIssuer }

// eqCache is the aggregator's VWAP cache keyed "<base>/<quote>" for every window, with fixed stamps.
type eqCache map[string]v1.CachedVWAP

func (c eqCache) LookupTriangulatedVWAP(_ context.Context, base, quote canonical.Asset, _ time.Duration) (v1.CachedVWAP, bool, error) {
	v, ok := c[base.String()+"/"+quote.String()]
	return v, ok, nil
}

// eqMetaCache is eqCache with the router's composite meta keyed like it, so a looker decorator must forward
// [v1.CompositeMetaLooker] for the diverged and rerouted flags to survive.
type eqMetaCache struct {
	eqCache
	meta map[string]string
}

func (c eqMetaCache) LookupCompositeMeta(_ context.Context, base, quote canonical.Asset, _ time.Duration) ([]byte, bool, error) {
	m, ok := c.meta[base.String()+"/"+quote.String()]
	return []byte(m), ok, nil
}

// eqSlowReader holds every read past the request deadline.
type eqSlowReader struct {
	*stubPriceReader
	delay time.Duration
}

func (r eqSlowReader) LatestPrice(ctx context.Context, a, q canonical.Asset) (v1.PriceSnapshot, []string, bool, error) {
	time.Sleep(r.delay)
	return r.stubPriceReader.LatestPrice(ctx, a, q)
}

// eqSourcedReader is a gated chokepointReader whose markets carry their own venue lists.
type eqSourcedReader struct {
	*chokepointReader
	sources map[string][]string
}

func (r eqSourcedReader) LatestPrice(ctx context.Context, a, q canonical.Asset) (v1.PriceSnapshot, []string, bool, error) {
	snap, src, stale, err := r.chokepointReader.LatestPrice(ctx, a, q)
	if s, ok := r.sources[a.String()+"/"+q.String()]; ok && err == nil {
		src = s
	}
	return snap, src, stale, err
}

type eqPair struct {
	base, quote, price string
	sources            []string
	stale, substituted bool
}

// eqUniverseReader is the closed-bucket store of the shared universe. Codes name the path each id exercises:
// DIRC direct in every quote; STAL stale; SUBS a guard-substituted bucket; QXLM priced only against crypto:XLM; FRZQ in
// both XLM spellings with a freeze on native only; FRZH frozen with a held value; FRZN frozen with none; CRSS a USD leg for
// the crypto and fiat crosses; BASI a one-venue EUR book beside a deep USD leg; TOKN priced only against the USDC peg;
// TRIA cache-only (triangulated); MISS nothing; UNRD an unreadable USD market. The XLM market sits under crypto:XLM only.
func eqUniverseReader() *stubPriceReader {
	deep := []string{"binance", "coinbase", "kraken"}
	pairs := []eqPair{
		{base: eqID("DIRC"), quote: "fiat:USD", price: "0.42", sources: []string{"binance", "sdex"}},
		{base: eqID("DIRC"), quote: "native", price: "3.5", sources: []string{"sdex", "soroswap"}},
		{base: eqID("DIRC"), quote: "fiat:EUR", price: "0.39", sources: []string{"binance", "kraken"}},
		{base: eqID("DIRC"), quote: "crypto:BTC", price: "0.0000042", sources: []string{"binance", "kraken"}},
		{base: eqID("STAL"), quote: "fiat:USD", price: "0.05", sources: []string{"sdex"}, stale: true},
		{base: eqID("SUBS"), quote: "fiat:USD", price: "0.07", sources: []string{"sdex", "soroswap"}, substituted: true},
		{base: eqID("QXLM"), quote: "crypto:XLM", price: "2.0", sources: []string{"sdex", "soroswap"}},
		{base: eqID("FRZQ"), quote: "native", price: "1.5", sources: []string{"sdex", "soroswap"}},
		{base: eqID("FRZQ"), quote: "crypto:XLM", price: "1.5", sources: []string{"sdex", "soroswap"}},
		{base: eqID("FRZH"), quote: "fiat:USD", price: "0.9", sources: []string{"binance", "sdex"}},
		{base: eqID("FRZN"), quote: "fiat:USD", price: "0.9", sources: []string{"binance", "sdex"}},
		{base: eqID("CRSS"), quote: "fiat:USD", price: "2.0", sources: deep},
		{base: "crypto:BTC", quote: "fiat:USD", price: "100000", sources: deep},
		{base: eqID("BASI"), quote: "fiat:USD", price: "0.6", sources: deep},
		{base: eqID("BASI"), quote: "fiat:EUR", price: "0.5", sources: []string{"kraken"}},
		{base: eqID("TOKN"), quote: usdcPeg.String(), price: "1.001", sources: []string{"sdex"}},
		{base: "crypto:XLM", quote: "fiat:USD", price: "0.12", sources: []string{"binance", "coinbase"}},
	}
	r := &stubPriceReader{
		snapshots: map[string]v1.PriceSnapshot{},
		sources:   map[string][]string{},
		stale:     map[string]bool{},
		errByPair: map[string]error{eqID("UNRD") + "/fiat:USD": errors.New("canceling statement due to statement timeout")},
	}
	for _, p := range pairs {
		key := p.base + "/" + p.quote
		observed := eqObservedAt
		if p.substituted {
			observed = observed.Add(-10 * time.Minute)
		}
		r.snapshots[key] = v1.PriceSnapshot{
			AssetID: p.base, Quote: p.quote, Price: p.price, PriceType: "vwap",
			ObservedAt: v1.WireTime(observed), Substituted: p.substituted,
		}
		r.sources[key] = p.sources
		r.stale[key] = p.stale
	}
	return r
}

func eqUniverseOptions(*testing.T) v1.Options {
	return v1.Options{
		Prices: eqUniverseReader(),
		Triangulated: eqCache{
			eqID("TRIA") + "/fiat:EUR": {Value: "0.33", Triangulated: true, ObservedAt: eqObservedAt},
			eqID("FRZH") + "/fiat:USD": {Value: "0.85", ObservedAt: eqObservedAt},
			eqID("FRZQ") + "/native":   {Value: "1.4", ObservedAt: eqObservedAt},
		},
		Freeze:            frozenPairs{eqID("FRZH") + "/fiat:USD": true, eqID("FRZN") + "/fiat:USD": true, eqID("FRZQ") + "/native": true},
		USDPeggedClassics: []canonical.Asset{usdcPeg},
		Currencies: &stubCurrenciesReader{snap: &v1.CurrenciesSnapshot{
			Currencies:  []v1.CurrencyEntry{{Ticker: "EUR", Name: "Euro", RateUSD: 0.92, UpdatedAt: eqObservedAt}},
			PublishedAt: eqObservedAt,
		}},
		FXFixings:     fixingsOf(hourlyFixing("EUR", "0.92", eqBarEnd)),
		Change24h:     &stubChange24hReader{prices: map[string]string{eqID("DIRC"): "0.40", "native": "0.11", eqID("STAL"): "0.05"}},
		PegDeclaredAt: eqObservedAt,
	}
}

// eqSpec is one server wiring and the ids, quotes and include_thin settings priced against it.
type eqSpec struct {
	name    string
	ids     []string
	invalid []string // ids that make the whole batch a 400
	quotes  []string
	thin    []bool
	build   func(t *testing.T) v1.Options
}

func eqSpecs() []eqSpec {
	off, both := []bool{false}, []bool{false, true}
	usd := []string{"fiat:USD"}
	from := time.Now().Add(-time.Hour)
	return []eqSpec{{
		name: "universe",
		ids: []string{
			eqID("DIRC"), eqID("STAL"), eqID("SUBS"), eqID("QXLM"), eqID("FRZQ"), eqID("FRZH"), eqID("FRZN"), eqID("CRSS"),
			eqID("BASI"), eqID("TOKN"), eqID("TRIA"), eqID("MISS"), eqID("UNRD"), "native", "crypto:XLM", eqXLMSAC,
			usdcPeg.String(), "fiat:EUR",
		},
		invalid: []string{"@@@", "raw:BTC"},
		quotes:  eqQuotes, thin: off, build: eqUniverseOptions,
	}, {
		// The XLM quote in its other spelling: per-pair markers key on the spelling that answered.
		name: "xlm-quote", ids: []string{eqID("FRZQ"), eqID("QXLM"), eqID("DIRC")}, quotes: []string{"crypto:XLM"}, thin: off,
		build: eqUniverseOptions,
	}, {
		name: "thin", ids: []string{thinAssetID, clearedAssetID}, quotes: usd, thin: both,
		build: func(*testing.T) v1.Options {
			r := newChokepointReader(nil, thinAssetID)
			r.observed = eqObservedAt
			o := v1.Options{Prices: r}
			withChange24h(&o)
			return o
		},
	}, {
		name: "scam", ids: []string{clearedAssetID}, quotes: usd, thin: both,
		build: func(t *testing.T) v1.Options {
			r := newChokepointReader(flaggedScamGate(t))
			r.observed = eqObservedAt
			return v1.Options{Prices: r}
		},
	}, {
		name: "source-excluded", ids: []string{"native"}, quotes: usd, thin: off,
		build: func(*testing.T) v1.Options {
			gate := pricingguard.NewSourceExclusionGate(exclTable{rows: []timescale.SourceExclusion{{ID: 1, Source: "soroswap", From: from}}},
				pricingguard.SourceExclusionGateOptions{})
			return v1.Options{Prices: &stubPriceReader{}, Triangulated: exclCache{value: "0.2468", direct: true}, Exclusions: gate}
		},
	}, {
		name: "deadline", ids: []string{"native"}, quotes: usd, thin: off,
		build: func(*testing.T) v1.Options {
			return v1.Options{Prices: eqSlowReader{&stubPriceReader{err: errors.New("boom")}, 150 * time.Millisecond}, RequestTimeout: 20 * time.Millisecond}
		},
	}, {
		// A thin one-venue EUR book beside a deep USD leg: the basis rule on a thin second pass.
		name: "thin-basis", ids: []string{thinAssetID}, quotes: []string{"fiat:EUR"}, thin: both,
		build: func(*testing.T) v1.Options {
			r := newChokepointReader(nil)
			r.observed = eqObservedAt
			r.prices[thinAssetID+"/fiat:EUR"] = "0.0110"
			r.thinPair(thinAssetID, "fiat:EUR")
			o := v1.Options{
				Prices: eqSourcedReader{r, map[string][]string{
					thinAssetID + "/fiat:EUR": {"kraken"}, thinAssetID + "/fiat:USD": {"binance", "coinbase"},
				}},
				FXFixings: fixingsOf(hourlyFixing("EUR", "0.92", eqBarEnd)),
			}
			withChange24h(&o)
			return o
		},
	}, {
		name: "composite-meta", ids: []string{eqID("DIRC"), eqID("TRIA"), eqID("FRZH")}, quotes: []string{"fiat:USD", "fiat:EUR"},
		thin: off,
		build: func(t *testing.T) v1.Options {
			o := eqUniverseOptions(t)
			o.Triangulated = eqMetaCache{eqCache: o.Triangulated.(eqCache), meta: map[string]string{
				eqID("DIRC") + "/fiat:USD": `{"diverged":true}`,
				eqID("TRIA") + "/fiat:EUR": `{"rerouted":true,"pivot_unverified":true}`,
				eqID("FRZH") + "/fiat:USD": `{"diverged":true,"rerouted":true}`,
			}}
			return o
		},
	}}
}

// eqProjection is what a batch row and a /v1/price data object both carry, copied from the decoded response type.
type eqProjection struct {
	AssetID       string       `json:"asset_id"`
	Quote         string       `json:"quote"`
	Price         string       `json:"price"`
	PriceType     string       `json:"price_type"`
	ObservedAt    v1.WireTime  `json:"observed_at"`
	WindowSeconds int          `json:"window_seconds"`
	Truncated     *bool        `json:"truncated"`
	CoveredFrom   *v1.WireTime `json:"covered_from"`
	FXRate        string       `json:"fx_rate"`
	FXAsOf        *v1.WireTime `json:"fx_as_of"`
	FXSource      string       `json:"fx_source"`
	FXResolution  string       `json:"fx_resolution"`
	USDLeg        *v1.USDLeg   `json:"usd_leg"`
}

func eqProject(s v1.PriceSnapshot) eqProjection {
	return eqProjection{
		AssetID: s.AssetID, Quote: s.Quote, Price: s.Price, PriceType: s.PriceType, ObservedAt: s.ObservedAt,
		WindowSeconds: s.WindowSeconds, Truncated: s.Truncated, CoveredFrom: s.CoveredFrom,
		FXRate: s.FXRate, FXAsOf: s.FXAsOf, FXSource: s.FXSource, FXResolution: s.FXResolution, USDLeg: s.USDLeg,
	}
}

// eqObservation is one surface's answer for one id: a verdict, and for a served price its fields.
type eqObservation struct {
	verdict string // served | withheld | missing | unread | bad_request | status:<n>
	fields  map[string]string
}

func eqServed(t *testing.T, s v1.PriceSnapshot, sources []string, flags map[string]bool) eqObservation {
	t.Helper()
	raw, err := json.Marshal(eqProject(s))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	fields := map[string]string{"sources": strings.Join(sources, ",")}
	for k, v := range m {
		fields[k] = string(v)
	}
	for k, v := range flags {
		fields["flag:"+k] = strconv.FormatBool(v)
	}
	return eqObservation{verdict: "served", fields: fields}
}

func eqRead(t *testing.T, rawURL string) (*http.Response, []byte) {
	t.Helper()
	resp := mustGet(t, rawURL)
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp, body
}

func eqQuery(ids, quote string, thin bool, idParam string) string {
	q := url.Values{idParam: {ids}, "quote": {quote}}
	if thin {
		q.Set("include_thin", "true")
	}
	return q.Encode()
}

// eqSingle maps /v1/price's answer: a 404 price-withheld and a 503 frozen-nothing-held are both "withheld".
func eqSingle(t *testing.T, base, id, quote string, thin bool) eqObservation {
	t.Helper()
	resp, body := eqRead(t, base+"/v1/price?"+eqQuery(id, quote, thin, "asset"))
	var problem struct{ Type, Title string }
	_ = json.Unmarshal(body, &problem)
	switch resp.StatusCode {
	case http.StatusOK:
		var env struct {
			Data    v1.PriceSnapshot `json:"data"`
			Flags   v1.Flags         `json:"flags"`
			Sources []string         `json:"sources"`
		}
		if err := json.Unmarshal(body, &env); err != nil {
			t.Fatalf("decode %s: %v", body, err)
		}
		return eqServed(t, env.Data, env.Sources, map[string]bool{
			"stale": env.Flags.Stale, "triangulated": env.Flags.Triangulated, "thin": env.Flags.ThinMarket,
			"frozen": env.Flags.Frozen, "single_source": env.Flags.SingleSource,
		})
	case http.StatusBadRequest:
		return eqObservation{verdict: "bad_request"}
	case http.StatusNotFound:
		if strings.HasSuffix(problem.Type, "/price-withheld") {
			return eqObservation{verdict: "withheld"}
		}
		return eqObservation{verdict: "missing"}
	case http.StatusServiceUnavailable:
		if strings.HasPrefix(problem.Title, "Price frozen") {
			return eqObservation{verdict: "withheld"}
		}
		return eqObservation{verdict: "unread"}
	case http.StatusInternalServerError:
		return eqObservation{verdict: "unread"}
	}
	return eqObservation{verdict: fmt.Sprintf("status:%d", resp.StatusCode)}
}

type eqEnvelope struct {
	Data         []v1.PriceSnapshot `json:"data"`
	Sources      []string           `json:"sources"`
	Withheld     []string           `json:"withheld"`
	Unread       []string           `json:"unread"`
	Thin         []string           `json:"thin"`
	Stale        []string           `json:"stale"`
	Triangulated []string           `json:"triangulated"`
	Flags        v1.Flags           `json:"flags"`
}

func eqHas(list []string, id string) bool {
	for _, s := range list {
		if s == id {
			return true
		}
	}
	return false
}

// eqBatch maps one id's place in a batch answer; envelope-level flags stand for the row because the batch holds one
// distinct id.
func eqBatch(t *testing.T, base string, ids []string, id, quote string, thin bool) eqObservation {
	t.Helper()
	resp, body := eqRead(t, base+"/v1/price/batch?"+eqQuery(strings.Join(ids, ","), quote, thin, "asset_ids"))
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusBadRequest:
		return eqObservation{verdict: "bad_request"}
	case http.StatusServiceUnavailable:
		return eqObservation{verdict: "unread"}
	default:
		return eqObservation{verdict: fmt.Sprintf("status:%d", resp.StatusCode)}
	}
	var env eqEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	var rows []v1.PriceSnapshot
	for _, r := range env.Data {
		if r.AssetID == id {
			rows = append(rows, r)
		}
	}
	switch {
	case len(rows) > 1:
		t.Errorf("%s: %d rows for one de-duplicated id: %s", id, len(rows), body)
	case len(rows) == 1 && eqHas(env.Thin, id) && rows[0].Change24hPct != nil:
		t.Errorf("%s: thin row carries change_24h_pct (ADR-0018): %s", id, body)
	}
	switch {
	case len(rows) > 0:
		return eqServed(t, rows[0], env.Sources, map[string]bool{
			"stale": eqHas(env.Stale, id), "triangulated": eqHas(env.Triangulated, id), "thin": eqHas(env.Thin, id),
			"frozen": env.Flags.Frozen, "single_source": env.Flags.SingleSource,
		})
	case eqHas(env.Withheld, id):
		return eqObservation{verdict: "withheld"}
	case eqHas(env.Unread, id):
		return eqObservation{verdict: "unread"}
	}
	return eqObservation{verdict: "missing"}
}

// eqDiff names what differs: the verdict alone, or the served fields.
func eqDiff(single, batch eqObservation) []string {
	if single.verdict != batch.verdict {
		return []string{"verdict"}
	}
	var out []string
	for k, v := range single.fields {
		if batch.fields[k] != v {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

func eqKey(spec, id, quote string, thin bool) string {
	k := spec + " " + id + "@" + quote
	if thin {
		k += " include_thin"
	}
	return k
}

// eqKnownDivergence pins a pre-existing batch≠single difference so that any change to it is noticed. It is not an
// endorsement: each entry is a defect on file, and fixing it means deleting the entry.
type eqKnownDivergence struct {
	single, batch string   // the two verdicts
	fields        []string // the differing served fields, when both serve
	why           string
}

func eqKnownDivergences() map[string]eqKnownDivergence {
	// /v1/price walks the quote's spellings (native, then crypto:XLM, then the SAC); the batch reads the literal quote
	// only. Under quote=native every id with no literal X/native market is answered by /v1/price through crypto:XLM
	// (the USD-anchored crypto cross, which a `native` quote never enters), and omitted by the batch as "no data".
	const noQuoteWalk = "batch has no quote-alias walk (pre-existing, pinned so a change is noticed)"
	out := map[string]eqKnownDivergence{}
	for code, single := range map[string]string{
		"STAL": "served", "SUBS": "served", "QXLM": "served", "FRZH": "served", "CRSS": "served", "BASI": "served",
		"TOKN": "served", "FRZN": "withheld", "UNRD": "unread",
	} {
		out[eqKey("universe", eqID(code), "native", false)] = eqKnownDivergence{
			single: single, batch: "missing", fields: []string{"verdict"}, why: noQuoteWalk,
		}
	}
	return out
}

// eqAnchors pin what a fixture is FOR, on the single answer, so the harness cannot pass by comparing two equal misses.
func eqAnchors() map[string]string {
	return map[string]string{
		eqKey("universe", eqID("DIRC"), "fiat:USD", false):     "served",
		eqKey("universe", eqID("STAL"), "fiat:USD", false):     "served flag:stale=true",
		eqKey("universe", eqID("SUBS"), "fiat:USD", false):     "served",
		eqKey("universe", eqID("FRZH"), "fiat:USD", false):     "served flag:frozen=true",
		eqKey("universe", eqID("FRZN"), "fiat:USD", false):     "withheld",
		eqKey("universe", eqID("CRSS"), "crypto:BTC", false):   "served flag:triangulated=true",
		eqKey("universe", eqID("CRSS"), "fiat:EUR", false):     "served flag:triangulated=true",
		eqKey("universe", eqID("BASI"), "fiat:EUR", false):     "served flag:triangulated=true",
		eqKey("universe", eqID("TOKN"), "fiat:USD", false):     "served flag:triangulated=true",
		eqKey("universe", eqID("TRIA"), "fiat:EUR", false):     "served flag:triangulated=true",
		eqKey("universe", eqID("MISS"), "fiat:USD", false):     "missing",
		eqKey("universe", eqID("UNRD"), "fiat:USD", false):     "unread",
		eqKey("universe", "native", "fiat:USD", false):         "served",
		eqKey("universe", "crypto:XLM", "fiat:USD", false):     "served",
		eqKey("universe", "fiat:EUR", "fiat:USD", false):       "served flag:triangulated=true",
		eqKey("universe", usdcPeg.String(), "fiat:USD", false): "served",
		eqKey("universe", "raw:BTC", "fiat:USD", false):        "bad_request",
		eqKey("universe", "@@@", "fiat:USD", false):            "bad_request",
		eqKey("thin", thinAssetID, "fiat:USD", false):          "withheld",
		eqKey("thin", thinAssetID, "fiat:USD", true):           "served flag:thin=true",
		eqKey("scam", clearedAssetID, "fiat:USD", true):        "withheld",
		eqKey("source-excluded", "native", "fiat:USD", false):  "withheld",
		eqKey("deadline", "native", "fiat:USD", false):         "unread",
		eqKey("thin-basis", thinAssetID, "fiat:EUR", true):     "served flag:thin=true flag:triangulated=true",
	}
}

func eqAnchorHolds(o eqObservation, anchor string) bool {
	parts := strings.Fields(anchor)
	if o.verdict != parts[0] {
		return false
	}
	for _, kv := range parts[1:] {
		k, v, _ := strings.Cut(kv, "=")
		if o.fields[k] != v {
			return false
		}
	}
	return true
}

// TestPriceBatchEquivalence_RowEqualsSinglePrice: for every id, quote and include_thin setting, the batch row (or the
// list that names the id) says exactly what /v1/price says for that id alone.
func TestPriceBatchEquivalence_RowEqualsSinglePrice(t *testing.T) {
	eqRowsEqualSingles(t, func(t *testing.T, spec eqSpec) http.Handler { return v1.New(spec.build(t)).Handler() },
		func(ids []string) []string { return ids })
}

// eqRowsEqualSingles checks every fixture's batch row, in a batch of pad(ids), against its single /v1/price answer on
// the server serve builds.
func eqRowsEqualSingles(t *testing.T, serve func(*testing.T, eqSpec) http.Handler, pad func([]string) []string) {
	t.Helper()
	known, anchors := eqKnownDivergences(), eqAnchors()
	seen := map[string]bool{}
	for _, spec := range eqSpecs() {
		for _, quote := range spec.quotes {
			for _, thin := range spec.thin {
				ts := startHTTPTest(t, serve(t, spec))
				for _, id := range append(append([]string{}, spec.ids...), spec.invalid...) {
					key := eqKey(spec.name, id, quote, thin)
					seen[key] = true
					single := eqSingle(t, ts.URL, id, quote, thin)
					batch := eqBatch(t, ts.URL, pad([]string{id, id}), id, quote, thin)
					if a, ok := anchors[key]; ok && !eqAnchorHolds(single, a) {
						t.Errorf("%s: /v1/price = %s %v, want %q: the fixture no longer exercises its path", key, single.verdict, single.fields, a)
					}
					diff := eqDiff(single, batch)
					want, isKnown := known[key]
					switch {
					case !isKnown && len(diff) > 0:
						t.Errorf("%s: batch differs from /v1/price on %v\n single: %s %v\n batch:  %s %v",
							key, diff, single.verdict, single.fields, batch.verdict, batch.fields)
					case isKnown && (single.verdict != want.single || batch.verdict != want.batch ||
						strings.Join(diff, ",") != strings.Join(want.fields, ",")):
						t.Errorf("%s: pinned divergence changed (fixed? then delete the entry)\n want single %s batch %s fields %v\n got  single %s batch %s fields %v",
							key, want.single, want.batch, want.fields, single.verdict, batch.verdict, diff)
					}
				}
			}
		}
	}
	for k := range known {
		if !seen[k] {
			t.Errorf("pinned divergence %q names no fixture", k)
		}
	}
	for k := range anchors {
		if !seen[k] {
			t.Errorf("anchor %q names no fixture", k)
		}
	}
}

// eqBatchRequest is one /v1/price/batch call: GET with asset_ids, or POST with a JSON body.
type eqBatchRequest struct {
	post  bool
	ids   []string
	quote string
	thin  bool
}

// eqVolatile is the response's own clock and request id: the only bytes two correct answers may differ in.
var eqVolatile = regexp.MustCompile(`"(as_of|request_id|instance)":"[^"]*"`)

func eqDoBatch(t *testing.T, h http.Handler, req eqBatchRequest) (status int, retryAfter string, body []byte) {
	t.Helper()
	ts := startHTTPTest(t, h)
	var resp *http.Response
	if req.post {
		payload, err := json.Marshal(map[string]any{"asset_ids": req.ids, "quote": req.quote})
		if err != nil {
			t.Fatal(err)
		}
		u := ts.URL + "/v1/price/batch"
		if req.thin {
			u += "?include_thin=true"
		}
		resp = mustPostJSON(t, u, string(payload))
	} else {
		resp = mustGet(t, ts.URL+"/v1/price/batch?"+eqQuery(strings.Join(req.ids, ","), req.quote, req.thin, "asset_ids"))
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, resp.Header.Get("Retry-After"), eqVolatile.ReplaceAll(raw, []byte(`"$1":""`))
}

// batchEnvelopeMismatch sends req to a and to b and describes the first difference in status, Retry-After or envelope
// bytes (data, sources, withheld, unread, thin, stale, triangulated, flags); "" when the two answers are identical.
func batchEnvelopeMismatch(t *testing.T, a, b http.Handler, req eqBatchRequest) string {
	t.Helper()
	sa, ra, ba := eqDoBatch(t, a, req)
	sb, rb, bb := eqDoBatch(t, b, req)
	switch {
	case sa != sb:
		return fmt.Sprintf("status %d vs %d", sa, sb)
	case ra != rb:
		return fmt.Sprintf("Retry-After %q vs %q", ra, rb)
	case string(ba) != string(bb):
		return fmt.Sprintf("body\n a: %s\n b: %s", ba, bb)
	}
	return ""
}

// eqGoldenRequests is every batch call made over a spec: valid ids with a trailing duplicate, and with the invalid ids,
// by GET and POST, per quote and include_thin setting.
func eqGoldenRequests(spec eqSpec) []eqBatchRequest {
	var out []eqBatchRequest
	sets := [][]string{append(append([]string{}, spec.ids...), spec.ids[0])}
	if len(spec.invalid) > 0 {
		sets = append(sets, append(append([]string{}, spec.ids...), spec.invalid...))
	}
	for _, quote := range spec.quotes {
		for _, thin := range spec.thin {
			for _, ids := range sets {
				for _, post := range []bool{false, true} {
					out = append(out, eqBatchRequest{post: post, ids: ids, quote: quote, thin: thin})
				}
			}
		}
	}
	return out
}

// TestPriceBatchGolden_SameWiringIsByteIdentical runs the envelope comparison over the whole universe with today's
// wiring on both sides; a batch read change swaps one side for the new wiring.
func TestPriceBatchGolden_SameWiringIsByteIdentical(t *testing.T) {
	for _, spec := range eqSpecs() {
		t.Run(spec.name, func(t *testing.T) {
			a, b := v1.New(spec.build(t)).Handler(), v1.New(spec.build(t)).Handler()
			for _, req := range eqGoldenRequests(spec) {
				if m := batchEnvelopeMismatch(t, a, b, req); m != "" {
					t.Errorf("%+v: envelopes differ: %s", req, m)
				}
			}
		})
	}
	t.Run("detects a different server", func(t *testing.T) {
		moved := eqUniverseOptions(t)
		reader := eqUniverseReader()
		snap := reader.snapshots[eqID("DIRC")+"/fiat:USD"]
		snap.Price = "0.43"
		reader.snapshots[eqID("DIRC")+"/fiat:USD"] = snap
		moved.Prices = reader
		a, b := v1.New(eqUniverseOptions(t)).Handler(), v1.New(moved).Handler()
		req := eqBatchRequest{ids: []string{eqID("DIRC"), eqID("MISS")}, quote: "fiat:USD"}
		if m := batchEnvelopeMismatch(t, a, b, req); !strings.Contains(m, `"price":"0.42"`) || !strings.Contains(m, `"price":"0.43"`) {
			t.Errorf("a one-row price change went undetected: %q", m)
		}
	})
}
