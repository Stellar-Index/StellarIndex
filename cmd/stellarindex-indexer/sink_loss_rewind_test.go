// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/pipeline"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// fakeRewinder records RewindCursor calls against a scripted cursor.
type fakeRewinder struct {
	fakeCursorStore
	rewindErr  error
	rewinds    int
	rewoundTo  uint32
	rewoundSrc string
	rewoundSub string
}

func (f *fakeRewinder) RewindCursor(_ context.Context, source, sub string, last uint32) (uint32, error) {
	f.rewinds++
	f.rewoundSrc, f.rewoundSub, f.rewoundTo = source, sub, last
	return f.cursor.LastLedger, f.rewindErr
}

func TestRewindCursorForSinkLoss(t *testing.T) {
	t.Parallel()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	cases := []struct {
		name            string
		cursor          uint32
		getErr          error
		rewindErr       error
		loss            pipeline.ShutdownLoss
		producerStopped bool
		wantCovered     bool
		wantRewinds     int
		wantTo          uint32
	}{
		{name: "abandoned on-chain trades rewind below the lowest", cursor: 1250, loss: pipeline.ShutdownLoss{Rows: 3, MinLedger: 1200}, producerStopped: true, wantCovered: true, wantRewinds: 1, wantTo: 1199},
		{name: "an abandoned event below every trade sets the target", cursor: 1250, loss: pipeline.ShutdownLoss{Rows: 3, MinLedger: 1100}, producerStopped: true, wantCovered: true, wantRewinds: 1, wantTo: 1099},
		{name: "mixed loss still rewinds for the on-chain part", cursor: 1250, loss: pipeline.ShutdownLoss{Rows: 4, MinLedger: 1200, LedgerUnknown: true}, producerStopped: true, wantRewinds: 1, wantTo: 1199},
		{name: "no loss", cursor: 1250, producerStopped: true, wantCovered: true},
		{name: "ledger-less loss only", cursor: 1250, loss: pipeline.ShutdownLoss{Rows: 2, LedgerUnknown: true}, producerStopped: true},
		{name: "producer still running", cursor: 1250, loss: pipeline.ShutdownLoss{Rows: 3, MinLedger: 1200}},
		{name: "cursor never passed the loss", cursor: 1199, loss: pipeline.ShutdownLoss{Rows: 3, MinLedger: 1200}, producerStopped: true, wantCovered: true},
		{name: "cursor never passed the loss, ledger-less rows too", cursor: 1199, loss: pipeline.ShutdownLoss{Rows: 3, MinLedger: 1200, LedgerUnknown: true}, producerStopped: true},
		{name: "cursor read fails", getErr: errors.New("conn refused"), loss: pipeline.ShutdownLoss{Rows: 3, MinLedger: 1200}, producerStopped: true},
		{name: "rewind fails, not fatal", cursor: 1250, rewindErr: errors.New("conn refused"), loss: pipeline.ShutdownLoss{Rows: 3, MinLedger: 1200}, producerStopped: true, wantRewinds: 1, wantTo: 1199},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := &fakeRewinder{
				fakeCursorStore: fakeCursorStore{cursor: timescale.Cursor{LastLedger: tc.cursor}, err: tc.getErr},
				rewindErr:       tc.rewindErr,
			}
			got := rewindCursorForSinkLoss(context.Background(), f, tc.loss, tc.producerStopped, logger)
			if got != tc.wantCovered {
				t.Errorf("loss covered = %v, want %v", got, tc.wantCovered)
			}
			if f.rewinds != tc.wantRewinds {
				t.Fatalf("RewindCursor calls = %d, want %d", f.rewinds, tc.wantRewinds)
			}
			if tc.wantRewinds > 0 && (f.rewoundTo != tc.wantTo || f.rewoundSrc != cursorSource || f.rewoundSub != "") {
				t.Errorf("RewindCursor(%q, %q, %d), want (%q, \"\", %d)", f.rewoundSrc, f.rewoundSub, f.rewoundTo, cursorSource, tc.wantTo)
			}
		})
	}
}

// After a loss the next start will not re-walk, /metrics must outlive one scrape; with nothing lost it stops at once.
func TestStopMetricsServerLingersOnlyOnAnUncoveredLoss(t *testing.T) {
	t.Parallel()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	start := func() (*http.Server, string) {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }), ReadHeaderTimeout: time.Second}
		go func() { _ = srv.Serve(ln) }()
		return srv, "http://" + ln.Addr().String()
	}
	scrape := func(url string) error {
		resp, err := (&http.Client{Timeout: time.Second}).Get(url)
		if err == nil {
			_ = resp.Body.Close()
		}
		return err
	}

	srv, url := start()
	stopped := make(chan struct{})
	go func() { stopMetricsServer(srv, false, 500*time.Millisecond, logger); close(stopped) }()
	time.Sleep(100 * time.Millisecond)
	if err := scrape(url); err != nil {
		t.Errorf("scrape during the linger failed: %v", err)
	}
	<-stopped
	if err := scrape(url); err == nil {
		t.Error("server still answers after stopMetricsServer returned")
	}

	srv, url = start()
	began := time.Now()
	stopMetricsServer(srv, true, time.Hour, logger)
	if d := time.Since(began); d > 10*time.Second {
		t.Errorf("a clean shutdown waited %s on the metrics server", d)
	}
	if err := scrape(url); err == nil {
		t.Error("server still answers after a clean stop")
	}
	stopMetricsServer(nil, false, time.Hour, logger)
}
