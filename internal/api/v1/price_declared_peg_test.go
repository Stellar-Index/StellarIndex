package v1_test

import (
	"context"
	"net/http"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// usdcClassicID is Circle's mainnet USDC — the ONE peg the shipped
// operator configuration declares
// (configs/ansible/roles/archival-node/templates/stellarindex.toml.j2,
// [trades].usd_pegged_classic_assets), and the asset the live defect was
// found on. Every test in this file runs in that single-peg shape: with
// one declared peg there is no sibling peg to walk, so the only market a
// depeg can print on is the peg's own XLM book.
const usdcClassicID = "USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"

// declaredPegAdoptedAt is a fixed adoption stamp the tests pin the wire
// value to, well in the past so it can never be confused with a request
// clock.
var declaredPegAdoptedAt = time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)

// recordingPriceReader is a stubPriceReader that records every pair
// LatestPrice was asked for, so a test can prove which markets a route
// consulted — and which it never touched.
type recordingPriceReader struct {
	stubPriceReader
	mu    sync.Mutex
	calls []string
}

func (r *recordingPriceReader) LatestPrice(ctx context.Context, a, q canonical.Asset) (v1.PriceSnapshot, []string, bool, error) {
	r.mu.Lock()
	r.calls = append(r.calls, a.String()+"/"+q.String())
	r.mu.Unlock()
	return r.stubPriceReader.LatestPrice(ctx, a, q)
}

func (r *recordingPriceReader) pairsAsked() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := append([]string(nil), r.calls...)
	sort.Strings(out)
	return out
}

type pegEnvelope struct {
	Data  v1.PriceSnapshot `json:"data"`
	AsOf  time.Time        `json:"as_of"`
	Flags v1.Flags         `json:"flags"`
}

func getPegEnvelope(t *testing.T, url string) pegEnvelope {
	t.Helper()
	resp := mustGet(t, url)
	if resp.StatusCode != http.StatusOK {
		body, _ := readAll(resp)
		t.Fatalf("status = %d, want 200. Body: %s", resp.StatusCode, body)
	}
	var env pegEnvelope
	mustDecode(t, resp, &env)
	return env
}

// TestPrice_DeclaredPegServesTheObservedXLMCross pins the rule the
// stablecoin-proxy peg arm broke: the operator's 1:1 declaration is a
// CONSTANT, and a constant must never pre-empt an observation.
//
// The peg arm used to answer the moment it recognised the requested
// asset as a declared peg, so /v1/price?asset=USDC-GA5Z…&quote=fiat:USD
// published 1.000000000000 while the market was somewhere else — the
// live shape (2026-09-03) was /v1/price serving the flat peg in the same
// minute /v1/assets served 1.0008594347 for the same asset. Under a real
// depeg the surface would have gone on publishing $1 rather than the
// break, which is the one moment the number matters.
//
// Here USDC has no fiat:USD bucket anywhere (nothing on chain quotes in
// fiat), but its XLM book on SDEX has repriced: 9.5 XLM per USDC while
// XLM's own CEX dollar market prints 0.10 — a cross of 0.95. The route
// must find it and serve it as an observation — price_type, observed_at
// and window_seconds intact — with the two legs' venues both credited.
func TestPrice_DeclaredPegServesTheObservedXLMCross(t *testing.T) {
	usdc, err := canonical.ParseAsset(usdcClassicID)
	if err != nil {
		t.Fatalf("parse USDC: %v", err)
	}
	pegLegAt := time.Unix(1745000000, 0).UTC()
	pivotAt := pegLegAt.Add(-3 * time.Minute) // the staler leg bounds freshness
	reader := &stubPriceReader{
		snapshots: map[string]v1.PriceSnapshot{
			// The peg's own book, quoted in `native` — where SDEX prints it.
			// Both legs are closed 1-minute buckets, the shape
			// VWAP1mToSnapshot hands the reader's callers.
			usdcClassicID + "/native": {
				AssetID: usdcClassicID, Quote: "native",
				Price: "9.5", PriceType: "vwap", ObservedAt: pegLegAt, WindowSeconds: 60,
			},
			// XLM's dollar market, stored under the CEX spelling — reached
			// through the alias loop, one form away from `native`.
			"crypto:XLM/fiat:USD": {
				AssetID: "crypto:XLM", Quote: "fiat:USD",
				Price: "0.10", PriceType: "vwap", ObservedAt: pivotAt, WindowSeconds: 60,
			},
		},
		sources: map[string][]string{
			usdcClassicID + "/native": {"sdex"},
			"crypto:XLM/fiat:USD":     {"coinbase", "bitstamp"},
		},
	}
	srv := v1.New(v1.Options{
		Prices:            reader,
		USDPeggedClassics: []canonical.Asset{usdc},
		PegDeclaredAt:     declaredPegAdoptedAt,
	})
	ts := startHTTPTest(t, srv.Handler())

	env := getPegEnvelope(t, ts.URL+"/v1/price?asset="+usdcClassicID+"&quote=fiat:USD")
	if env.Data.Price != "0.9500000000" {
		t.Errorf("price = %q, want 0.9500000000 — the observed XLM cross, not the declared peg", env.Data.Price)
	}
	if env.Data.PriceType != "vwap" {
		t.Errorf("price_type = %q, want vwap (an observation, not a declaration)", env.Data.PriceType)
	}
	if env.Data.WindowSeconds != 60 {
		t.Errorf("window_seconds = %d, want 60 — a served vwap names its window; only last_trade omits it", env.Data.WindowSeconds)
	}
	if !env.Flags.Triangulated {
		t.Errorf("flags.triangulated = false, want true — the value is composed through XLM")
	}
	if !env.Data.ObservedAt.Equal(pivotAt) {
		t.Errorf("observed_at = %s, want the OLDER leg's %s — a derived price is only as fresh as its staler input",
			env.Data.ObservedAt, pivotAt)
	}
	if env.Data.AssetID != usdcClassicID || env.Data.Quote != "fiat:USD" {
		t.Errorf("echo = %s/%s, want the requested %s/fiat:USD", env.Data.AssetID, env.Data.Quote, usdcClassicID)
	}
	// sources ride the envelope, not the snapshot; window_seconds is
	// omitempty, so its presence is checked on the wire, not the struct.
	resp := mustGet(t, ts.URL+"/v1/price?asset="+usdcClassicID+"&quote=fiat:USD")
	body, _ := readAll(resp)
	if !strings.Contains(body, `"sources":["bitstamp","coinbase","sdex"]`) {
		t.Errorf("sources must credit both legs' venues, sorted: %s", body)
	}
	if !strings.Contains(body, `"window_seconds":60`) {
		t.Errorf("window_seconds must be on the wire for a vwap: %s", body)
	}
}

