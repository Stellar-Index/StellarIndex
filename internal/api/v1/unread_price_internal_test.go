package v1

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// depegProbeReader answers every peg observation with one error or value.
type depegProbeReader struct {
	value string
	err   error
}

func (d depegProbeReader) PriceAt(context.Context, canonical.Pair, time.Time, time.Duration) (string, time.Time, int, error) {
	return d.value, time.Time{}, 0, d.err
}

// TestWithProxyDeviation_UnreadPegIsDegraded: a peg check that did not answer must not leave a clean, cacheable "no depeg".
func TestWithProxyDeviation_UnreadPegIsDegraded(t *testing.T) {
	peg := mustAsset(t, withheldTestPegIssuer)
	cases := []struct {
		name               string
		reader             depegProbeReader
		deviated, degraded bool
	}{
		{"read failed", depegProbeReader{err: errors.New("pool timeout")}, false, true},
		{"no observation", depegProbeReader{err: ErrPriceAtUnavailable}, false, false},
		{"in band", depegProbeReader{value: "1.0001"}, false, false},
		{"depegged", depegProbeReader{value: "0.87"}, true, false},
	}
	for _, c := range cases {
		s := &Server{Options: Options{PriceAt: c.reader, USDPeggedClassics: []canonical.Asset{peg}}}
		got := s.withProxyDeviation(context.Background(), Flags{Triangulated: true}, time.Now())
		if got.ProxyDeviation != c.deviated || got.Degraded != c.degraded {
			t.Errorf("%s: proxy_deviation=%v degraded=%v, want %v %v", c.name, got.ProxyDeviation, got.Degraded, c.deviated, c.degraded)
		}
	}
	s := &Server{Options: Options{PriceAt: depegProbeReader{err: errors.New("pool timeout")}, USDPeggedClassics: []canonical.Asset{peg}}}
	if got := s.withProxyDeviation(context.Background(), Flags{}, time.Now()); got.Degraded {
		t.Error("a direct (non-triangulated) answer has no peg leg to check")
	}
}

// TestPopulatePriceUSD_UnreadIsNotAbsent: a failed price read marks the detail, so its null price_usd is not cached as "no market".
func TestPopulatePriceUSD_UnreadIsNotAbsent(t *testing.T) {
	asset := mustAsset(t, withheldTestAsset)
	peg := mustAsset(t, withheldTestPegIssuer)
	direct := asset.String() + "/" + defaultPriceQuote.String()
	viaPeg := asset.String() + "/" + peg.String()
	boom := errors.New("pool timeout")
	cases := []struct {
		name   string
		reader withheldUSDReader
		unread bool
	}{
		{"direct read failed", withheldUSDReader{direct: {err: boom}}, true},
		{"proxy read failed", withheldUSDReader{viaPeg: {err: boom}}, true},
		{"direct failed, proxy served", withheldUSDReader{direct: {err: boom}, viaPeg: {price: "0.5"}}, true},
		{"never traded", withheldUSDReader{}, false},
		{"pair mismatch is a verdict", withheldUSDReader{direct: {err: canonical.ErrPairMismatch}}, false},
		{"served", withheldUSDReader{direct: {price: "0.25"}}, false},
	}
	for _, c := range cases {
		s := &Server{
			Options: Options{Prices: c.reader, USDPeggedClassics: []canonical.Asset{peg}},
			logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		}
		var detail AssetDetail
		s.populatePriceUSD(context.Background(), &detail, asset)
		if detail.priceUnread != c.unread {
			t.Errorf("%s: priceUnread = %v, want %v", c.name, detail.priceUnread, c.unread)
		}
	}
}

// TestPopulatePriceUSD_UnreadAliasIsNotAbsent: a not-found on one XLM spelling must not hide a failed read of another.
func TestPopulatePriceUSD_UnreadAliasIsNotAbsent(t *testing.T) {
	s := &Server{
		Options: Options{Prices: withheldUSDReader{"crypto:XLM/" + defaultPriceQuote.String(): {err: errors.New("pool timeout")}}},
		logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	var detail AssetDetail
	s.populatePriceUSD(context.Background(), &detail, canonical.Asset{Type: canonical.AssetNative})
	if !detail.priceUnread {
		t.Error("priceUnread = false with crypto:XLM unread; a null price_usd would be cached as no market")
	}
}
