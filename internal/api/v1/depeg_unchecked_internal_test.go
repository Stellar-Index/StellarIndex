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
	} {
		s := &Server{Options: Options{PriceAt: c.reader, USDPeggedClassics: []canonical.Asset{peg}}}
		f := s.withProxyDeviation(context.Background(), Flags{Triangulated: true}, time.Now())
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