// TestPrice_DeclaredPegWithNoObservationServesTheDeclaration pins the
// fallback that remains when NO market prices the peg at all: the flat $1
// still serves (F-1232) rather than a 404, it is labelled a declaration,
// and it carries the declaration's adoption stamp — not the clock.
func TestPrice_DeclaredPegWithNoObservationServesTheDeclaration(t *testing.T) {
	usdc, err := canonical.ParseAsset(usdcClassicID)
	if err != nil {
		t.Fatalf("parse USDC: %v", err)
	}
	reader := &stubPriceReader{err: v1.ErrPriceNotFound}
	srv := v1.New(v1.Options{
		Prices:            reader,
		USDPeggedClassics: []canonical.Asset{usdc},
		PegDeclaredAt:     declaredPegAdoptedAt,
	})
	ts := startHTTPTest(t, srv.Handler())

	env := getPegEnvelope(t, ts.URL+"/v1/price?asset="+usdcClassicID+"&quote=fiat:USD")
	if env.Data.Price != "1.000000000000" {
		t.Errorf("price = %q, want 1.000000000000 — the declaration is the last resort, not a 404", env.Data.Price)
	}
	if env.Data.PriceType != "peg" {
		t.Errorf("price_type = %q, want peg", env.Data.PriceType)
	}
	if !env.Data.ObservedAt.Equal(declaredPegAdoptedAt) {
		t.Errorf("observed_at = %s, want the declaration stamp %s — a constant is not re-observed per request",
			env.Data.ObservedAt, declaredPegAdoptedAt)
	}
	if !env.Flags.Triangulated {
		t.Errorf("flags.triangulated = false, want true — the wire shape F-1232 fixed")
	}
}

