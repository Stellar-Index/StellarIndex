package explorer

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// txCoverageReader is a TransactionByHash-found ExplorerReader whose per-op
// result-code and contract-event sub-reads can be made to error independently,
// so TxDetail runs its non-fatal-degrade path (tx.go). Everything else returns
// harmless zero values (embedded capReader).
type txCoverageReader struct {
	*capReader
	resultsErr error
	eventsErr  error
}

func (r *txCoverageReader) TransactionByHash(context.Context, string) (clickhouse.TxSummary, bool, error) {
	return clickhouse.TxSummary{Seq: 42}, true, nil
}

func (r *txCoverageReader) OperationsByTx(context.Context, uint32, string) ([]clickhouse.OpRow, error) {
	return nil, nil
}

func (r *txCoverageReader) OperationResultsByTx(context.Context, uint32, string) (map[uint32]clickhouse.OpResult, error) {
	if r.resultsErr != nil {
		return nil, r.resultsErr
	}
	return map[uint32]clickhouse.OpResult{}, nil
}

func (r *txCoverageReader) EventsByTx(context.Context, uint32, string) ([]clickhouse.EventSummary, error) {
	if r.eventsErr != nil {
		return nil, r.eventsErr
	}
	return nil, nil
}

// txDetailBody drives TxDetail against a txCoverageReader and returns the
// marshalled wire body. It captures the JSON bytes (not the Go struct) so the
// assertion is on the WIRE CONTRACT — the exact surface coverage_note is about — and
// stays valid whether or not the response type carries a coverage_note field.
func txDetailBody(t *testing.T, resultsErr, eventsErr error) map[string]any {
	t.Helper()
	var captured []byte
	h := &Handler{
		Reader:        &txCoverageReader{capReader: &capReader{probe: &deadlineProbe{}}, resultsErr: resultsErr, eventsErr: eventsErr},
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		ClientAborted: func(*http.Request, error) bool { return false },
		WriteProblem: func(w http.ResponseWriter, _ *http.Request, _, _ string, status int, _ string) {
			w.WriteHeader(status)
		},
		WriteJSON: func(w http.ResponseWriter, data any, _ bool) {
			b, err := json.Marshal(data)
			if err != nil {
				t.Fatalf("marshalling TxDetail response: %v", err)
			}
			captured = b
			_, _ = w.Write(b)
		},
	}

	r := httptest.NewRequest(http.MethodGet, "/v1/tx/"+validTestTxHash, nil)
	r.SetPathValue("hash", validTestTxHash)
	rec := httptest.NewRecorder()
	h.TxDetail(rec, r)

	if rec.Code != http.StatusOK {
		t.Fatalf("TxDetail returned status %d, want 200 (the reads degrade, they must not fail the request)", rec.Code)
	}
	if captured == nil {
		t.Fatal("TxDetail never wrote a JSON body")
	}
	var out map[string]any
	if err := json.Unmarshal(captured, &out); err != nil {
		t.Fatalf("unmarshalling captured body: %v", err)
	}
	return out
}

// A failed sub-read must also drop the route's shared-cache band, or a CDN
// replays the partial tx for the band's whole s-maxage after the read recovers.
func TestTxDetail_FailedSubReadDropsCacheBand(t *testing.T) {
	const band = "public, max-age=60, s-maxage=300"
	readErr := errors.New("clickhouse read failed")
	for _, tc := range []struct {
		name                  string
		resultsErr, eventsErr error
		want                  string
	}{
		{"both reads succeed", nil, nil, band},
		{"results read fails", readErr, nil, "no-store"},
		{"events read fails", nil, readErr, "no-store"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := &Handler{
				Reader:        &txCoverageReader{capReader: &capReader{probe: &deadlineProbe{}}, resultsErr: tc.resultsErr, eventsErr: tc.eventsErr},
				Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
				ClientAborted: func(*http.Request, error) bool { return false },
				WriteProblem: func(w http.ResponseWriter, _ *http.Request, _, _ string, status int, _ string) {
					w.WriteHeader(status)
				},
				WriteJSON: func(w http.ResponseWriter, _ any, _ bool) { w.WriteHeader(http.StatusOK) },
				WriteJSONAt: func(w http.ResponseWriter, _ any, _, degraded bool, _ time.Time) {
					if degraded {
						w.Header().Set("Cache-Control", "no-store")
					}
					w.WriteHeader(http.StatusOK)
				},
			}
			r := httptest.NewRequest(http.MethodGet, "/v1/tx/"+validTestTxHash, nil)
			r.SetPathValue("hash", validTestTxHash)
			rec := httptest.NewRecorder()
			rec.Header().Set("Cache-Control", band)
			h.TxDetail(rec, r)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", rec.Code)
			}
			if got := rec.Header().Get("Cache-Control"); got != tc.want {
				t.Errorf("Cache-Control = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestTxDetail_FailedSubReadSurfacesCoverageNote is the regression guard: a
// FAILED per-op-result-code read and a FAILED contract-event read must each
// surface a non-empty coverage_note distinct from a transaction that genuinely
// had none. Both `events` and each op's `result_code` are omitempty, so without a
// note a failed read serialises byte-identically to a real empty — this test
// pins that they differ.
func TestTxDetail_FailedSubReadSurfacesCoverageNote(t *testing.T) {
	readErr := errors.New("clickhouse read failed")

	// Baseline: both sub-reads succeed and are genuinely empty. This is the
	// "genuinely had none" wire shape the failure cases must be distinguishable
	// from — it MUST carry no coverage_note.
	genuinelyEmpty := txDetailBody(t, nil, nil)
	if note, present := genuinelyEmpty["coverage_note"]; present {
		t.Fatalf("a genuinely-empty tx must not carry a coverage_note, got %q", note)
	}

	cases := []struct {
		name       string
		resultsErr error
		eventsErr  error
		wantSubstr string
	}{
		{"results read fails", readErr, nil, "result_code"},
		{"events read fails", nil, readErr, "events"},
		{"both reads fail", readErr, readErr, "result_code"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := txDetailBody(t, tc.resultsErr, tc.eventsErr)

			noteRaw, present := body["coverage_note"]
			if !present {
				t.Fatalf("a failed sub-read must surface a coverage_note, but the field is absent — "+
					"the failure is byte-identical to a genuinely-empty tx (W1.2). body=%v", body)
			}
			note, _ := noteRaw.(string)
			if note == "" {
				t.Fatalf("coverage_note present but empty — a failed read must be distinguishable from empty (W1.2)")
			}
			// Non-vacuous: the failing response must actually DIFFER on the wire
			// from the genuinely-empty one.
			if _, ok := genuinelyEmpty["coverage_note"]; ok {
				t.Fatal("baseline unexpectedly carried a coverage_note")
			}
			if !strings.Contains(strings.ToLower(note), strings.ToLower(tc.wantSubstr)) {
				t.Fatalf("coverage_note %q does not mention the degraded field %q", note, tc.wantSubstr)
			}
		})
	}
}
