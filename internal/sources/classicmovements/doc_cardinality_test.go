package classicmovements

import (
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/stellar/go-stellar-sdk/xdr"
)

// TestCAP0038CardinalityDocsMatchDecoder pins the prose specs of a
// CAP-0038 liquidation's row cardinality to what DecodeCAP0038Revocation
// actually emits, so they cannot keep describing only the withdraw leg.
func TestCAP0038CardinalityDocsMatchDecoder(t *testing.T) {
	native := xdr.Asset{Type: xdr.AssetTypeAssetTypeNative}
	usdc := mkAlphanum4Asset(t, "USDC", 0x99)
	changes := []EntryChangeXDR{
		mkClaimableBalanceCreatedChange(t, 0x9A, native, 800),
		mkClaimableBalanceCreatedChange(t, 0x9B, usdc, 4000),
	}
	movements, err := DecodeCAP0038Revocation(40_000_000, time.Time{}, "tx8", 0,
		mkAllowTrustOp(t, 0x98, "USDC", 0), mkAllowTrustSuccessResult(), changes)
	if err != nil {
		t.Fatalf("DecodeCAP0038Revocation: %v", err)
	}
	kinds := map[Kind]bool{}
	for _, m := range movements {
		kinds[m.Kind] = true
	}
	spelled := map[int]string{2: "2|two", 4: "4|four", 6: "6|six"}[len(movements)]
	if spelled == "" {
		t.Fatalf("no spelling for %d movements; extend the map", len(movements))
	}
	count := regexp.MustCompile(`(?i)\b(` + spelled + `) (rows|for a real)\b`)

	for _, doc := range []struct{ path, anchor, stop string }{
		{"README.md", "CAP-0038 edge only", "\n"},
		{"doc.go", "A CAP-0038 liquidation", "\n//\n"},
		{"../../../deploy/clickhouse/tier1_schema.sql", "CAP-0038 auto-liquidation edge", "\n--\n"},
	} {
		passage := cardinalityPassage(t, doc.path, doc.anchor, doc.stop)
		prose := strings.Join(strings.Fields(commentPrefix.ReplaceAllString(passage, " ")), " ")
		for k := range kinds {
			if !strings.Contains(passage, string(k)) {
				t.Errorf("%s: CAP-0038 cardinality passage omits emitted kind %q:\n%s", doc.path, k, passage)
			}
		}
		if !count.MatchString(prose) {
			t.Errorf("%s: CAP-0038 cardinality passage does not state the %d rows a two-asset liquidation emits:\n%s",
				doc.path, len(movements), passage)
		}
	}
}

// commentPrefix strips Go and SQL line-comment markers so a count that
// wraps across comment lines still reads as one phrase.
var commentPrefix = regexp.MustCompile(`(?m)^\s*(//|--)`)

// cardinalityPassage returns the text from the start of the line holding
// anchor up to the first stop delimiter after it.
func cardinalityPassage(t *testing.T, path, anchor, stop string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	s := string(raw)
	i := strings.Index(s, anchor)
	if i < 0 {
		t.Fatalf("%s: anchor %q not found", path, anchor)
	}
	i = strings.LastIndex(s[:i], "\n") + 1
	if j := strings.Index(s[i:], stop); j >= 0 {
		return s[i : i+j]
	}
	return s[i:]
}
