package v1_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/currency"
	"github.com/Stellar-Index/StellarIndex/internal/pricingguard"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

const (
	thinAssetID    = "THIN-GA7QYNF7SOWQ3GLR2BGMZEHXAVIRZA4KVWLTJJFC7MGXUA74P7UJVSGZ"
	clearedAssetID = "DEEP-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
	// proxyTokenID has no direct USD market; it prices through a USD peg.
	proxyTokenID = "TOKN-GCQTGZQQ5G4PTM2GL7CDIFKUBIPEC52BROAQIAPW53XBRJVN6ZJVTG6V"
	thinPegID    = "USDA-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
	deepPegID    = "USDB-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
)

var (
	thinMarket  = timescale.MarketSubstance{VolumeUSD: "8.57", Buckets: 1, ValuedBuckets: 1}
	thickMarket = timescale.MarketSubstance{VolumeUSD: "5000000.00", Buckets: 1440, ValuedBuckets: 1440, SpanSeconds: 86000}
)

// thinStore answers every substance read: thin when any leg is in thin
// or the pair is in pairs (either orientation). calls counts the reads.
type thinStore struct {
	thin  map[string]bool
	pairs map[string]bool
	calls *atomic.Int64
}

func (s thinStore) measure(bases, quotes []canonical.Asset) timescale.MarketSubstance {
	if s.calls != nil {
		s.calls.Add(1)
	}
	for _, a := range append(append([]canonical.Asset{}, bases...), quotes...) {
		if s.thin[a.String()] {
			return thinMarket
		}
	}
	for _, b := range bases {
		for _, q := range quotes {
			if s.pairs[b.String()+"/"+q.String()] || s.pairs[q.String()+"/"+b.String()] {
				return thinMarket
			}
		}
	}
	return thickMarket
}

func (s thinStore) PairMarketSubstance(_ context.Context, bases, quotes []canonical.Asset, _ time.Duration) (timescale.MarketSubstance, error) {
	return s.measure(bases, quotes), nil
}

func (s thinStore) PairMarketSubstanceAt(_ context.Context, bases, quotes []canonical.Asset, _ time.Time, _ time.Duration, _ timescale.HistoryGranularity) (timescale.MarketSubstance, error) {
	return s.measure(bases, quotes), nil
}

// chokepointReader is a price store whose gate step mirrors the
// production chokepoint: Judge with the record's release, then Record.
type chokepointReader struct {
	gate     pricingguard.Gate
	store    thinStore
	prices   map[string]string
	observed time.Time

	// hold parks every read made under an opted-in record until closed;
	// entered reports each read's record kind (true: opted in).
	hold    chan struct{}
	entered chan bool
	// secondPassErr answers every read made under an opted-in record.
	secondPassErr error

	mu         sync.Mutex
	calls      int
	optedCalls int
}

func newChokepointReader(scam *pricingguard.ScamGate, thin ...string) *chokepointReader {
	listed := map[string]bool{}
	for _, id := range thin {
		listed[id] = true
	}
	store := thinStore{thin: listed, pairs: map[string]bool{}, calls: new(atomic.Int64)}
	sub := pricingguard.NewSubstanceGate(store, pricingguard.SubstanceGateOptions{})
	return &chokepointReader{
		gate:  pricingguard.Gate{Substance: sub, Scam: scam},
		store: store,
		prices: map[string]string{
			thinAssetID + "/fiat:USD":    "0.01230000000000",
			clearedAssetID + "/fiat:USD": "1.25000000000000",
		},
		observed: time.Now().Add(-30 * time.Second).Truncate(time.Minute).UTC(),
	}
}

// thinPair marks one market thin without thinning either asset's others.
func (r *chokepointReader) thinPair(base, quote string) *chokepointReader {
	r.store.pairs[base+"/"+quote] = true
	return r
}

func (r *chokepointReader) enter(ctx context.Context) error {
	opted := v1.ThinAdmissionFrom(ctx).Requested()
	r.mu.Lock()
	r.calls++
	if opted {
		r.optedCalls++
	}
	r.mu.Unlock()
	if r.entered != nil {
		select {
		case r.entered <- opted:
		default:
		}
	}
	if !opted {
		return nil
	}
	if r.hold != nil {
		<-r.hold
	}
	return r.secondPassErr
}

func (r *chokepointReader) judge(ctx context.Context, a, q canonical.Asset, query pricingguard.Query) pricingguard.Withholding {
	adm := v1.ThinAdmissionFrom(ctx)
	query.AdmitThin = adm.Requested() && adm.Covers(a, q)
	v := r.gate.Judge(ctx, a, q, "test", query)
	adm.Record(a, q, v)
	return v.Withholding
}

func (r *chokepointReader) LatestPrice(ctx context.Context, a, q canonical.Asset) (v1.PriceSnapshot, []string, bool, error) {
	if err := r.enter(ctx); err != nil {
		return v1.PriceSnapshot{}, nil, false, err
	}
	price, ok := r.prices[a.String()+"/"+q.String()]
	if !ok {
		return v1.PriceSnapshot{}, nil, false, v1.ErrPriceNotFound
	}
	if w := r.judge(ctx, a, q, pricingguard.Query{}); w != pricingguard.NotWithheld {
		return v1.PriceSnapshot{}, nil, false, v1.PriceWithheldError(w)
	}
	return v1.PriceSnapshot{
		AssetID: a.String(), Quote: q.String(), Price: price, PriceType: "vwap",
		ObservedAt: v1.WireTime(r.observed),
	}, []string{"sdex"}, false, nil
}

func (r *chokepointReader) RecentClosedSnapshots(context.Context, canonical.Asset, canonical.Asset, int) ([]v1.PriceSnapshot, error) {
	return nil, nil
}

func (r *chokepointReader) PriceAt(ctx context.Context, pair canonical.Pair, ts time.Time, _ time.Duration) (string, time.Time, int, error) {
	if v1.ThinAdmissionFrom(ctx).Requested() && r.secondPassErr != nil {
		if errors.Is(r.secondPassErr, v1.ErrPriceNotFound) {
			return "", time.Time{}, 0, v1.ErrPriceAtUnavailable
		}
		return "", time.Time{}, 0, r.secondPassErr
	}
	price, ok := r.prices[pair.Base.String()+"/"+pair.Quote.String()]
	if !ok {
		return "", time.Time{}, 0, v1.ErrPriceAtUnavailable
	}
	if w := r.judge(ctx, pair.Base, pair.Quote, pricingguard.Query{PointInTime: true, At: ts}); w != pricingguard.NotWithheld {
		return "", time.Time{}, 0, v1.PriceWithheldError(w)
	}
	return price, ts.Truncate(time.Minute), 60, nil
}

