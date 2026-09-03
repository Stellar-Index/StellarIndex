package explorer

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestTxDetail_SubReadDeadlineIs503NotAPartial202OK pins the boundary
// between the two things a failed sub-read can mean on /v1/tx/{hash}.
//
// A plain read failure is the non-fatal class: serve the transaction
// without per-op result codes or contract events, and say so in
// coverage_note (W1.2). A blown DEADLINE is not that class. The budget
// belongs to the whole request, so every remaining sub-read is already
// doomed, and the "partial" 200 assembled out of nothing but failures
// tells the caller the transaction emitted no events — a wrong answer
// served with full confidence, where the truthful answer is "retry".
//
// The two fatal reads above these already take that branch; this pins
// that the non-fatal pair now agrees with them.
func TestTxDetail_SubReadDeadlineIs503NotAPartial200OK(t *testing.T) {
	cases := map[string]struct{ resultsErr, eventsErr error }{
		"result-code read hits the deadline": {context.DeadlineExceeded, nil},
		"event read hits the deadline":       {nil, context.DeadlineExceeded},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			h := &Handler{
				Reader: &txCoverageReader{
					capReader:  &capReader{probe: &deadlineProbe{}},
					resultsErr: tc.resultsErr,
					eventsErr:  tc.eventsErr,
				},
				Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
				ClientAborted: func(*http.Request, error) bool { return false },
				WriteProblem: func(w http.ResponseWriter, _ *http.Request, _, _ string, status int, _ string) {
					w.WriteHeader(status)
				},
				WriteJSON: func(w http.ResponseWriter, _ any, _ bool) {
					w.WriteHeader(http.StatusOK)
				},
			}
			r := httptest.NewRequest(http.MethodGet, "/v1/tx/"+validTestTxHash, nil)
			r.SetPathValue("hash", validTestTxHash)
			rec := httptest.NewRecorder()
			h.TxDetail(rec, r)

			if rec.Code == http.StatusOK {
				t.Fatalf("status = 200 — a sub-read that blew the request deadline must not be " +
					"served as a partial transaction; the caller cannot tell it from a tx that " +
					"genuinely emitted nothing")
			}
			if rec.Code != http.StatusServiceUnavailable {
				t.Fatalf("status = %d, want 503 (retryable, same branch the fatal reads take)", rec.Code)
			}
		})
	}
}
