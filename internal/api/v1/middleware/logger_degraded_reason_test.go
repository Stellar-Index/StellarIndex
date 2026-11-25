// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package middleware

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/obs"
)

// The request line carries degraded_reason only for a degraded response, with the real HTTPMetrics outside Logger as in the server stack.
func TestLogger_degradedReasonField(t *testing.T) {
	cases := []struct {
		name string
		mark func(w http.ResponseWriter)
		want any
	}{
		{"named", func(w http.ResponseWriter) {
			obs.MarkDegradedReason(w, obs.DegradedRefreshFailed)
			obs.MarkResponseFlags(w, true, true)
		}, obs.DegradedRefreshFailed},
		{"unnamed degraded", func(w http.ResponseWriter) { obs.MarkResponseFlags(w, true, false) }, obs.DegradedOther},
		{"stale only", func(w http.ResponseWriter) { obs.MarkResponseFlags(w, false, true) }, nil},
		{"clean", func(http.ResponseWriter) {}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			buf := &bytes.Buffer{}
			logger := slog.New(slog.NewJSONHandler(buf, nil))
			h := obs.HTTPMetrics(Logger(logger)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				tc.mark(w)
			})))
			h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/v1/price", nil))

			var rec map[string]any
			if err := json.Unmarshal([]byte(strings.TrimSpace(buf.String())), &rec); err != nil {
				t.Fatalf("log line: %v (%q)", err, buf.String())
			}
			if got := rec["degraded_reason"]; got != tc.want {
				t.Errorf("degraded_reason = %v, want %v", got, tc.want)
			}
		})
	}
}