func (r *chokepointReader) readCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

func (r *chokepointReader) optedReads() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.optedCalls
}

func flaggedScamGate(t *testing.T) *pricingguard.ScamGate {
	t.Helper()
	return pricingguard.NewScamGate(flaggedDirectory{}, pricingguard.ScamGateOptions{})
}

// flaggedDirectory tags every issuer scam-class.
type flaggedDirectory struct{}

func (flaggedDirectory) DirectoryEntryByAddress(context.Context, string) (timescale.DirectoryEntry, bool, error) {
	return timescale.DirectoryEntry{Tags: []string{"unsafe"}}, true, nil
}

func thinServer(t *testing.T, reader *chokepointReader) *testServerImpl {
	t.Helper()
	return thinServerWith(t, reader, nil)
}

func thinServerWith(t *testing.T, reader *chokepointReader, configure func(*v1.Options)) *testServerImpl {
	t.Helper()
	opts := v1.Options{Prices: reader, PriceAt: reader}
	if configure != nil {
		configure(&opts)
	}
	return startHTTPTest(t, v1.New(opts).Handler())
}

func fetch(t *testing.T, url string) (int, []byte) {
	t.Helper()
	resp := mustGet(t, url)
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, body
}

func postBody(t *testing.T, url, payload string) (int, []byte) {
	t.Helper()
	resp, err := http.Post(url, "application/json", strings.NewReader(payload)) //nolint:noctx // test loopback
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, body
}

func decodeMap(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	return m
}

// assertEvidence checks the evidence block's shape: money as strings (I4)
// and the measurement oriented to the request base.
func assertEvidence(t *testing.T, ev any, base string) {
	t.Helper()
	m, ok := ev.(map[string]any)
	if !ok {
		t.Fatalf("substance = %#v, want an object", ev)
	}
	if m["base"] != base {
		t.Errorf("substance.base = %v, want the request base %s", m["base"], base)
	}
	if v, ok := m["volume_usd"].(string); !ok || v != "8.57" {
		t.Errorf("substance.volume_usd = %#v, want the string \"8.57\"", m["volume_usd"])
	}
	floor, _ := m["floor"].(map[string]any)
	if _, ok := floor["min_volume_usd"].(string); !ok {
		t.Errorf("substance.floor.min_volume_usd = %#v, want a decimal string", floor["min_volume_usd"])
	}
	if m["failed"] == "" || m["failed"] == nil {
		t.Errorf("substance.failed missing in %v", m)
	}
	if s, _ := m["measured_at"].(string); !strings.HasSuffix(s, "Z") {
		t.Errorf("substance.measured_at = %q, want RFC 3339 UTC", s)
	}
}

func TestHandlePriceIncludeThin(t *testing.T) {
	reader := newChokepointReader(nil, thinAssetID)
	ts := thinServer(t, reader)
	url := ts.URL + "/v1/price?asset=" + thinAssetID + "&quote=fiat:USD"

	status, body := fetch(t, url)
	if status != http.StatusNotFound {
		t.Fatalf("default: status %d, want 404: %s", status, body)
	}
	p := decodeMap(t, body)
	if !strings.HasSuffix(p["type"].(string), "/price-withheld") {
		t.Errorf("default: type = %v, want price-withheld", p["type"])
	}
	assertEvidence(t, p["substance"], thinAssetID)

	if status, other := fetch(t, url+"&include_thin=yes"); status != http.StatusNotFound || !bytes.Equal(stripRequestID(other), stripRequestID(body)) {
		t.Errorf("include_thin=yes: status %d, want the default 404 body; got %s", status, other)
	}

	status, body = fetch(t, url+"&include_thin=true")
	if status != http.StatusOK {
		t.Fatalf("opted in: status %d, want 200: %s", status, body)
	}
	env := decodeMap(t, body)
	data := env["data"].(map[string]any)
	if data["price"] != "0.01230000000000" {
		t.Errorf("price = %v, want the reader's string", data["price"])
	}
	if flags, _ := env["flags"].(map[string]any); flags["thin_market"] != true {
		t.Errorf("flags.thin_market = %v, want true", flags["thin_market"])
	}
	if data["confidence"] != v1.ThinMarketConfidenceCeiling {
		t.Errorf("confidence = %v, want the declared ceiling %v", data["confidence"], v1.ThinMarketConfidenceCeiling)
	}
	if _, ok := data["confidence_factors"]; ok {
		t.Errorf("confidence_factors present with no cached score")
	}
	assertEvidence(t, data["substance"], thinAssetID)
}

func TestHandlePriceIncludeThinClearedIsByteIdentical(t *testing.T) {
	reader := newChokepointReader(nil, thinAssetID)
	ts := thinServer(t, reader)
	url := ts.URL + "/v1/price?asset=" + clearedAssetID + "&quote=fiat:USD"
	_, def := fetch(t, url)
	before := reader.readCount()
	_, opted := fetch(t, url+"&include_thin=true")
	if !bytes.Equal(stripRequestID(def), stripRequestID(opted)) {
		t.Errorf("cleared pair: opted-in body differs from default\ndefault: %s\nopted:   %s", def, opted)
	}
	if strings.Contains(string(opted), "thin_market") || strings.Contains(string(opted), "substance") {
		t.Errorf("cleared pair carries a thin marker: %s", opted)
	}
	if n := reader.readCount() - before; n != 1 {
		t.Errorf("cleared opted-in read cost %d upstream reads, want 1 (no second pass)", n)
	}
}

func TestIncludeThinDoesNotReleaseFlaggedIssuer(t *testing.T) {
	ts := thinServer(t, newChokepointReader(flaggedScamGate(t), thinAssetID))
	status, body := fetch(t, ts.URL+"/v1/price?asset="+thinAssetID+"&quote=fiat:USD&include_thin=true")
	if status != http.StatusNotFound {
		t.Fatalf("status %d, want 404: %s", status, body)
	}
	p := decodeMap(t, body)
	if p["title"] != "Price withheld — issuer flagged" {
		t.Errorf("title = %v, want the flagged-issuer withholding", p["title"])
	}
	if _, ok := p["substance"]; ok {
		t.Errorf("a flagged issuer's 404 carries substance evidence: %s", body)
	}
}

