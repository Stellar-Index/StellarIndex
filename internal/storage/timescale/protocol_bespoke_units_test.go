package timescale

import (
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// The r1 value that rendered as "10,687,415.41T token-units" sits above 2^63; scaling must stay exact.
func TestBLNDUnits_ExactAboveInt64(t *testing.T) {
	for raw, want := range map[string]string{
		"10687415316550896518": "1068741531655.0896518",
		"22970856108738":       "2297085.6108738",
		"1":                    "0.0000001",
		"0":                    "0.0000000",
	} {
		a, err := canonical.FromString(raw)
		if err != nil {
			t.Fatal(err)
		}
		if got := blndUnits(a); got != want {
			t.Errorf("blndUnits(%s) = %s, want %s", raw, got, want)
		}
	}
}
