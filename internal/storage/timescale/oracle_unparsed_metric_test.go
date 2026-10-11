package timescale

import (
	"bytes"
	"context"
	"database/sql/driver"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
)

// TestOracleStreamUnparsedRowsAreCounted pins that an oracle_updates row whose stored canonical
// text will not parse is dropped loudly (log and metric). Dropping is correct, since there is
// nothing to serve for an asset we cannot name, but silence matters here: the documented fix
// for a mislabelled row is a raw SQL UPDATE on a column with no CHECK constraint, so a typo
// would make the row vanish from /v1/oracle/streams and look like a successful relabel.
//
// This asserts the parser's verdict on the shapes an operator typo produces;
// [TestLatestOracleStreams_DropsUnparseableRowsLoudly] drives the real reader.
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
}

// TestLatestOracleStreams_DropsUnparseableRowsLoudly drives [Store.LatestOracleStreams] itself
// through the scripted driver rather than self-incrementing the counter: `dropped` could go
// unincremented in a parse-fail continue branch, leaving the slog.Warn summary unreachable while
// the per-row counter still fired. One healthy row, one with an unparseable asset and one with
// an unparseable quote exercise both continue branches.
func TestLatestOracleStreams_DropsUnparseableRowsLoudly(t *testing.T) {
	var buf bytes.Buffer
	prevLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prevLogger) })

	const source = "gh1219-fixture"
	beforeAsset := testutil.ToFloat64(obs.OracleStreamRowsUnparsedTotal.WithLabelValues(source, "asset"))
	beforeQuote := testutil.ToFloat64(obs.OracleStreamRowsUnparsedTotal.WithLabelValues(source, "quote"))

	ts := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	store, _ := newScriptedStore(t, scriptedResult{
		cols: []string{
			"source", "contract_id", "ledger", "tx_hash", "op_index", "ts",
			"asset", "quote", "price", "decimals", "confidence", "observer", "published_price",
		},
		rows: [][]driver.Value{
			{source, "", int64(1000), "tx-good", int64(0), ts, "native", "fiat:USD", "100", int64(7), 0.9, "", nil},
			// Truncated + lower-cased strkey — same shape TestOracleStreamUnparsedRowsAreCounted pins as unparseable.
			{source, "", int64(1001), "tx-bad-asset", int64(0), ts, "usdc-ga5zsejyb37jrc5", "fiat:USD", "100", int64(7), 0.9, "", nil},
			// Missing the fiat: prefix.
			{source, "", int64(1002), "tx-bad-quote", int64(0), ts, "native", "USD", "100", int64(7), 0.9, "", nil},
		},
	})

	out, err := store.LatestOracleStreams(context.Background())
	if err != nil {
		t.Fatalf("LatestOracleStreams: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("got %d row(s), want 1 (the two unparseable rows must be dropped): %+v", len(out), out)
	}
	if out[0].TxHash != "tx-good" {
		t.Errorf("surviving row has tx_hash %q, want the healthy row tx-good", out[0].TxHash)
	}

	if got := testutil.ToFloat64(obs.OracleStreamRowsUnparsedTotal.WithLabelValues(source, "asset")) - beforeAsset; got != 1 {
		t.Errorf("asset-field unparsed counter moved by %v, want exactly 1", got)
	}
	if got := testutil.ToFloat64(obs.OracleStreamRowsUnparsedTotal.WithLabelValues(source, "quote")) - beforeQuote; got != 1 {
		t.Errorf("quote-field unparsed counter moved by %v, want exactly 1", got)
	}

	logs := buf.String()
	if !strings.Contains(logs, "rows dropped for unparseable asset/quote") {
		t.Fatalf("dropped rows produced no drop-summary log line — 'no longer SILENT' is the file's own "+
			"claim; log output:\n%s", logs)
	}
	if !strings.Contains(logs, "dropped=2") {
		t.Errorf("drop summary does not report dropped=2 for the two unparseable rows, log output:\n%s", logs)
	}
	if !strings.Contains(logs, "returned=1") {
		t.Errorf("drop summary does not report returned=1, log output:\n%s", logs)
	}
}