func TestOracleIgnoresIncludeThin(t *testing.T) {
	ts := thinServer(t, newChokepointReader(nil, thinAssetID))
	url := ts.URL + "/v1/oracle/lastprice?asset=" + thinAssetID
	defStatus, def := fetch(t, url)
	optStatus, opted := fetch(t, url+"&include_thin=true")
	if defStatus == http.StatusOK || optStatus != defStatus || !bytes.Equal(stripRequestID(def), stripRequestID(opted)) {
		t.Errorf("SEP-40 answered the opt-in differently: default %d %s, opted %d %s", defStatus, def, optStatus, opted)
	}
}

func TestPriceBatchIncludeThin(t *testing.T) {
	ts := thinServerWith(t, newChokepointReader(nil, thinAssetID), withChange24h)
	ids := thinAssetID + "," + clearedAssetID
	check := func(t *testing.T, body []byte, opted bool) {
		t.Helper()
		env := decodeMap(t, body)
		rows, _ := env["data"].([]any)
		served := map[string]map[string]any{}
		for _, r := range rows {
			row := r.(map[string]any)
			served[row["asset_id"].(string)] = row
		}
		thin, _ := env["thin"].([]any)
		withheld, _ := env["withheld"].([]any)
		flags, _ := env["flags"].(map[string]any)
		if served[clearedAssetID] == nil {
			t.Fatalf("cleared row missing: %s", body)
		}
		if _, ok := served[clearedAssetID]["change_24h_pct"]; !ok {
			t.Errorf("cleared row lost its change_24h_pct: %s", body)
		}
		if !opted {
			if served[thinAssetID] != nil || len(withheld) != 1 || withheld[0] != thinAssetID || thin != nil || flags["thin_market"] != nil {
				t.Errorf("default: want the thin id on withheld only: %s", body)
			}
			return
		}
		row := served[thinAssetID]
		if row == nil || len(thin) != 1 || thin[0] != thinAssetID || withheld != nil || flags["thin_market"] != true {
			t.Fatalf("opted in: want the thin id in data and thin, flagged: %s", body)
		}
		for _, k := range []string{"change_24h_pct", "confidence", "substance"} {
			if _, ok := row[k]; ok {
				t.Errorf("thin batch row carries %s", k)
			}
		}
	}
	_, def := fetch(t, ts.URL+"/v1/price/batch?asset_ids="+ids)
	check(t, def, false)
	_, opted := fetch(t, ts.URL+"/v1/price/batch?asset_ids="+ids+"&include_thin=true")
	check(t, opted, true)
	payload := `{"asset_ids":["` + thinAssetID + `","` + clearedAssetID + `"]}`
	_, postDef := postBody(t, ts.URL+"/v1/price/batch", payload)
	check(t, postDef, false)
	_, postOpted := postBody(t, ts.URL+"/v1/price/batch?include_thin=true", payload)
	check(t, postOpted, true)
}

// withChange24h wires a 24h-ago anchor, so a row's change_24h_pct is
// computed unless the row is refused one.
func withChange24h(o *v1.Options) {
	then := "1.00000000000000"
	o.Change24h = &stubChange24hReader{prices: map[string]string{
		thinAssetID: then, clearedAssetID: then, proxyTokenID: then,
	}}
}

// batchServed indexes a batch envelope's data rows by asset id.
func batchServed(t *testing.T, body []byte) (map[string]map[string]any, map[string]any) {
	t.Helper()
	env := decodeMap(t, body)
	served := map[string]map[string]any{}
	rows, _ := env["data"].([]any)
	for _, r := range rows {
		row := r.(map[string]any)
		served[row["asset_id"].(string)] = row
	}
	return served, env
}

// TestPriceBatchThinRowHasNoChange24h: a thin row served from the
// fallback chain or a freeze hold derives no change either.
func TestPriceBatchThinRowHasNoChange24h(t *testing.T) {
	cases := []struct {
		name      string
		id        string
		price     string
		configure func(*chokepointReader, *v1.Options)
	}{
		{"fallback", proxyTokenID, "0.98000000000000", func(r *chokepointReader, o *v1.Options) {
			r.prices[proxyTokenID+"/"+thinPegID] = "0.98000000000000"
			r.thinPair(proxyTokenID, thinPegID)
			o.USDPeggedClassics = []canonical.Asset{mustParseAsset(t, thinPegID)}
		}},
		{"frozen hold", thinAssetID, "0.0100", func(r *chokepointReader, o *v1.Options) {
			r.store.thin[thinAssetID] = true
			o.Freeze = frozenPairs{thinAssetID + "/fiat:USD": true}
			o.Triangulated = lkgPairs{thinAssetID + "/fiat:USD/300": "0.0100"}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reader := newChokepointReader(nil)
			ts := thinServerWith(t, reader, func(o *v1.Options) {
				withChange24h(o)
				tc.configure(reader, o)
			})
			_, body := fetch(t, ts.URL+"/v1/price/batch?asset_ids="+tc.id+","+clearedAssetID+"&include_thin=true")
			served, env := batchServed(t, body)
			row := served[tc.id]
			if thin, _ := env["thin"].([]any); row == nil || len(thin) != 1 || thin[0] != tc.id {
				t.Fatalf("want %s served thin: %s", tc.id, body)
			}
			if row["price"] != tc.price {
				t.Fatalf("price = %v, want %s: the row did not take the %s path", row["price"], tc.price, tc.name)
			}
			if _, ok := row["change_24h_pct"]; ok {
				t.Errorf("thin %s row carries change_24h_pct: %s", tc.name, body)
			}
			if _, ok := served[clearedAssetID]["change_24h_pct"]; !ok {
				t.Errorf("cleared row lost its change_24h_pct: %s", body)
			}
		})
	}
}

