package v1

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// TestWithProxyDeviation_UncheckedPegIsOnTheWire: Degraded is header-only, so a failed peg read must reach the JSON body,
// or an SDK caller reads the absent proxy_deviation as "peg holds".
func TestWithProxyDeviation_UncheckedPegIsOnTheWire(t *testing.T) {
	peg := mustAsset(t, withheldTestPegIssuer)
	for _, c := range []struct {
		name         string
		reader       depegProbeReader
		want         bool
		wantDegraded bool
	}{
		{"read failed", depegProbeReader{err: errors.New("pool timeout")}, true, true},
		// A peg that cannot be observed was not checked: absent proxy_deviation would read as "peg holds".
		{"no observation", depegProbeReader{err: ErrPriceAtUnavailable}, true, false},
		{"guarded", depegProbeReader{err: ErrPriceAtGuarded}, true, false},
		{"in band", depegProbeReader{value: "1.0001"}, false, false},
		{"in band, closed within grace", depegProbeReader{value: "1.0001", age: 10 * time.Minute}, false, false},
		// A stalled peg feed's last in-band bucket is not evidence the peg holds now.
		{"in band but stale", depegProbeReader{value: "1.0001", age: 23 * time.Hour}, true, false},
		{"in band, closed 17m before at", depegProbeReader{value: "1.0001", age: 17 * time.Minute}, true, false},
	} {
		s := &Server{Options: Options{PriceAt: c.reader, USDPeggedClassics: []canonical.Asset{peg}, VerifiedCurrencies: mustEmbeddedCatalogue(t)}}
		f := s.withProxyDeviation(context.Background(), Flags{Triangulated: true}, usdQuote(t), time.Now())
		if f.Degraded != c.wantDegraded {
			t.Errorf("%s: Degraded = %v, want %v", c.name, f.Degraded, c.wantDegraded)
		}
		body, err := json.Marshal(f)
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.Contains(string(body), `"proxy_deviation_unchecked":true`); got != c.want {
			t.Errorf("%s: proxy_deviation_unchecked on the wire = %v, want %v (%s)", c.name, got, c.want, body)
		}
	}
}

// TestPegObservationMarkets_CoverFiatCombineBooks: every non-literal quote the fiat combine may read must have its peg
// checked (or be a declared peg counted as unbound), so a new backer cannot feed an answer whose depeg check skips it.
func TestPegObservationMarkets_CoverFiatCombineBooks(t *testing.T) {
	s := &Server{Options: Options{USDPeggedClassics: []canonical.Asset{mustAsset(t, withheldTestPegIssuer)}, VerifiedCurrencies: mustEmbeddedCatalogue(t)}}
	for _, code := range []string{"USD", "EUR"} {
		quote, err := canonical.NewFiatAsset(code)
		if err != nil {
			t.Fatal(err)
		}
		pair, err := canonical.NewPair(mustAsset(t, "native"), quote)
		if err != nil {
			t.Fatal(err)
		}
		established, heldBack := s.usdPeggedConstituentSets(pair)
		for _, book := range append(established, heldBack...) {
			q := book.Quote
			if q.Equal(quote) {
				continue
			}
			markets, _ := s.pegObservationMarkets(quote, []canonical.Pair{book})
			checked := map[string]bool{}
			for _, forms := range markets {
				for _, p := range forms {
					checked[p.Base.String()] = true
				}
			}
			if ticker, ok := s.verifiedPegTicker(q); ok {
				q = ticker
			} else if q.Type == canonical.AssetClassic {
				continue // a declared peg with no verified ticker is counted unbound, so the answer reads unchecked
			}
			if !checked[q.String()] {
				t.Errorf("%s: combine reads %s/%s but the depeg check never observes %s", code, book.Base, book.Quote, q)
			}
		}
	}
	// Plain triangulation (no combine books) never reads X/crypto:USDT, so it does not answer for USDT's peg.
	plain, _ := s.pegObservationMarkets(mustAsset(t, "fiat:USD"), nil)
	for _, forms := range plain {
		if forms[0].Base.String() == "crypto:USDT" {
			t.Errorf("depeg check without combine books observes crypto:USDT")
		}
	}
}

// TestWithProxyDeviation_StaleReadingOnlyAddsDoubt: a feed that halts mid-depeg must keep the warning; staleness adds
// proxy_deviation_unchecked and never clears proxy_deviation.
func TestWithProxyDeviation_StaleReadingOnlyAddsDoubt(t *testing.T) {
	peg := mustAsset(t, withheldTestPegIssuer)
	for _, c := range []struct {
		name                     string
		reader                   depegProbeReader
		wantDeviated, wantUnchkd bool
	}{
		{"out of band, fresh", depegProbeReader{value: "0.88"}, true, false},
		{"out of band but stale", depegProbeReader{value: "0.88", age: 23 * time.Hour}, true, true},
		{"in band but stale", depegProbeReader{value: "1.0001", age: 23 * time.Hour}, false, true},
		// The price-at ladder serves a feed dead for a day from its 1d rung; a bucket that opened ~47h ago is no all-clear.
		{"in band, 1d bucket closed 23h ago", depegProbeReader{value: "1.0001", age: 23 * time.Hour, width: 86400}, false, true},
		{"in band, 1d bucket closing now", depegProbeReader{value: "1.0001", width: 86400}, false, true},
		{"in band, 15m bucket closed 1m ago", depegProbeReader{value: "1.0001", age: time.Minute, width: 900}, false, false},
		{"in band, 15m bucket closed 3m ago", depegProbeReader{value: "1.0001", age: 3 * time.Minute, width: 900}, false, true},
	} {
		s := &Server{Options: Options{PriceAt: c.reader, USDPeggedClassics: []canonical.Asset{peg}, VerifiedCurrencies: mustEmbeddedCatalogue(t)}}
		f := s.withProxyDeviation(context.Background(), Flags{Triangulated: true}, usdQuote(t), time.Now())
		if f.ProxyDeviation != c.wantDeviated || f.ProxyDeviationUnchecked != c.wantUnchkd {
			t.Errorf("%s: proxy_deviation=%v unchecked=%v, want %v/%v", c.name, f.ProxyDeviation, f.ProxyDeviationUnchecked, c.wantDeviated, c.wantUnchkd)
		}
	}
}
