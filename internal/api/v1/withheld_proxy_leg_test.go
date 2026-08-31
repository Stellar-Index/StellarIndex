package v1_test

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// Regression suite for wave-D MSP-06: a WITHHELD verdict reached on the
// stablecoin-proxy leg was swallowed, so the response said
// errors/price-not-found.
//
// The two answers are not interchangeable. "No price data" tells a
// customer to look nowhere. The withheld problem names /v1/observations,
// /v1/ohlc and /v1/history — where the data IS available — and that
// guidance is the whole reason the distinct problem type exists.
//
// The mechanism: the direct fiat read misses (on-chain trades are quoted
// in issuer stablecoins, not fiat:USD), so the handler enters
// priceFallback; the proxy loop calls LatestPrice(asset, <peg>), which
// returns ErrPriceWithheld; and the loop's bare `continue` — written to
// skip an INACTIVE peg — discarded it along with the miss.
//
// Withheld is now sticky across the loop and does NOT stop it: a later
// peg may still serve, which beats a 404, and the verdict surfaces only
// if none does.

// pegAwarePriceReader returns a different error per pair, which is what
// this scenario needs: not-found on the fiat leg, withheld on the peg.
type pegAwarePriceReader struct {
	errByPair map[string]error
}

func (r *pegAwarePriceReader) LatestPrice(
	_ context.Context, a, q canonical.Asset,
) (v1.PriceSnapshot, []string, bool, error) {
	if err, ok := r.errByPair[a.String()+"/"+q.String()]; ok {
		return v1.PriceSnapshot{}, nil, false, err
	}
	return v1.PriceSnapshot{}, nil, false, v1.ErrPriceNotFound
}

func (r *pegAwarePriceReader) RecentClosedSnapshots(
	_ context.Context, _, _ canonical.Asset, _ int,
) ([]v1.PriceSnapshot, error) {
	return []v1.PriceSnapshot{}, nil
}

const msp06Peg = "USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"

// TestPriceReportsWithheldFromProxyLeg is the headline case.
func TestPriceReportsWithheldFromProxyLeg(t *testing.T) {
	peg, err := canonical.ParseAsset(msp06Peg)
	if err != nil {
		t.Fatal(err)
	}
	asset, err := canonical.ParseAsset("RIO-GA7QYNF7SOWQ3GLR2BGMZEHXAVIRZA4KVWLTJJFC7MGXUA74P7UJVSGZ")
	if err != nil {
		t.Fatal(err)
	}

	reader := &pegAwarePriceReader{errByPair: map[string]error{
		// Direct fiat read misses — the dominant on-chain shape.
		asset.String() + "/fiat:USD": v1.ErrPriceNotFound,
		// The peg leg HAS a price, and policy withholds it.
		asset.String() + "/" + peg.String(): v1.ErrPriceWithheld,
	}}
	srv := v1.New(v1.Options{
		Prices:            reader,
		USDPeggedClassics: []canonical.Asset{peg},
	})
	ts := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, ts.URL+"/v1/price?asset="+asset.String()+"&quote=fiat:USD")
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404. Body: %s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "errors/price-withheld") {
		t.Errorf("body reports the wrong problem type — the proxy leg returned a "+
			"WITHHELD verdict, so the answer is \"we have a price and decline to "+
			"publish it\", not \"we have none\" (MSP-06). Body: %s", body)
	}
	if strings.Contains(string(body), "errors/price-not-found") {
		t.Errorf("withheld reported as not-found — the customer is told to look "+
			"nowhere, when the withheld body would name /v1/observations, "+
			"/v1/ohlc and /v1/history. Body: %s", body)
	}
}

// TestPriceStillNotFoundWhenNothingWithheld is the blast-radius guard:
// an asset with no price anywhere must still get price-not-found. If
// this regressed, every genuine miss would claim to be a withholding
// decision — the opposite dishonesty.
func TestPriceStillNotFoundWhenNothingWithheld(t *testing.T) {
	peg, err := canonical.ParseAsset(msp06Peg)
	if err != nil {
		t.Fatal(err)
	}
	asset, err := canonical.ParseAsset("RIO-GA7QYNF7SOWQ3GLR2BGMZEHXAVIRZA4KVWLTJJFC7MGXUA74P7UJVSGZ")
	if err != nil {
		t.Fatal(err)
	}

	reader := &pegAwarePriceReader{errByPair: map[string]error{
		asset.String() + "/fiat:USD":        v1.ErrPriceNotFound,
		asset.String() + "/" + peg.String(): v1.ErrPriceNotFound,
	}}
	srv := v1.New(v1.Options{
		Prices:            reader,
		USDPeggedClassics: []canonical.Asset{peg},
	})
	ts := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, ts.URL+"/v1/price?asset="+asset.String()+"&quote=fiat:USD")
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404. Body: %s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "errors/price-not-found") {
		t.Errorf("a genuine miss must stay price-not-found — claiming a withholding "+
			"decision we never made is the opposite dishonesty. Body: %s", body)
	}
}

// TestPriceWithheldDoesNotStopThePegLoop — withheld must be STICKY, not
// terminal. A later peg that can serve beats a 404, so the loop keeps
// going and the verdict surfaces only when nothing serves.
func TestPriceWithheldDoesNotStopThePegLoop(t *testing.T) {
	pegA, err := canonical.ParseAsset(msp06Peg)
	if err != nil {
		t.Fatal(err)
	}
	pegB, err := canonical.ParseAsset("USDT-GA2XZLXNLAL26VBCA2OESAIMXTRH5GXKLHYZMDGNCR2SYS5QZWWNBLCK")
	if err != nil {
		t.Fatal(err)
	}
	asset, err := canonical.ParseAsset("RIO-GA7QYNF7SOWQ3GLR2BGMZEHXAVIRZA4KVWLTJJFC7MGXUA74P7UJVSGZ")
	if err != nil {
		t.Fatal(err)
	}

	// pegA withholds; pegB serves. The served price must win.
	reader := &servingPegReader{
		withheldPair: asset.String() + "/" + pegA.String(),
		servingPair:  asset.String() + "/" + pegB.String(),
		price:        "0.4200000000",
	}
	srv := v1.New(v1.Options{
		Prices:            reader,
		USDPeggedClassics: []canonical.Asset{pegA, pegB},
	})
	ts := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, ts.URL+"/v1/price?asset="+asset.String()+"&quote=fiat:USD")
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 — a withheld verdict on ONE peg must not "+
			"abort the loop before a later peg that can serve. Body: %s",
			resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "0.4200000000") {
		t.Errorf("served body missing the second peg's price: %s", body)
	}
}

// servingPegReader withholds one pair and serves another.
type servingPegReader struct {
	withheldPair string
	servingPair  string
	price        string
}

func (r *servingPegReader) LatestPrice(
	_ context.Context, a, q canonical.Asset,
) (v1.PriceSnapshot, []string, bool, error) {
	key := a.String() + "/" + q.String()
	switch key {
	case r.withheldPair:
		return v1.PriceSnapshot{}, nil, false, v1.ErrPriceWithheld
	case r.servingPair:
		return v1.PriceSnapshot{
			AssetID: a.String(), Quote: q.String(),
			Price: r.price, PriceType: "vwap",
		}, []string{"sdex"}, false, nil
	}
	return v1.PriceSnapshot{}, nil, false, v1.ErrPriceNotFound
}

func (r *servingPegReader) RecentClosedSnapshots(
	_ context.Context, _, _ canonical.Asset, _ int,
) ([]v1.PriceSnapshot, error) {
	return []v1.PriceSnapshot{}, nil
}
