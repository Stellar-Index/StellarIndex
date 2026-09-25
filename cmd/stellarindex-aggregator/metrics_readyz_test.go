package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
)

type fakeSchemaReader struct{ version uint }

func (f fakeSchemaReader) SchemaMigrationVersion(context.Context) (uint, bool, error) {
	return f.version, false, nil
}

// TestMetricsMuxReadyzGatesOnSchemaHead pins GH-1167: the deploy gate
// probes the aggregator's /readyz, which must 503 when the applied schema is
// behind the binary, while /healthz stays a constant liveness 200.
func TestMetricsMuxReadyzGatesOnSchemaHead(t *testing.T) {
	cases := []struct {
		name    string
		applied uint
		want    int
	}{
		{"schema behind binary", v1.ExpectedSchemaVersion - 1, http.StatusServiceUnavailable},
		{"schema at binary head", v1.ExpectedSchemaVersion, http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mux := newMetricsMux(fakeSchemaReader{version: tc.applied})

			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
			if rec.Code != tc.want {
				t.Fatalf("/readyz = %d, want %d (body %q)", rec.Code, tc.want, rec.Body.String())
			}
			if tc.want != http.StatusOK && !strings.Contains(rec.Body.String(), "schema") {
				t.Fatalf("/readyz body %q does not name the failed schema check", rec.Body.String())
			}

			rec = httptest.NewRecorder()
			mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
			if rec.Code != http.StatusOK {
				t.Fatalf("/healthz = %d, want 200 regardless of schema", rec.Code)
			}
		})
	}
}