func TestPriceAtIncludeThin(t *testing.T) {
	ts := thinServer(t, newChokepointReader(nil, thinAssetID))
	at := time.Now().Add(-48 * time.Hour).UTC().Truncate(time.Hour)
	url := ts.URL + "/v1/price/at?asset=" + thinAssetID + "&ts=" + at.Format(time.RFC3339)

	status, body := fetch(t, url)
	if status != http.StatusNotFound {
		t.Fatalf("default: status %d, want 404: %s", status, body)
	}
	p := decodeMap(t, body)
	assertEvidence(t, p["substance"], thinAssetID)
	if p["substance"].(map[string]any)["window_end"] == nil {
		t.Errorf("point-in-time evidence has no window_end: %s", body)
	}

	status, body = fetch(t, url+"&include_thin=true")
	if status != http.StatusOK {
		t.Fatalf("opted in: status %d, want 200: %s", status, body)
	}
	env := decodeMap(t, body)
	if flags, _ := env["flags"].(map[string]any); flags["thin_market"] != true {
		t.Errorf("flags.thin_market = %v, want true", flags["thin_market"])
	}
	data := env["data"].(map[string]any)
	assertEvidence(t, data["substance"], thinAssetID)
	end, _ := time.Parse(time.RFC3339, data["substance"].(map[string]any)["window_end"].(string))
	if end.After(at) || at.Sub(end) > time.Hour {
		t.Errorf("window_end = %v, want the requested instant %v (grain-truncated)", end, at)
	}
	if _, ok := data["confidence"]; ok {
		t.Errorf("/v1/price/at invented a confidence")
	}
}

func TestPriceChangesIncludeThin(t *testing.T) {
	ts := thinServer(t, newChokepointReader(nil, thinAssetID))
	url := ts.URL + "/v1/price/changes?asset=" + thinAssetID + "&quote=fiat:USD"

	status, body := fetch(t, url)
	if status != http.StatusNotFound {
		t.Fatalf("default: status %d, want 404: %s", status, body)
	}
	assertEvidence(t, decodeMap(t, body)["substance"], thinAssetID)

	status, body = fetch(t, url+"&include_thin=true")
	if status != http.StatusOK {
		t.Fatalf("opted in: status %d, want 200: %s", status, body)
	}
	env := decodeMap(t, body)
	if flags, _ := env["flags"].(map[string]any); flags["thin_market"] != true {
		t.Errorf("flags.thin_market = %v, want true", flags["thin_market"])
	}
	data := env["data"].(map[string]any)
	assertEvidence(t, data["substance"], thinAssetID)
	for _, h := range []string{"1h", "24h", "7d", "30d"} {
		hz := data[h].(map[string]any)
		if hz["thin_market"] != true || hz["withheld"] != false {
			t.Errorf("horizon %s = %v, want a thin-served horizon", h, hz)
		}
	}

	_, cleared := fetch(t, ts.URL+"/v1/price/changes?asset="+clearedAssetID+"&quote=fiat:USD&include_thin=true")
	if strings.Contains(string(cleared), "thin_market") || strings.Contains(string(cleared), "substance") {
		t.Errorf("cleared pair carries a thin marker: %s", cleared)
	}
}

func TestAssetDetailIncludeThin(t *testing.T) {
	reader := newChokepointReader(nil, thinAssetID)
	ts := thinServer(t, reader)
	url := ts.URL + "/v1/assets/" + thinAssetID

	status, body := fetch(t, url)
	if status != http.StatusOK {
		t.Fatalf("default: status %d: %s", status, body)
	}
	def := decodeMap(t, body)["data"].(map[string]any)
	if def["price_usd"] != nil || def["price_withheld_reason"] != string(v1.PriceWithheldSubstance) {
		t.Errorf("default detail: price_usd=%v reason=%v, want withheld for substance", def["price_usd"], def["price_withheld_reason"])
	}
	for _, k := range []string{"thin_market", "substance"} {
		if _, ok := def[k]; ok {
			t.Errorf("default detail carries %s", k)
		}
	}

	before := reader.readCount()
	status, body = fetch(t, url+"?include_thin=true")
	if status != http.StatusOK {
		t.Fatalf("opted in: status %d: %s", status, body)
	}
	if reader.readCount() == before {
		t.Fatalf("opted-in detail was served from the default cache entry")
	}
	d := decodeMap(t, body)["data"].(map[string]any)
	if d["price_usd"] != "0.01230000000000" || d["thin_market"] != true {
		t.Errorf("opted-in detail: price_usd=%v thin_market=%v, want the thin price flagged", d["price_usd"], d["thin_market"])
	}
	if _, ok := d["price_withheld_reason"]; ok {
		t.Errorf("a served price still carries price_withheld_reason")
	}
	assertEvidence(t, d["substance"], thinAssetID)
	for _, k := range []string{"market_cap_usd", "fdv_usd", "change_24h_pct", "price_history_24h", "price_history_7d", "ath"} {
		if d[k] != nil {
			t.Errorf("thin detail derives %s = %v", k, d[k])
		}
	}

	_, cleared := fetch(t, ts.URL+"/v1/assets/"+clearedAssetID+"?include_thin=true")
	if strings.Contains(string(cleared), "thin_market") || strings.Contains(string(cleared), `"substance"`) {
		t.Errorf("cleared asset carries a thin marker: %s", cleared)
	}
}

func withPegs(t *testing.T, ids ...string) func(*v1.Options) {
	t.Helper()
	pegs := make([]canonical.Asset, 0, len(ids))
	for _, id := range ids {
		pegs = append(pegs, mustParseAsset(t, id))
	}
	return func(o *v1.Options) { o.USDPeggedClassics = pegs }
}

// proxyReader prices proxyTokenID only through USD pegs; the thin peg's
// market fails the floor.
func proxyReader(scam *pricingguard.ScamGate) *chokepointReader {
	r := newChokepointReader(scam).thinPair(proxyTokenID, thinPegID)
	r.prices[proxyTokenID+"/"+thinPegID] = "0.98000000000000"
	r.prices[proxyTokenID+"/"+deepPegID] = "0.99000000000000"
	return r
}

func problemOf(t *testing.T, status int, body []byte) map[string]any {
	t.Helper()
	if status != http.StatusNotFound {
		t.Fatalf("status %d, want 404: %s", status, body)
	}
	return decodeMap(t, body)
}

