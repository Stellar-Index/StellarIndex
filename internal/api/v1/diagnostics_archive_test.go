package v1_test

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"

	v1 "github.com/StellarIndex/stellar-index/internal/api/v1"
)

// TestDiagnosticsArchive_UnconfiguredIs503 — a deployment without an
// archive_report_path opts out of the endpoint entirely.
func TestDiagnosticsArchive_UnconfiguredIs503(t *testing.T) {
	srv := v1.New(v1.Options{})
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/diagnostics/archive")
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 when no report path configured", resp.StatusCode)
	}
	assertProblemNoStore(t, resp)
}

// TestDiagnosticsArchive_MissingFileIs404 — a configured path whose
// file doesn't exist yet is the legitimate "daemon hasn't run" state,
// not a server error.
func TestDiagnosticsArchive_MissingFileIs404(t *testing.T) {
	srv := v1.New(v1.Options{
		ArchiveReportPath: filepath.Join(t.TempDir(), "never-written.json"),
	})
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/diagnostics/archive")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 for a not-yet-written report", resp.StatusCode)
	}
	assertProblemNoStore(t, resp)
}

// TestDiagnosticsArchive_ServesReport — round-trips a daemon-shaped
// report file through the endpoint, pinning the wire field names the
// explorer panel consumes.
func TestDiagnosticsArchive_ServesReport(t *testing.T) {
	report := `{
	  "schema": "1",
	  "scanned_at": "2026-07-03T04:00:00Z",
	  "range": {"from": 2, "to": 63305532},
	  "cross_anchor": {
	    "archive_root": "/srv/history-archive",
	    "expected": 989148,
	    "found": 989147,
	    "missing_count": 1,
	    "missing": [63305471],
	    "truncated": false
	  }
	}`
	path := filepath.Join(t.TempDir(), "last-completeness-report.json")
	if err := os.WriteFile(path, []byte(report), 0o600); err != nil {
		t.Fatal(err)
	}

	srv := v1.New(v1.Options{ArchiveReportPath: path})
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/diagnostics/archive")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var env struct {
		Data v1.ArchiveReportView `json:"data"`
	}
	mustDecode(t, resp, &env)

	if env.Data.Schema != "1" {
		t.Errorf("schema = %q, want \"1\"", env.Data.Schema)
	}
	if env.Data.Range.From != 2 || env.Data.Range.To != 63305532 {
		t.Errorf("range = %+v, want [2, 63305532]", env.Data.Range)
	}
	ca := env.Data.CrossAnchor
	if ca == nil {
		t.Fatal("cross_anchor section missing")
	}
	if ca.Expected != 989148 || ca.Found != 989147 || ca.MissingCount != 1 {
		t.Errorf("cross_anchor = %+v, want expected=989148 found=989147 missing_count=1", ca)
	}
	if len(ca.Missing) != 1 || ca.Missing[0] != 63305471 {
		t.Errorf("cross_anchor.missing = %v, want [63305471]", ca.Missing)
	}
	if env.Data.Primary != nil {
		t.Errorf("primary = %+v, want nil (absent in report)", env.Data.Primary)
	}
}

// TestDiagnosticsArchive_UnparseableIs500 — a torn write / schema
// break is a server-side problem, not a 404.
func TestDiagnosticsArchive_UnparseableIs500(t *testing.T) {
	path := filepath.Join(t.TempDir(), "corrupt.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	srv := v1.New(v1.Options{ArchiveReportPath: path})
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/diagnostics/archive")
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 for an unparseable report", resp.StatusCode)
	}
	assertProblemNoStore(t, resp)
}

// assertProblemNoStore pins the cachecontrol.go invariant: every
// problem+json response carries Cache-Control: no-store so a CDN
// can't cache a transient failure against the success key.
func assertProblemNoStore(t *testing.T, resp *http.Response) {
	t.Helper()
	if ct := resp.Header.Get("Content-Type"); ct != "application/problem+json" {
		t.Errorf("Content-Type = %q, want application/problem+json", ct)
	}
	if cc := resp.Header.Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store on problem responses", cc)
	}
}
