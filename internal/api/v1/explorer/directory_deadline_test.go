package explorer

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// deadlineDirectoryReader fails the batch lookup the way an expired
// explorerReadTimeout does — the driver's message WRAPPING
// context.DeadlineExceeded, which is how it arrives in production.
type deadlineDirectoryReader struct{ DirectoryReader }

func (deadlineDirectoryReader) DirectoryEntriesByAddresses(
	context.Context, []string,
) (map[string]timescale.DirectoryEntry, error) {
	return nil, fmt.Errorf("read account_directory: %w", context.DeadlineExceeded)
}

// GET /v1/directory was the one handler in this package still mapping a
// blown explorerReadTimeout to `errors/internal` 500 — every sibling
// listing already pairs retryableColdMiss with a `…-timeout` 503. The
// request context is alive when it happens (the 8s budget fires well
// inside the 15s blanket deadline), so nothing downstream could correct
// it: the 500 reached the wire, and the sla-probe booked a permanent
// availability failure for a condition a retry clears.
func TestDirectoryLookup_ReadDeadlineIs503NotInternal500(t *testing.T) {
	const addr = "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
	h := &Handler{
		Directory:     deadlineDirectoryReader{},
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		ClientAborted: func(*http.Request, error) bool { return false },
		WriteProblem: func(w http.ResponseWriter, _ *http.Request, _, _ string, status int, _ string) {
			w.WriteHeader(status)
		},
		WriteJSON: func(w http.ResponseWriter, _ any, _ bool) {
			w.WriteHeader(http.StatusOK)
		},
	}
	rec := httptest.NewRecorder()
	h.DirectoryLookup(rec, httptest.NewRequest(http.MethodGet, "/v1/directory?addresses="+addr, nil))

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 — a read that blew the explorer budget is retryable "+
			"capacity, not an internal fault", rec.Code)
	}
	if ra := rec.Header().Get("Retry-After"); ra == "" {
		t.Errorf("no Retry-After on the retryable 503; every client is left to guess, and the "+
			"guess is `immediately` (headers %v)", rec.Header())
	}
}