// TestIncludeThinReleasesOwnThinProxyLeg: a token priced only through a
// thin USD peg is withheld by default and served flagged when opted in.
func TestIncludeThinReleasesOwnThinProxyLeg(t *testing.T) {
	ts := thinServerWith(t, proxyReader(nil), withPegs(t, thinPegID))
	url := ts.URL + "/v1/price?asset=" + proxyTokenID + "&quote=fiat:USD"

	status, body := fetch(t, url)
	p := problemOf(t, status, body)
	if p["title"] != "Price withheld" || !strings.HasSuffix(p["type"].(string), "/price-withheld") {
		t.Errorf("default: %v / %v, want the unattributed withholding", p["title"], p["type"])
	}
	assertEvidence(t, p["substance"], proxyTokenID)

	status, body = fetch(t, url+"&include_thin=true")
	if status != http.StatusOK {
		t.Fatalf("opted in: status %d, want 200: %s", status, body)
	}
	env := decodeMap(t, body)
	data := env["data"].(map[string]any)
	if data["price"] != "0.98000000000000" {
		t.Errorf("price = %v, want the peg leg's", data["price"])
	}
	if flags, _ := env["flags"].(map[string]any); flags["thin_market"] != true {
		t.Errorf("flags.thin_market = %v, want true", flags["thin_market"])
	}
	assertEvidence(t, data["substance"], proxyTokenID)
	if q := data["substance"].(map[string]any)["quote"]; q != thinPegID {
		t.Errorf("substance.quote = %v, want the thin peg %s", q, thinPegID)
	}

	batch := ts.URL + "/v1/price/batch?quote=fiat:USD&asset_ids=" + proxyTokenID
	_, def := fetch(t, batch)
	served, env := batchServed(t, def)
	if withheld, _ := env["withheld"].([]any); served[proxyTokenID] != nil || len(withheld) != 1 || withheld[0] != proxyTokenID {
		t.Errorf("batch default: want the token on withheld only: %s", def)
	}
	_, opted := fetch(t, batch+"&include_thin=true")
	served, env = batchServed(t, opted)
	if thin, _ := env["thin"].([]any); served[proxyTokenID] == nil || len(thin) != 1 || thin[0] != proxyTokenID {
		t.Errorf("batch opted in: want the token in data and thin: %s", opted)
	}
}

// TestIncludeThinClearedPegWins: a cleared route beats a thin one, so
// the opt-in changes nothing and runs no second pass.
func TestIncludeThinClearedPegWins(t *testing.T) {
	reader := proxyReader(nil)
	ts := thinServerWith(t, reader, withPegs(t, thinPegID, deepPegID))
	for _, path := range []string{
		"/v1/price?asset=" + proxyTokenID + "&quote=fiat:USD",
		"/v1/price/changes?asset=" + proxyTokenID + "&quote=fiat:USD",
	} {
		defStatus, def := fetch(t, ts.URL+path)
		optStatus, opted := fetch(t, ts.URL+path+"&include_thin=true")
		if optStatus != defStatus || !bytes.Equal(stripRequestID(def), stripRequestID(opted)) {
			t.Errorf("%s: opted-in %d %s, want the default %d %s", path, optStatus, opted, defStatus, def)
		}
		if strings.Contains(string(opted), "thin_market\":true") {
			t.Errorf("%s: a cleared route is flagged thin: %s", path, opted)
		}
	}
	_, body := fetch(t, ts.URL+"/v1/price?asset="+proxyTokenID+"&quote=fiat:USD")
	if data, _ := decodeMap(t, body)["data"].(map[string]any); data["price"] != "0.99000000000000" {
		t.Fatalf("control: want the cleared peg's price: %s", body)
	}
	if n := reader.optedReads(); n != 0 {
		t.Errorf("%d opted-in reads: a priced first pass must not retry", n)
	}
}

// TestIncludeThinXLMCrossStaysGated: the opt-in covers the request
// base's own markets only, never a shared XLM/quote cross leg.
func TestIncludeThinXLMCrossStaysGated(t *testing.T) {
	reader := newChokepointReader(nil).thinPair("native", "fiat:USD")
	reader.prices[thinPegID+"/native"] = "6.25000000000000"
	reader.prices["native/fiat:USD"] = "0.16000000000000"
	ts := thinServerWith(t, reader, withPegs(t, thinPegID))
	url := ts.URL + "/v1/price?asset=" + thinPegID + "&quote=fiat:USD"

	defStatus, def := fetch(t, url)
	p := problemOf(t, defStatus, def)
	if p["title"] != "Price withheld" {
		t.Fatalf("control: default = %v, want withheld on the XLM leg: %s", p["title"], def)
	}
	optStatus, opted := fetch(t, url+"&include_thin=true")
	if optStatus != defStatus || !bytes.Equal(stripRequestID(def), stripRequestID(opted)) {
		t.Errorf("opted in: %d %s, want the default %d %s", optStatus, opted, defStatus, def)
	}
	for _, body := range [][]byte{def, opted} {
		if strings.Contains(string(body), "substance") || strings.Contains(string(body), "thin_market") {
			t.Errorf("the XLM leg's evidence reached the peg's response: %s", body)
		}
	}
}

