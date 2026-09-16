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
)

// levels captures the level of every "http request" line a handler run
// produced, decoded from the real slog JSON output rather than from a
// recording handler, so the test sees what the journal would.
func levels(t *testing.T, buf *bytes.Buffer) []string {
	t.Helper()
	var out []string
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("log line is not JSON: %q (%v)", line, err)
		}
		if rec["msg"] != "http request" {
			continue
		}
		lvl, _ := rec["level"].(string)
		out = append(out, lvl)
	}
	return out
}

func runOnce(t *testing.T, ua string, status int, level slog.Level) *bytes.Buffer {
	t.Helper()
	buf := &bytes.Buffer{}
	logger := slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: level}))

	h := Logger(logger)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
	}))
	req := httptest.NewRequest(http.MethodGet, "/v1/price", nil)
	if ua != "" {
		req.Header.Set("User-Agent", ua)
	}
	h.ServeHTTP(httptest.NewRecorder(), req)
	return buf
}

// The SLA probe drives ~800 requests per endpoint per run across ten
// endpoints every 15 minutes. Measured on r1 2026-09-16, that was
// 287,914 API journal entries in 5.4 hours — 98% of everything the
// journal held — which collapsed a configured 14-day retention into
// about five hours and aged that morning's own outage out of the
// journal before lunchtime.
//
// So a successful request from first-party synthetic traffic logs at
// DEBUG: emitted, but below the level a production journal stores.
func TestSuccessfulSyntheticRequestDoesNotSpendJournalRetention(t *testing.T) {
	for _, ua := range []string{"stellarindex-probe/1", "stellarindex-smoke/1", "stellarindex-prewarm/1"} {
		t.Run(ua, func(t *testing.T) {
			// At the level a production journal runs, the line is absent.
			if got := levels(t, runOnce(t, ua, http.StatusOK, slog.LevelInfo)); len(got) != 0 {
				t.Errorf("%s produced %v at INFO; synthetic success must not reach the journal", ua, got)
			}
			// It is emitted, not discarded — an operator who turns the
			// level down still gets it.
			if got := levels(t, runOnce(t, ua, http.StatusOK, slog.LevelDebug)); len(got) != 1 || got[0] != "DEBUG" {
				t.Errorf("%s logged %v at DEBUG level, want exactly one DEBUG line", ua, got)
			}
		})
	}
}

// The demotion is for successful requests only. A probe that sees a 5xx
// is the most valuable line in the file — it is a failure observed by
// something whose whole job is to observe — and demoting it would hide
// an outage from the journal to save space during one.
func TestFailingSyntheticRequestStillReachesTheJournal(t *testing.T) {
	cases := []struct {
		status int
		want   string
	}{
		{http.StatusInternalServerError, "ERROR"},
		{http.StatusBadRequest, "WARN"},
		{http.StatusNotFound, "WARN"},
	}
	for _, tc := range cases {
		got := levels(t, runOnce(t, "stellarindex-probe/1", tc.status, slog.LevelInfo))
		if len(got) != 1 || got[0] != tc.want {
			t.Errorf("synthetic %d logged %v, want one %s line", tc.status, got, tc.want)
		}
	}
}

// Real traffic is untouched: an unrecognised User-Agent keeps its INFO
// line. A change that quietened the journal by quietening customers
// would be the wrong fix to the same problem.
func TestCustomerRequestStillLogsAtInfo(t *testing.T) {
	for _, ua := range []string{"", "Go-http-client/1.1", "Mozilla/5.0", "stellarindex-is-not-a-prefix"} {
		got := levels(t, runOnce(t, ua, http.StatusOK, slog.LevelInfo))
		if len(got) != 1 || got[0] != "INFO" {
			t.Errorf("User-Agent %q logged %v, want one INFO line", ua, got)
		}
	}
}
