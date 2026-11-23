package v1_test

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/pricingguard"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// minuteBucketReader serves native/USDC as a closed 1m bucket closing at the minute at-or-before ts; it records no sources, as
// the CAGG ladder behind /v1/price/at and /v1/price/changes does not.
type minuteBucketReader struct{}

func (minuteBucketReader) PriceAt(_ context.Context, pair canonical.Pair, ts time.Time, _ time.Duration) (string, time.Time, int, error) {
	if pair.Base.String() != "native" || pair.Quote.String() != exclUSDC {
		return "", time.Time{}, 0, v1.ErrPriceAtUnavailable
	}
	return "0.25", ts.Truncate(time.Minute), 60, nil
}

func priceAtExclServer(gated bool, rows []timescale.SourceExclusion, err error) *v1.Server {
	opts := v1.Options{PriceAt: minuteBucketReader{}}
	if gated {
		opts.Exclusions = pricingguard.NewSourceExclusionGate(exclTable{rows: rows, err: err}, pricingguard.SourceExclusionGateOptions{})
	}
	return v1.New(opts)
}

func soroswapExcluded(from, until time.Time) []timescale.SourceExclusion {
	return []timescale.SourceExclusion{{ID: 1, Source: "soroswap", Base: "native", Quote: exclUSDC, From: from, Until: until}}
}

// TestPriceAtWithholdsExcludedBucket: a point-in-time bucket pools every source, so an exclusion in force over its span
// withholds it; one outside the span leaves the response byte-identical, and an unreadable table withholds like /v1/ohlc.
func TestPriceAtWithholdsExcludedBucket(t *testing.T) {
	ts := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Minute).Add(30 * time.Second)
	bucketClose := ts.Truncate(time.Minute)
	path := "/v1/price/at?asset=native&quote=" + exclUSDC + "&ts=" + ts.Format(time.RFC3339)

	status, baseline := exclGet(t, priceAtExclServer(false, nil, nil), path)
	if status != http.StatusOK || !strings.Contains(baseline, `"price":"0.25"`) {
		t.Fatalf("no gate: %d %s, want the bucket served", status, baseline)
	}
	for name, rows := range map[string][]timescale.SourceExclusion{
		"no exclusions":          nil,
		"exclusion after bucket": soroswapExcluded(bucketClose.Add(time.Minute), time.Time{}),
		"exclusion before it":    soroswapExcluded(bucketClose.Add(-time.Hour), bucketClose.Add(-time.Minute)),
		"another pair excluded":  {{ID: 1, Source: "soroswap", Base: exclAQUA, Quote: exclUSDC, From: bucketClose.Add(-time.Hour)}},
	} {
		if status, body := exclGet(t, priceAtExclServer(true, rows, nil), path); status != http.StatusOK || exclAsOf.ReplaceAllString(body, "") != exclAsOf.ReplaceAllString(baseline, "") {
			t.Errorf("%s: %d %s, want byte-identical %s", name, status, body, baseline)
		}
	}
	covering := soroswapExcluded(bucketClose.Add(-30*time.Second), bucketClose.Add(time.Hour))
	if status, body := exclGet(t, priceAtExclServer(true, covering, nil), path); status != http.StatusNotFound ||
		!strings.Contains(body, "price-withheld") || !strings.Contains(body, `"source_excluded"`) || strings.Contains(body, "0.25") {
		t.Errorf("exclusion over the bucket: %d %s, want 404 price-withheld source_excluded", status, body)
	}
	if status, body := exclGet(t, priceAtExclServer(true, nil, errors.New("db down")), path); status != http.StatusNotFound ||
		!strings.Contains(body, "price-withheld") || strings.Contains(body, "0.25") {
		t.Errorf("exclusions unread: %d %s, want a withheld 404, never the bucket", status, body)
	}
}

