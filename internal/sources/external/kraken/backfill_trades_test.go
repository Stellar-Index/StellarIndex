// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package kraken

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// The first page is a REAL frame captured from Kraken's /Trades for
// XLMUSD at since=2018-07-01 (board #44 probe) — the fills that prove
// the deep-history path reaches 2018 where /OHLC returns nothing.
const krakenTradesPage1 = `{"error":[],"result":{"XXLMZUSD":[
["0.19329800","159.80957483",1530403225.7644963,"b","l","",460991],
["0.19329800","0.19042517",1530403225.767804,"b","l","",460992],
["0.19330000","3145.32229695",1530403225.770289,"b","l","",460993]],
"last":"1530403225770289000"}}`

const krakenTradesPage2 = `{"error":[],"result":{"XXLMZUSD":[],"last":"1530403225770289000"}}`

func TestBackfillTrades_DeepHistory(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path != tradesPath {
			t.Errorf("path = %s", r.URL.Path)
		}
		if calls == 1 {
			fmt.Fprint(w, krakenTradesPage1)
			return
		}
		fmt.Fprint(w, krakenTradesPage2)
	}))
	defer srv.Close()

	pair, _ := canonical.NewPair(mustAsset(t, "crypto:XLM"), mustAsset(t, "fiat:USD"))
	s := &Streamer{Endpoint: srv.URL, PairMap: map[string]canonical.Pair{"XXLMZUSD": pair}}

	from := time.Date(2018, 7, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2018, 7, 2, 0, 0, 0, 0, time.UTC)
	trades, err := s.BackfillTrades(context.Background(), pair, from, to)
	if err != nil {
		t.Fatalf("BackfillTrades: %v", err)
	}
	if len(trades) != 3 {
		t.Fatalf("trades = %d, want 3", len(trades))
	}
	tr := trades[0]
	// price 0.19329800 × volume 159.80957483 at 10^8 scale:
	// base = 15980957483, quote = base×price/1e8 = 3089087119.
	if got := tr.BaseAmount.String(); got != "15980957483" {
		t.Errorf("base = %s", got)
	}
	if got := tr.QuoteAmount.String(); got != "3089087119" {
		t.Errorf("quote = %s", got)
	}
	if tr.Timestamp.Year() != 2018 || tr.Source != SourceName || tr.Ledger != 0 {
		t.Errorf("identity: %+v", tr)
	}
	// Idempotency: same fill → same synthetic hash.
	trades2, _ := s.BackfillTrades(context.Background(), pair, from, to)
	if len(trades2) == 3 && trades2[0].TxHash != tr.TxHash {
		t.Error("synthetic tx hash not deterministic across runs")
	}
}

func mustAsset(t *testing.T, id string) canonical.Asset {
	t.Helper()
	a, err := canonical.ParseAsset(id)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// TestBackfillTrades_UnresponsiveVenueIsBounded pins the per-request
// deadline on the /Trades pagination loop (#371 F5).
//
// The loop only checks ctx BETWEEN pages, and `stellarindex-ops
// backfill` hands it the process root context — which has no deadline.
// So the ONLY bound on a venue that accepts the connection and then
// never writes a response is the HTTP client's own Timeout. With
// http.DefaultClient (Timeout: 0) there is none: the backfill wedges
// until the operator kills it, holding its DB handles and its slot in
// the run.
//
// The server here black-holes every request rather than returning an
// error, because that is the failure a client deadline catches and a
// connection-refused does not.
func TestBackfillTrades_UnresponsiveVenueIsBounded(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		<-release // never responds
	}))
	defer func() {
		close(release)
		srv.Close()
	}()

	// Drives the REAL production path (BackfillTrades → fetchKrakenTrades
	// → the package REST client); only the deadline is shortened, so the
	// suite does not wait the production 30 s.
	restore := krakenRESTTimeout
	krakenRESTTimeout = 150 * time.Millisecond
	t.Cleanup(func() { krakenRESTTimeout = restore })

	pair, _ := canonical.NewPair(mustAsset(t, "crypto:XLM"), mustAsset(t, "fiat:USD"))
	s := &Streamer{Endpoint: srv.URL, PairMap: map[string]canonical.Pair{"XXLMZUSD": pair}}

	from := time.Date(2018, 7, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2018, 7, 2, 0, 0, 0, 0, time.UTC)

	done := make(chan error, 1)
	go func() {
		_, err := s.BackfillTrades(context.Background(), pair, from, to)
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("BackfillTrades returned nil against a venue that never responds; want a timeout error")
		}
		var nerr net.Error
		if !errors.As(err, &nerr) || !nerr.Timeout() {
			t.Fatalf("BackfillTrades error = %v; want a net.Error reporting Timeout() — the bound must come from the client deadline, not from an unrelated failure", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("BackfillTrades did not return within 5 s against a venue that never responds — the /Trades GET is unbounded (http.DefaultClient has no Timeout), so one black-holed connection wedges the whole backfill")
	}
}