// TestPrice_DeclaredPegDirectUSDObservationWins pins the order: a
// directly observed fiat:USD market for the peg is served as-is, and the
// XLM cross is never consulted for it — no XLM-quoted read of the peg,
// no read of XLM's dollar market. The cross is a fallback for a missing
// market, never a competitor to a present one.
func TestPrice_DeclaredPegDirectUSDObservationWins(t *testing.T) {
	usdc, err := canonical.ParseAsset(usdcClassicID)
	if err != nil {
		t.Fatalf("parse USDC: %v", err)
	}
	directAt := time.Unix(1745000000, 0).UTC()
	reader := &recordingPriceReader{stubPriceReader: stubPriceReader{
		snapshots: map[string]v1.PriceSnapshot{
			usdcClassicID + "/fiat:USD": {
				AssetID: usdcClassicID, Quote: "fiat:USD",
				Price: "0.9700", PriceType: "vwap", ObservedAt: directAt,
			},
			// Both cross legs are present and would compose to 0.95 —
			// they must not be read.
			usdcClassicID + "/native": {
				AssetID: usdcClassicID, Quote: "native",
				Price: "9.5", PriceType: "vwap", ObservedAt: directAt,
			},
			"crypto:XLM/fiat:USD": {
				AssetID: "crypto:XLM", Quote: "fiat:USD",
				Price: "0.10", PriceType: "vwap", ObservedAt: directAt,
			},
		},
		sources: map[string][]string{
			usdcClassicID + "/fiat:USD": {"kraken"},
		},
	}}
	srv := v1.New(v1.Options{
		Prices:            reader,
		USDPeggedClassics: []canonical.Asset{usdc},
		PegDeclaredAt:     declaredPegAdoptedAt,
	})
	ts := startHTTPTest(t, srv.Handler())

	env := getPegEnvelope(t, ts.URL+"/v1/price?asset="+usdcClassicID+"&quote=fiat:USD")
	if env.Data.Price != "0.9700" {
		t.Errorf("price = %q, want the direct 0.9700", env.Data.Price)
	}
	if env.Data.PriceType != "vwap" || !env.Data.ObservedAt.Equal(directAt) {
		t.Errorf("snapshot = %s@%s, want the direct observation vwap@%s", env.Data.PriceType, env.Data.ObservedAt, directAt)
	}
	if env.Flags.Triangulated {
		t.Errorf("flags.triangulated = true — a directly observed market is not composed")
	}
	for _, pair := range reader.pairsAsked() {
		if !strings.HasSuffix(pair, "/fiat:USD") || !strings.HasPrefix(pair, usdcClassicID+"/") {
			t.Errorf("reader asked for %s — the cross must not run once the direct market answered", pair)
		}
	}
}

