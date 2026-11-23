package v1_test

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/pricingguard"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

const exclEURC = "EURC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"

// sourcedBucketReader is [minuteBucketReader] for a reader that reports the bucket's recorded sources.
type sourcedBucketReader struct{ sources []string }

func (r sourcedBucketReader) PriceAt(ctx context.Context, pair canonical.Pair, ts time.Time, m time.Duration) (string, time.Time, int, error) {
	v, at, res, _, err := r.PriceAtSources(ctx, pair, ts, m)
	return v, at, res, err
}

func (r sourcedBucketReader) PriceAtSources(ctx context.Context, pair canonical.Pair, ts time.Time, m time.Duration) (string, time.Time, int, []string, error) {
	v, at, res, err := minuteBucketReader{}.PriceAt(ctx, pair, ts, m)
	return v, at, res, r.sources, err
}

func priceAtGated(reader v1.PriceAtReader, rows []timescale.SourceExclusion) *v1.Server {
	usdc, _ := canonical.ParseAsset(exclUSDC)
	return v1.New(v1.Options{
		PriceAt:           reader,
		USDPeggedClassics: []canonical.Asset{usdc},
		Exclusions:        pricingguard.NewSourceExclusionGate(exclTable{rows: rows}, pricingguard.SourceExclusionGateOptions{}),
	})
}

// TestPriceAtJudgesBucketSources: a bucket that records its sources is withheld only by an exclusion naming one of them on its
// book or the requested pair; one with none recorded keeps the unattributed verdict.
func TestPriceAtJudgesBucketSources(t *testing.T) {
	ts := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Minute).Add(30 * time.Second)
	from := ts.Add(-time.Hour)
	at := func(asset, quote string) string {
		return "/v1/price/at?asset=" + asset + "&quote=" + quote + "&ts=" + ts.Format(time.RFC3339)
	}
	excl := func(base, quote string) []timescale.SourceExclusion {
		return []timescale.SourceExclusion{{ID: 1, Source: "soroswap", Base: base, Quote: quote, From: from}}
	}
	sdex, both := sourcedBucketReader{[]string{"sdex"}}, sourcedBucketReader{[]string{"sdex", "soroswap"}}
	for _, tc := range []struct {
		name     string
		reader   v1.PriceAtReader
		rows     []timescale.SourceExclusion
		path     string
		withheld bool
	}{
		{"other book, bucket without the source", sdex, excl("native", exclEURC), at("native", exclUSDC), false},
		{"same book, bucket without the source", sdex, excl("native", exclUSDC), at("native", exclUSDC), false},
		{"source-wide, bucket without it", sdex, excl("", ""), at("native", exclUSDC), false},
		{"source-wide, bucket with it", both, excl("", ""), at("native", exclUSDC), true},
		{"same book, bucket with it", both, excl("native", exclUSDC), at("native", exclUSDC), true},
		{"no sources recorded: unattributed", minuteBucketReader{}, excl("native", exclEURC), at("native", exclUSDC), true},
		{"source-wide, no sources recorded", minuteBucketReader{}, excl("", ""), at("native", exclUSDC), true},
		{"SAC-form exclusion, native read", both, excl(canonical.NativeSACContractID(), exclUSDC), at("native", exclUSDC), true},
		{"SAC-form exclusion, crypto:XLM read", both, excl(canonical.NativeSACContractID(), exclUSDC), at("crypto:XLM", exclUSDC), true},
		{"fiat-pair exclusion, stablecoin fallback", both, excl("native", "fiat:USD"), at("native", "fiat:USD"), true},
		{"fiat-pair exclusion, fallback without the source", sdex, excl("native", "fiat:USD"), at("native", "fiat:USD"), false},
	} {
		status, body := exclGet(t, priceAtGated(tc.reader, tc.rows), tc.path)
		served := status == http.StatusOK && strings.Contains(body, `"price":"0.25"`)
		refused := status == http.StatusNotFound && strings.Contains(body, `"source_excluded"`) && !strings.Contains(body, "0.25")
		if (tc.withheld && !refused) || (!tc.withheld && !served) {
			t.Errorf("%s: %d %s, want withheld=%v", tc.name, status, body, tc.withheld)
		}
	}
}