// TestIncludeThinRetriesOnlyWithheld: the second pass runs only for a
// substance-shaped withholding that recorded evidence.
func TestIncludeThinRetriesOnlyWithheld(t *testing.T) {
	cases := []struct {
		name      string
		reader    func() *chokepointReader
		configure func(*v1.Options)
		path      string
		retry     bool
		title     string
	}{
		{
			name: "substance", reader: func() *chokepointReader { return newChokepointReader(nil, thinAssetID) },
			path: "/v1/price?asset=" + thinAssetID + "&quote=fiat:USD", retry: true,
		},
		{
			name: "unattributed thin peg", reader: func() *chokepointReader { return proxyReader(nil) },
			configure: withPegs(t, thinPegID), path: "/v1/price?asset=" + proxyTokenID + "&quote=fiat:USD", retry: true,
		},
		{
			name: "scam entry", reader: func() *chokepointReader { return proxyReader(nil) },
			configure: func(o *v1.Options) { withPegs(t, thinPegID)(o); o.Scam = flaggedScamGate(t) },
			path:      "/v1/price?asset=" + proxyTokenID + "&quote=fiat:USD", title: "Price withheld",
		},
		{
			name: "fx leg", reader: func() *chokepointReader { return proxyReader(nil) },
			configure: func(o *v1.Options) { withPegs(t, thinPegID, deepPegID)(o); o.FXFixings = &stubFXFixings{} },
			path:      "/v1/price?asset=" + proxyTokenID + "&quote=fiat:EUR", title: "Price withheld — FX leg unavailable",
		},
		{
			name: "flagged peg leg", reader: func() *chokepointReader { return proxyReader(flaggedScamGate(t)) },
			configure: withPegs(t, thinPegID), path: "/v1/price?asset=" + proxyTokenID + "&quote=fiat:USD", title: "Price withheld",
		},
		{
			name: "not found", reader: func() *chokepointReader { return newChokepointReader(nil, thinAssetID) },
			path: "/v1/price?asset=" + proxyTokenID + "&quote=fiat:USD", title: "No price data for pair",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reader := tc.reader()
			ts := thinServerWith(t, reader, tc.configure)
			status, body := fetch(t, ts.URL+tc.path+"&include_thin=true")
			if tc.retry {
				if status != http.StatusOK || reader.optedReads() == 0 {
					t.Errorf("status %d after %d opted-in reads, want the second pass to serve: %s", status, reader.optedReads(), body)
				}
				return
			}
			if n := reader.optedReads(); n != 0 {
				t.Errorf("%d opted-in reads: this withholding must not retry", n)
			}
			p := problemOf(t, status, body)
			if p["title"] != tc.title {
				t.Errorf("title = %v, want %q", p["title"], tc.title)
			}
			if _, ok := p["substance"]; ok {
				t.Errorf("a non-substance outcome carries substance evidence: %s", body)
			}
		})
	}

	t.Run("detail upstream leg", func(t *testing.T) {
		for _, flagged := range []bool{false, true} {
			var scam *pricingguard.ScamGate
			if flagged {
				scam = flaggedScamGate(t)
			}
			reader := proxyReader(scam)
			ts := thinServerWith(t, reader, withPegs(t, thinPegID))
			_, body := fetch(t, ts.URL+"/v1/assets/"+proxyTokenID+"?include_thin=true")
			d := decodeMap(t, body)["data"].(map[string]any)
			if flagged {
				if reader.optedReads() != 0 || d["price_usd"] != nil || d["price_withheld_reason"] != string(v1.PriceWithheldUpstreamLeg) || d["thin_market"] != nil {
					t.Errorf("flagged peg leg: %d opted-in reads, detail %v; want upstream_leg withheld, no second pass", reader.optedReads(), d)
				}
				continue
			}
			if reader.optedReads() == 0 || d["price_usd"] != "0.98000000000000" || d["thin_market"] != true {
				t.Errorf("thin peg leg: %d opted-in reads, detail %v; want the second pass to serve it thin", reader.optedReads(), d)
			}
		}
	})
}

// TestIncludeThinPass2MissServesPass1: a second pass that also misses
// serves the first pass's withholding unchanged.
func TestIncludeThinPass2MissServesPass1(t *testing.T) {
	for _, miss := range []error{v1.PriceWithheldError(pricingguard.WithheldThinMarket), v1.ErrPriceNotFound} {
		t.Run(miss.Error(), func(t *testing.T) {
			reader := newChokepointReader(nil, thinAssetID)
			reader.secondPassErr = miss
			ts := thinServer(t, reader)
			for _, path := range []string{
				"/v1/price?asset=" + thinAssetID + "&quote=fiat:USD",
				"/v1/price/changes?asset=" + thinAssetID + "&quote=fiat:USD",
				"/v1/price/batch?asset_ids=" + thinAssetID + "," + clearedAssetID,
			} {
				defStatus, def := fetch(t, ts.URL+path)
				optStatus, opted := fetch(t, ts.URL+path+"&include_thin=true")
				if optStatus != defStatus || !bytes.Equal(stripRequestID(def), stripRequestID(opted)) {
					t.Errorf("%s: opted-in %d %s, want the default %d %s", path, optStatus, opted, defStatus, def)
				}
			}
			if reader.optedReads() == 0 {
				t.Fatal("the second pass never ran; the case is vacuous")
			}
		})
	}
}

// TestHandlePriceNeverNullPrice: an admitted second pass with no price
// serves the first pass's withholding, never an empty 200.
func TestHandlePriceNeverNullPrice(t *testing.T) {
	reader := newChokepointReader(nil, thinAssetID)
	reader.prices[thinAssetID+"/fiat:USD"] = ""
	ts := thinServer(t, reader)
	for _, path := range []string{
		"/v1/price?asset=" + thinAssetID + "&quote=fiat:USD",
		"/v1/price/batch?asset_ids=" + thinAssetID + "," + clearedAssetID,
	} {
		defStatus, def := fetch(t, ts.URL+path)
		optStatus, opted := fetch(t, ts.URL+path+"&include_thin=true")
		if optStatus != defStatus || !bytes.Equal(stripRequestID(def), stripRequestID(opted)) {
			t.Errorf("%s: opted-in %d %s, want the default %d %s", path, optStatus, opted, defStatus, def)
		}
	}
	if reader.optedReads() == 0 {
		t.Fatal("the second pass never ran; the case is vacuous")
	}
}

func TestHandlePriceThinConfidenceIsCapped(t *testing.T) {
	for _, tc := range []struct{ cached, want float64 }{{0.5, v1.ThinMarketConfidenceCeiling}, {0.05, 0.05}} {
		ts := thinServerWith(t, newChokepointReader(nil, thinAssetID), func(o *v1.Options) {
			o.Confidence = &stubConfidenceLooker{score: v1.PriceSnapshotConfidence{Confidence: tc.cached}, found: true}
		})
		_, body := fetch(t, ts.URL+"/v1/price?asset="+thinAssetID+"&quote=fiat:USD&include_thin=true")
		data := decodeMap(t, body)["data"].(map[string]any)
		if data["confidence"] != tc.want {
			t.Errorf("cached %v: confidence = %v, want min(cached, %v) = %v", tc.cached, data["confidence"], v1.ThinMarketConfidenceCeiling, tc.want)
		}
		if _, ok := data["confidence_factors"]; !ok {
			t.Errorf("cached %v: confidence_factors dropped from a cached score", tc.cached)
		}
	}
}

// TestSubstanceEvidenceOrientedToRequest: a verdict cached by the
// reverse pair still names the request base as the evidence's base.
func TestSubstanceEvidenceOrientedToRequest(t *testing.T) {
	reader := newChokepointReader(nil).thinPair(proxyTokenID, thinPegID)
	reader.prices[thinPegID+"/"+proxyTokenID] = "1.02000000000000"
	reader.prices[proxyTokenID+"/"+thinPegID] = "0.98000000000000"
	ts := thinServer(t, reader)

	pStatus, pBody := fetch(t, ts.URL+"/v1/price?asset="+thinPegID+"&quote="+proxyTokenID)
	p := problemOf(t, pStatus, pBody)
	assertEvidence(t, p["substance"], thinPegID)
	measured := reader.store.calls.Load()

	status, body := fetch(t, ts.URL+"/v1/price?asset="+proxyTokenID+"&quote="+thinPegID+"&include_thin=true")
	if status != http.StatusOK {
		t.Fatalf("opted in: status %d, want 200: %s", status, body)
	}
	ev := decodeMap(t, body)["data"].(map[string]any)["substance"]
	assertEvidence(t, ev, proxyTokenID)
	if q := ev.(map[string]any)["quote"]; q != thinPegID {
		t.Errorf("substance.quote = %v, want %s", q, thinPegID)
	}
	if n := reader.store.calls.Load(); n != measured {
		t.Errorf("%d new substance reads: the reverse pair's cached verdict was not reused", n-measured)
	}
}