// TestHistoryWithholdsPageWithExcludedFill: a trade page holding a fill an exclusion covers is withheld, never served with
// the row silently dropped; an exclusion outside the page's fills leaves it byte-identical; an unreadable table withholds.
func TestHistoryWithholdsPageWithExcludedFill(t *testing.T) {
	excluded, other := mkHistTrade(100), mkHistTrade(101)
	other.Source, other.Ledger = "aquarius", 2
	at := excluded.Timestamp
	srv := func(gated bool, rows []timescale.SourceExclusion, err error) *v1.Server {
		opts := v1.Options{History: &stubHistoryReader{trades: []canonical.Trade{excluded, other}}}
		if gated {
			opts.Exclusions = pricingguard.NewSourceExclusionGate(exclTable{rows: rows, err: err}, pricingguard.SourceExclusionGateOptions{})
		}
		return v1.New(opts)
	}
	window := func(from, until time.Time) []timescale.SourceExclusion {
		return []timescale.SourceExclusion{{ID: 1, Source: "soroswap", Base: "native", Quote: "fiat:USD", From: from, Until: until}}
	}
	path := "/v1/history?base=native&quote=fiat:USD&limit=50"
	status, baseline := exclGet(t, srv(false, nil, nil), path)
	if status != http.StatusOK || !strings.Contains(baseline, `"soroswap"`) || !strings.Contains(baseline, `"aquarius"`) {
		t.Fatalf("no gate: %d %s, want both fills", status, baseline)
	}
	for name, rows := range map[string][]timescale.SourceExclusion{
		"no exclusions":        nil,
		"window after fills":   window(at.Add(time.Second), time.Time{}),
		"another source":       {{ID: 1, Source: "phoenix", From: at.Add(-time.Hour)}},
		"window before fills":  window(at.Add(-time.Hour), at.Add(-time.Minute)),
		"another pair's fills": {{ID: 1, Source: "soroswap", Base: exclAQUA, Quote: exclUSDC, From: at.Add(-time.Hour)}},
	} {
		if status, body := exclGet(t, srv(true, rows, nil), path); status != http.StatusOK ||
			exclAsOf.ReplaceAllString(body, "") != exclAsOf.ReplaceAllString(baseline, "") {
			t.Errorf("%s: %d %s, want byte-identical %s", name, status, body, baseline)
		}
	}
	if status, body := exclGet(t, srv(true, window(at.Add(-time.Minute), at.Add(time.Minute)), nil), path); status != http.StatusNotFound ||
		!strings.Contains(body, `"source_excluded"`) || strings.Contains(body, `"aquarius"`) {
		t.Errorf("soroswap excluded over its fill: %d %s, want 404 source_excluded", status, body)
	}
	if status, body := exclGet(t, srv(true, nil, errors.New("db down")), path); status != http.StatusNotFound ||
		!strings.Contains(body, "price-withheld") {
		t.Errorf("exclusions unread: %d %s, want a withheld 404", status, body)
	}
}

// TestPriceChangesWithholdsExcludedHorizon: an exclusion over one horizon's reference bucket marks that horizon withheld and
// leaves its siblings and the anchor served; one over the anchor withholds the response; an unreadable table withholds it.
func TestPriceChangesWithholdsExcludedHorizon(t *testing.T) {
	path := "/v1/price/changes?asset=native&quote=" + exclUSDC
	day := time.Now().UTC().Add(-24 * time.Hour)
	h24 := `"24h":{"change_pct":null,"reference_price":null,"reference_at":null,"resolution":null,"available":false,"withheld":true}`

	status, baseline := exclGet(t, priceAtExclServer(false, nil, nil), path)
	if status != http.StatusOK || strings.Contains(baseline, `"withheld":true`) {
		t.Fatalf("no gate: %d %s, want every horizon served", status, baseline)
	}
	status, body := exclGet(t, priceAtExclServer(true, soroswapExcluded(day.Add(-10*time.Minute), day.Add(10*time.Minute)), nil), path)
	if status != http.StatusOK || !strings.Contains(body, h24) || strings.Count(body, `"available":true`) != 3 {
		t.Errorf("exclusion over the 24h reference: %d %s, want h24 withheld and h1, d7, d30 served", status, body)
	}
	if status, body := exclGet(t, priceAtExclServer(true, soroswapExcluded(day, time.Time{}), nil), path); status != http.StatusNotFound ||
		!strings.Contains(body, `"source_excluded"`) {
		t.Errorf("exclusion over the anchor: %d %s, want 404 source_excluded", status, body)
	}
	if status, body := exclGet(t, priceAtExclServer(true, nil, errors.New("db down")), path); status != http.StatusNotFound ||
		!strings.Contains(body, "price-withheld") {
		t.Errorf("exclusions unread: %d %s, want a withheld 404", status, body)
	}
	if status, body := exclGet(t, priceAtExclServer(true, soroswapExcluded(day.Add(-48*time.Hour), day.Add(-47*time.Hour)), nil), path); status != http.StatusOK ||
		strings.Contains(body, `"withheld":true`) {
		t.Errorf("exclusion between horizons: %d %s, want every horizon served", status, body)
	}
}