// TestPrice_DeclaredPegXLMLegPrefersAFreshForm pins which of XLM's
// forms prices the peg leg when more than one holds a row: a fresh
// answer under any form beats a stale one under an earlier form, and a
// stale answer still serves when it is the only one — the preference
// readPriceWithAliases applies on the direct read, mirrored on the leg.
//
// `native` is the first form in alias order. Taking the first form that
// answers would serve a three-day-old `native`-quoted book at 9.5 (a
// cross of 0.95, stamped three days ago) while a fresh SAC-quoted book
// prints 10.0 (a cross of 1.00, stamped minutes ago). The wire carries
// flags.stale=true on every fallback answer regardless
// (TestPrice_FallbackChainSetsStaleFlag), so the leg chosen shows in
// the price and in observed_at, not in the flag.
func TestPrice_DeclaredPegXLMLegPrefersAFreshForm(t *testing.T) {
	usdc, err := canonical.ParseAsset(usdcClassicID)
	if err != nil {
		t.Fatalf("parse USDC: %v", err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	staleAt := now.Add(-72 * time.Hour)
	freshAt := now.Add(-2 * time.Minute)
	pivotAt := now.Add(-1 * time.Minute)
	staleNativeLeg := usdcClassicID + "/native"
	freshSACLeg := usdcClassicID + "/" + canonical.XLMSacContractID
	legs := func(withFresh bool) *stubPriceReader {
		reader := &stubPriceReader{
			snapshots: map[string]v1.PriceSnapshot{
				staleNativeLeg: {
					AssetID: usdcClassicID, Quote: "native",
					Price: "9.5", PriceType: "vwap", ObservedAt: staleAt, WindowSeconds: 60,
				},
				"crypto:XLM/fiat:USD": {
					AssetID: "crypto:XLM", Quote: "fiat:USD",
					Price: "0.10", PriceType: "vwap", ObservedAt: pivotAt, WindowSeconds: 60,
				},
			},
			stale: map[string]bool{
				staleNativeLeg:        true,
				"crypto:XLM/fiat:USD": false,
			},
		}
		if withFresh {
			reader.snapshots[freshSACLeg] = v1.PriceSnapshot{
				AssetID: usdcClassicID, Quote: canonical.XLMSacContractID,
				Price: "10.0", PriceType: "vwap", ObservedAt: freshAt, WindowSeconds: 60,
			}
			reader.stale[freshSACLeg] = false
		}
		return reader
	}

	t.Run("a fresh SAC-quoted book beats the stale native-quoted one", func(t *testing.T) {
		srv := v1.New(v1.Options{
			Prices:            legs(true),
			USDPeggedClassics: []canonical.Asset{usdc},
			PegDeclaredAt:     declaredPegAdoptedAt,
		})
		ts := startHTTPTest(t, srv.Handler())
		env := getPegEnvelope(t, ts.URL+"/v1/price?asset="+usdcClassicID+"&quote=fiat:USD")
		if env.Data.Price != "1.0000000000" {
			t.Errorf("price = %q, want 1.0000000000 — the fresh SAC-quoted book, not the stale native one", env.Data.Price)
		}
		// The older of the two legs is the fresh peg leg, two minutes
		// ago — not the three-day-old book the earlier form holds.
		if !env.Data.ObservedAt.Equal(freshAt) {
			t.Errorf("observed_at = %s, want the fresh leg's %s", env.Data.ObservedAt, freshAt)
		}
		if env.Data.ObservedAt.Before(now.Add(-time.Hour)) {
			t.Errorf("observed_at = %s is not recent — the stale leg was served", env.Data.ObservedAt)
		}
	})

	t.Run("a stale book still serves when it is the only one", func(t *testing.T) {
		srv := v1.New(v1.Options{
			Prices:            legs(false),
			USDPeggedClassics: []canonical.Asset{usdc},
			PegDeclaredAt:     declaredPegAdoptedAt,
		})
		ts := startHTTPTest(t, srv.Handler())
		env := getPegEnvelope(t, ts.URL+"/v1/price?asset="+usdcClassicID+"&quote=fiat:USD")
		if env.Data.Price != "0.9500000000" {
			t.Errorf("price = %q, want 0.9500000000 — a stale observation still beats the declaration", env.Data.Price)
		}
		if !env.Data.ObservedAt.Equal(staleAt) {
			t.Errorf("observed_at = %s, want the stale leg's %s", env.Data.ObservedAt, staleAt)
		}
		if !env.Flags.Stale {
			t.Errorf("flags.stale = false, want true — a fallback answer is below the closed-bucket contract")
		}
	})
}

// TestPrice_DeclaredPegIsNotStampedAsAFreshObservation pins the wire
// shape of the declaration under the DEFAULT stamp — the one the API
// binary runs with, since the operator declaration carries no timestamp
// of its own: the server's construction time.
//
// observed_at used to be time.Now() on every request, so the constant
// was indistinguishable on the wire from an observation taken this
// instant — live, two consecutive GETs came back 123ms apart, each
// carrying an observed_at equal to its own envelope as_of. It now
// predates the first request ever made (the server existed before the
// request did), does not advance between requests, and does not claim
// to have been observed after the response was built.
func TestPrice_DeclaredPegIsNotStampedAsAFreshObservation(t *testing.T) {
	usdc, err := canonical.ParseAsset(usdcClassicID)
	if err != nil {
		t.Fatalf("parse USDC: %v", err)
	}
	reader := &stubPriceReader{err: v1.ErrPriceNotFound}
	srv := v1.New(v1.Options{
		Prices:            reader,
		USDPeggedClassics: []canonical.Asset{usdc},
	})
	ts := startHTTPTest(t, srv.Handler())

	// Taken AFTER the server was built and BEFORE anything was asked of
	// it: the declaration was adopted before this instant, and a stamp
	// taken inside a request handler cannot precede it.
	firstRequestStart := time.Now().UTC()
	first := getPegEnvelope(t, ts.URL+"/v1/price?asset="+usdcClassicID+"&quote=fiat:USD")
	second := getPegEnvelope(t, ts.URL+"/v1/price?asset="+usdcClassicID+"&quote=fiat:USD")

	// The declaration itself is unchanged — this surface still answers
	// $1 for a peg no market prices (F-1232).
	if first.Data.Price != "1.000000000000" {
		t.Errorf("price = %q, want 1.000000000000", first.Data.Price)
	}
	if first.Data.PriceType != "peg" {
		t.Errorf("price_type = %q, want peg", first.Data.PriceType)
	}
	// The stamp predates the FIRST request: it is the adoption time, not
	// the request clock.
	if first.Data.ObservedAt.After(firstRequestStart) {
		t.Errorf("observed_at %s is after the first request began at %s — the constant is stamped with the request clock, not the declaration's adoption time",
			first.Data.ObservedAt, firstRequestStart)
	}
	// A constant is not re-observed per request.
	if !first.Data.ObservedAt.Equal(second.Data.ObservedAt) {
		t.Errorf("observed_at advanced between requests: %s then %s — a declaration is not an observation",
			first.Data.ObservedAt, second.Data.ObservedAt)
	}
	// And it predates the response: an observed_at that equals the
	// envelope's as_of reads as an observation taken this instant.
	if !first.Data.ObservedAt.Before(first.AsOf) {
		t.Errorf("observed_at %s is not before as_of %s — the constant is still stamped with the clock",
			first.Data.ObservedAt, first.AsOf)
	}
}
