// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package kraken

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/StellarIndex/stellar-index/internal/canonical"
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
