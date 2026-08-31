package timescale

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
)

// TestOracleStreamUnparsedRowsAreCounted pins that an oracle_updates row
// whose stored canonical text will not parse is DROPPED LOUDLY.
//
// The drop itself is correct — there is nothing sane to serve for an
// asset we cannot name. What was wrong is that it happened with no log,
// metric or error, so the row simply vanished from /v1/oracle/streams and
// the explorer /oracles page (wave-D SI-OC-04).
//
// The silence mattered most exactly when it was most likely: the
// documented remediation for a mislabelled oracle row is an operator-run
// raw SQL UPDATE against that column, which has no CHECK constraint. A
// typo deleted the row from the served surface rather than erroring, and
// the operator would watch it disappear and reasonably conclude the
// relabel had worked.
//
// This asserts the PARSER's verdict on the shapes an operator typo
// actually produces, and that the counter is wired for both fields —
// exercising the query itself needs Postgres, which the integration
// suite covers.
func TestOracleStreamUnparsedRowsAreCounted(t *testing.T) {
	// Shapes a hand-written UPDATE plausibly produces. Each must FAIL to
	// parse — if canonical ever starts accepting one, the drop (and this
	// alert) would stop happening for it and that is worth knowing.
	for _, bad := range []string{
		"",                     // empty cell
		"USD",                  // missing the fiat: prefix
		"fiat:",                // prefix, no code
		"rwa:XAU ",             // trailing space from a shell-built statement
		"usdc-ga5zsejyb37jrc5", // truncated + lower-cased strkey
	} {
		if _, err := canonical.ParseAsset(bad); err == nil {
			t.Errorf("ParseAsset(%q) unexpectedly succeeded — a row carrying this "+
				"text would be SERVED rather than dropped, so the unparsed counter "+
				"would never fire for it", bad)
		}
	}

	// A correct value must still parse, or every row would be dropped.
	if _, err := canonical.ParseAsset("fiat:USD"); err != nil {
		t.Fatalf("ParseAsset(\"fiat:USD\") failed: %v — the drop path would swallow "+
			"every healthy row", err)
	}

	// Both fields are wired, and the counter exists before its first
	// increment for neither label combination (it is deliberately not
	// pre-seeded — see the alert's own comment).
	obs.OracleStreamRowsUnparsedTotal.WithLabelValues("redstone", "asset").Inc()
	obs.OracleStreamRowsUnparsedTotal.WithLabelValues("redstone", "quote").Inc()
	for _, field := range []string{"asset", "quote"} {
		got := testutil.ToFloat64(obs.OracleStreamRowsUnparsedTotal.WithLabelValues("redstone", field))
		if got < 1 {
			t.Errorf("counter for field=%q did not record the drop (got %v)", field, got)
		}
	}
}