const (
	hopAID = "HOPA-GCQTGZQQ5G4PTM2GL7CDIFKUBIPEC52BROAQIAPW53XBRJVN6ZJVTG6V"
	hopBID = "HOPB-GCQTGZQQ5G4PTM2GL7CDIFKUBIPEC52BROAQIAPW53XBRJVN6ZJVTG6V"
)

type transitiveCandidates []timescale.TransitivePrice

func (c transitiveCandidates) TransitiveUSDPriceCandidates(context.Context, string) ([]timescale.TransitivePrice, error) {
	return c, nil
}

// detailServer serves the asset detail with the reader's own substance
// gate on the catalogue and transitive producers.
func detailServer(t *testing.T, reader *chokepointReader, configure func(*v1.Options)) *testServerImpl {
	t.Helper()
	return thinServerWith(t, reader, func(o *v1.Options) {
		o.Substance = reader.gate.Substance
		o.AssetsReader = &stubAssetsReaderExt{rowErr: sql.ErrNoRows}
		if configure != nil {
			configure(o)
		}
	})
}

func detailData(t *testing.T, ts *testServerImpl, id, query string) (map[string]any, []byte) {
	t.Helper()
	status, body := fetch(t, ts.URL+"/v1/assets/"+id+query)
	if status != http.StatusOK {
		t.Fatalf("GET detail %s%s: status %d: %s", id, query, status, body)
	}
	return decodeMap(t, body)["data"].(map[string]any), body
}

func TestIncludeThinAssetDetailTransitiveThinIsFlagged(t *testing.T) {
	reader := newChokepointReader(nil).thinPair(proxyTokenID, hopAID)
	ts := detailServer(t, reader, func(o *v1.Options) {
		o.TransitivePricer = transitiveCandidates{{PriceUSD: "0.42", Hop: hopAID, HopVolume24hUSD: "9.00"}}
	})
	def, _ := detailData(t, ts, proxyTokenID, "")
	if def["price_usd"] != nil || def["thin_market"] != nil {
		t.Fatalf("default served the thin near leg: %v", def)
	}
	d, _ := detailData(t, ts, proxyTokenID, "?include_thin=true")
	if d["price_usd"] != "0.42" || d["price_basis"] != "transitive" || d["thin_market"] != true {
		t.Errorf("opted in: price %v basis %v thin %v, want 0.42 transitive flagged thin", d["price_usd"], d["price_basis"], d["thin_market"])
	}
	assertEvidence(t, d["substance"], proxyTokenID)
}

func TestIncludeThinClearedTransitiveCandidateWins(t *testing.T) {
	reader := newChokepointReader(nil).thinPair(proxyTokenID, hopAID)
	ts := detailServer(t, reader, func(o *v1.Options) {
		o.TransitivePricer = transitiveCandidates{
			{PriceUSD: "0.42", Hop: hopAID, HopVolume24hUSD: "9.00"},
			{PriceUSD: "0.43", Hop: hopBID, HopVolume24hUSD: "8.00"},
		}
	})
	def, defBody := detailData(t, ts, proxyTokenID, "")
	if def["price_usd"] != "0.43" {
		t.Fatalf("control: default price %v, want the cleared candidate's 0.43", def["price_usd"])
	}
	_, opted := detailData(t, ts, proxyTokenID, "?include_thin=true")
	if !bytes.Equal(stripRequestID(defBody), stripRequestID(opted)) {
		t.Errorf("opted in %s, want the default %s", opted, defBody)
	}
	if n := reader.optedReads(); n != 0 {
		t.Errorf("%d opted-in reads: a priced detail must not retry", n)
	}
}

func TestIncludeThinCatalogueBeatsThinF2(t *testing.T) {
	reader := newChokepointReader(nil).thinPair(proxyTokenID, "fiat:USD")
	reader.prices[proxyTokenID+"/fiat:USD"] = "0.80000000000000"
	asset := mustParseAsset(t, proxyTokenID)
	catalogue := "0.77"
	ts := detailServer(t, reader, func(o *v1.Options) {
		o.AssetsReader = &stubAssetsReaderExt{row: timescale.AssetRow{
			AssetID: proxyTokenID, Code: asset.Code, IssuerGStrkey: asset.Issuer, PriceUSD: &catalogue,
		}}
	})
	def, defBody := detailData(t, ts, proxyTokenID, "")
	if def["price_usd"] != catalogue {
		t.Fatalf("control: default price %v, want the catalogue's %s", def["price_usd"], catalogue)
	}
	_, opted := detailData(t, ts, proxyTokenID, "?include_thin=true")
	if !bytes.Equal(stripRequestID(defBody), stripRequestID(opted)) {
		t.Errorf("opted in %s, want the default %s", opted, defBody)
	}
}

// TestIncludeThinDeclaredPegWinsOverThinPrice: an operator-declared peg
// replaces a thin market price on the detail; without a fresh FX rate
// the thin price stands, flagged.
func TestIncludeThinDeclaredPegWinsOverThinPrice(t *testing.T) {
	aud, err := canonical.NewFiatAsset("AUD")
	if err != nil {
		t.Fatal(err)
	}
	fx := &stubFXHistoryReader{points: []v1.FXQuotePoint{
		{Bucket: time.Now().UTC().Add(-24 * time.Hour), RateUSDText: "1.5267", InverseUSDText: "0.655"},
	}}
	serve := func(withFX bool) *testServerImpl {
		return detailServer(t, newChokepointReader(nil, thinAssetID), func(o *v1.Options) {
			o.FiatPeggedClassics = map[string]canonical.Asset{thinAssetID: aud}
			if withFX {
				o.FXHistory = fx
			}
		})
	}

	ts := serve(true)
	def, _ := detailData(t, ts, thinAssetID, "")
	d, _ := detailData(t, ts, thinAssetID, "?include_thin=true")
	if d["price_usd"] != "0.655" || d["price_basis"] != "declared_peg" || d["thin_market"] != nil {
		t.Errorf("opted in: price %v basis %v thin %v, want the unflagged declared peg", d["price_usd"], d["price_basis"], d["thin_market"])
	}
	if d["substance"] == nil {
		t.Error("opted in: the measured thin market's evidence was dropped")
	}
	if def["price_usd"] != d["price_usd"] || def["price_basis"] != d["price_basis"] {
		t.Errorf("default %v/%v, want the same declared peg", def["price_usd"], def["price_basis"])
	}

	d, _ = detailData(t, serve(false), thinAssetID, "?include_thin=true")
	if d["price_usd"] != "0.01230000000000" || d["thin_market"] != true {
		t.Errorf("no FX rate: price %v thin %v, want the thin market price flagged", d["price_usd"], d["thin_market"])
	}
}

