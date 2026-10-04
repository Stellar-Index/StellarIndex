package timescale

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// errForAssetResolver prices every asset in prices and fails reads for the
// assets in failFor.
type errForAssetResolver struct {
	prices  map[string]string
	failFor map[string]bool
}

var errResolverRead = errors.New("rate store unreachable")

func (r errForAssetResolver) USDPriceAt(_ context.Context, a canonical.Asset, _ time.Time) (string, bool, error) {
	if r.failFor[a.String()] {
		return "", false, errResolverRead
	}
	p, ok := r.prices[a.String()]
	return p, ok, nil
}

func TestTradeUSDVolumeChecked_ResolverErrorIsReturned(t *testing.T) {
	t.Parallel()
	xlm := canonical.NativeAsset()
	// The anchor resolves XLM through its `native` form whichever wire form
	// the pool used, so failing `native` alone breaks only the anchor read.
	xlmSAC, _ := canonical.NewSorobanAsset(nativeXLMSAC)
	aqua, _ := canonical.NewClassicAsset("AQUA", "GBNZILSTVQZ4R7IKQDGHYGY2QXL5QOFJYQMXPKWRRM5PAV7Y4M67AQUA")
	tokn, _ := canonical.NewClassicAsset("TOKN", "GDHU6WRG4IEQXM5NZ4BMPKOXHW76MZM4Y2IEMFDVXBSDP6SJY4ITNPP2")

	cases := []struct {
		name string
		tr   canonical.Trade
		r    errForAssetResolver
	}{
		{
			// The XLM anchor errors while the quote leg is priceable: the
			// error must stop the waterfall, not fall through to the quote tier.
			name: "xlm base anchor error does not fall through to quote tier",
			tr:   mkClassicDEXTrade(t, "soroswap", xlmSAC, aqua, 50_000_000_000),
			r: errForAssetResolver{
				prices:  map[string]string{aqua.String(): "0.001", xlmSAC.String(): "0.1"},
				failFor: map[string]bool{xlm.String(): true},
			},
		},
		{
			name: "xlm quote anchor error does not fall through to quote tier",
			tr:   mkClassicDEXTrade(t, "soroswap", aqua, xlmSAC, 50_000_000_000),
			r: errForAssetResolver{
				prices:  map[string]string{aqua.String(): "0.001", xlmSAC.String(): "0.1"},
				failFor: map[string]bool{xlm.String(): true},
			},
		},
		{
			// Quote prices fine; the base leg's cross-check read fails, so
			// the candidate must be refused rather than served unchecked.
			name: "leg cross-check read error refuses the candidate",
			tr:   mkClassicDEXTrade(t, "soroswap", tokn, aqua, 50_000_000_000),
			r: errForAssetResolver{
				prices:  map[string]string{aqua.String(): "0.001"},
				failFor: map[string]bool{tokn.String(): true},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := tradeUSDVolumeChecked(context.Background(), tc.tr, nil, tc.r)
			if !errors.Is(err, errResolverRead) {
				t.Fatalf("err = %v, want the resolver error", err)
			}
			if got != nil {
				t.Errorf("got %q alongside an error, want nil", *got)
			}
		})
	}
}

func TestResolveUSDVolume_ErrorByGeneration(t *testing.T) {
	t.Parallel()
	xlm := canonical.NativeAsset()
	aqua, _ := canonical.NewClassicAsset("AQUA", "GBNZILSTVQZ4R7IKQDGHYGY2QXL5QOFJYQMXPKWRRM5PAV7Y4M67AQUA")
	tr := mkClassicDEXTrade(t, "soroswap", xlm, aqua, 50_000_000_000)
	failing := stubFXResolver{err: errResolverRead}

	live := &Store{usdVolumeFXResolver: failing, usdVolumeResolutionInstalled: true}
	if v, err := live.resolveUSDVolume(context.Background(), tr); err != nil || v != nil {
		t.Fatalf("gen 0: got (%v, %v), want (nil, nil): live path keeps NULL", v, err)
	}

	rederive := &Store{usdVolumeFXResolver: failing, usdVolumeResolutionInstalled: true, deriveGeneration: 7}
	v, err := rederive.resolveUSDVolume(context.Background(), tr)
	if !errors.Is(err, errResolverRead) {
		t.Fatalf("gen>0: err = %v, want the resolver error (write must fail closed)", err)
	}
	if v != nil {
		t.Errorf("gen>0: value %q returned alongside the error", *v)
	}
}
