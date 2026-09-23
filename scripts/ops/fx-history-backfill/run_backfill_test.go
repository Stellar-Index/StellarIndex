package main

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/sources/external/frankfurter"
)

// frankfurterStub serves a one-day, two-currency range response for every
// request whose path does not start with failPrefix, and a 502 otherwise.
func frankfurterStub(t *testing.T, failPrefix string) *frankfurter.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if failPrefix != "" && strings.HasPrefix(r.URL.Path, failPrefix) {
			http.Error(w, "upstream down", http.StatusBadGateway)
			return
		}
		day := strings.SplitN(strings.TrimPrefix(r.URL.Path, "/"), "..", 2)[0]
		_, _ = io.WriteString(w, `{"base":"USD","rates":{"`+day+`":{"EUR":0.9,"GBP":0.8}}}`)
	}))
	t.Cleanup(srv.Close)
	return frankfurter.NewClient().WithBase(srv.URL)
}

// twoChunkConfig splits 2000-01-01..2010-01-01 into two 5-year chunks whose
// request paths start with /2000 and /2005 respectively.
func twoChunkConfig() backfillConfig {
	return backfillConfig{
		from:         time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC),
		to:           time.Date(2010, 1, 1, 0, 0, 0, 0, time.UTC),
		chunkYears:   5,
		dryRun:       true,
		tickerFilter: map[string]struct{}{},
	}
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// A failed chunk leaves a hole in fx_quotes history; the run must count it
// and exit non-zero instead of logging a warning and reporting done.
func TestRunBackfill_FailedChunkExitsNonZero(t *testing.T) {
	res := runBackfill(context.Background(), discardLogger(), frankfurterStub(t, "/2005"), nil, twoChunkConfig())

	if res.chunks != 2 {
		t.Fatalf("chunks=%d, want 2", res.chunks)
	}
	if res.failedChunks != 1 {
		t.Fatalf("failedChunks=%d, want 1", res.failedChunks)
	}
	if res.totalRows != 2 {
		t.Errorf("totalRows=%d, want 2 (only the surviving chunk's rows)", res.totalRows)
	}
	if code := res.exitCode(); code != 1 {
		t.Errorf("exitCode()=%d, want 1 for a run with a failed chunk", code)
	}
	if code := res.report(discardLogger()); code != 1 {
		t.Errorf("report()=%d, want 1 for a run with a failed chunk", code)
	}
}

// An interrupted run covered only part of the window and must not exit 0.
func TestRunBackfill_InterruptedExitsNonZero(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	res := runBackfill(ctx, discardLogger(), frankfurterStub(t, ""), nil, twoChunkConfig())

	if !res.interrupted {
		t.Fatal("interrupted=false for a cancelled context, want true")
	}
	if res.chunks != 0 {
		t.Errorf("chunks=%d, want 0", res.chunks)
	}
	if code := res.exitCode(); code != 1 {
		t.Errorf("exitCode()=%d, want 1 for an interrupted run", code)
	}
}

func TestRunBackfill_CompleteRunExitsZero(t *testing.T) {
	res := runBackfill(context.Background(), discardLogger(), frankfurterStub(t, ""), nil, twoChunkConfig())

	if res.chunks != 2 || res.failedChunks != 0 || res.interrupted {
		t.Fatalf("got chunks=%d failed=%d interrupted=%v, want 2/0/false",
			res.chunks, res.failedChunks, res.interrupted)
	}
	if res.totalRows != 4 {
		t.Errorf("totalRows=%d, want 4", res.totalRows)
	}
	if code := res.report(discardLogger()); code != 0 {
		t.Errorf("report()=%d, want 0 for a complete run", code)
	}
}