// TestIncludeThinCatalogueSurfacesIgnoreIt: the global slug and the
// catalogue listing rows never take the opt-in.
func TestIncludeThinCatalogueSurfacesIgnoreIt(t *testing.T) {
	cat, err := currency.LoadEmbedded()
	if err != nil {
		t.Fatal(err)
	}
	store := thinStore{thin: map[string]bool{gateAQUAAssetID: true}, pairs: map[string]bool{}, calls: new(atomic.Int64)}
	ts := httpTestServer(t, v1.New(v1.Options{
		AssetsReader:       &gatedCatalogueAssets{},
		Substance:          pricingguard.NewSubstanceGate(store, pricingguard.SubstanceGateOptions{}),
		VerifiedCurrencies: cat,
	}))
	for _, path := range []string{
		"/v1/assets/aqua",
		"/v1/assets?asset_class=crypto&limit=100",
		"/v1/assets?asset_class=all&limit=100",
	} {
		sep := "?"
		if strings.Contains(path, "?") {
			sep = "&"
		}
		for _, p := range []string{path, path + sep + "include_thin=true"} {
			if got := gatedCatalogueRow(t, ts, p).published(); len(got) > 0 {
				t.Errorf("GET %s published a thin catalogue valuation: %v", p, got)
			}
		}
	}
	_, def := fetch(t, ts.URL+"/v1/assets/aqua")
	_, opted := fetch(t, ts.URL+"/v1/assets/aqua?include_thin=true")
	if !bytes.Equal(stripRequestID(def), stripRequestID(opted)) {
		t.Errorf("global slug: opted in %s, want the default %s", opted, def)
	}
}

type reply struct {
	status int
	body   []byte
}

func getAsync(url string) <-chan reply {
	ch := make(chan reply, 1)
	go func() {
		resp, err := http.Get(url) //nolint:noctx // test loopback
		if err != nil {
			ch <- reply{}
			return
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		ch <- reply{resp.StatusCode, body}
	}()
	return ch
}

func await(t *testing.T, name string, ch <-chan reply) reply {
	t.Helper()
	select {
	case r := <-ch:
		return r
	case <-time.After(5 * time.Second):
		t.Fatalf("%s: no reply in 5s; it joined a flight it does not share a verdict with", name)
		return reply{}
	}
}

func awaitRead(t *testing.T, reader *chokepointReader, opted bool) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case got := <-reader.entered:
			if got == opted {
				return
			}
		case <-deadline:
			t.Fatalf("no upstream read under an opted-in=%v record", opted)
		}
	}
}

func drain(ch chan bool) {
	for {
		select {
		case <-ch:
		default:
			return
		}
	}
}

// TestPriceReadFlightKeysOnAdmission: the opt-in bit is part of the
// coalescing key. A default or SEP-40 read never waits on, or is served
// from, an opted-in flight; a second opted-in read shares it.
func TestPriceReadFlightKeysOnAdmission(t *testing.T) {
	reader := newChokepointReader(nil, thinAssetID)
	reader.hold = make(chan struct{})
	reader.entered = make(chan bool, 64)
	var once sync.Once
	release := func() { once.Do(func() { close(reader.hold) }) }
	ts := thinServer(t, reader)
	t.Cleanup(release) // before the server's Close, which waits on held handlers
	url := ts.URL + "/v1/price?asset=" + thinAssetID + "&quote=fiat:USD"
	oracle := ts.URL + "/v1/oracle/lastprice?asset=" + thinAssetID
	sep40Status, sep40 := fetch(t, oracle)

	x := getAsync(url + "&include_thin=true")
	awaitRead(t, reader, true)

	d := await(t, "default read", getAsync(url))
	if d.status != http.StatusNotFound {
		t.Fatalf("default read beside an opted-in flight: status %d, want 404: %s", d.status, d.body)
	}
	assertEvidence(t, decodeMap(t, d.body)["substance"], thinAssetID)
	s := await(t, "SEP-40 read", getAsync(oracle))
	if s.status != sep40Status || !bytes.Equal(stripRequestID(s.body), stripRequestID(sep40)) {
		t.Errorf("SEP-40 beside an opted-in flight: %d %s, want its unchanged %d %s", s.status, s.body, sep40Status, sep40)
	}

	drain(reader.entered)
	y := getAsync(url + "&include_thin=true")
	awaitRead(t, reader, false) // y's first pass; its second joins x's flight
	time.Sleep(300 * time.Millisecond)
	release()
	for name, ch := range map[string]<-chan reply{"first opted-in": x, "coalesced opted-in": y} {
		r := await(t, name, ch)
		if r.status != http.StatusOK {
			t.Fatalf("%s: status %d, want 200: %s", name, r.status, r.body)
		}
		env := decodeMap(t, r.body)
		if flags, _ := env["flags"].(map[string]any); flags["thin_market"] != true {
			t.Errorf("%s: flags.thin_market = %v, want true", name, flags["thin_market"])
		}
		assertEvidence(t, env["data"].(map[string]any)["substance"], thinAssetID)
	}
	if n := reader.optedReads(); n != 1 {
		t.Errorf("opted-in upstream reads = %d, want 1: the second opted-in read must share the first's flight", n)
	}
}

// stripRequestID drops per-request members so two bodies compare on content.
func stripRequestID(body []byte) []byte {
	var m map[string]any
	if json.Unmarshal(body, &m) != nil {
		return body
	}
	delete(m, "request_id")
	delete(m, "instance")
	delete(m, "as_of")
	out, _ := json.Marshal(m)
	return out
}
