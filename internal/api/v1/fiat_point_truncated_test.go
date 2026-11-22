// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1_test

import (
	"context"
	"math/big"
	"net/http"
	"testing"
	"time"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// cappedConstituentReader honours TradesInRange's limit the way the store does: the newest `limit` rows, oldest first.
type cappedConstituentReader struct {
	*fiatConstituentReader
}

func (r cappedConstituentReader) TradesInRange(
	ctx context.Context, pair canonical.Pair, from, to time.Time, limit int,
) ([]canonical.Trade, error) {
	out, err := r.fiatConstituentReader.TradesInRange(ctx, pair, from, to, limit)
	if limit > 0 && len(out) > limit {
		out = out[len(out)-limit:]
	}
	return out, err
}

// A held-back constituent read cut at the cap leaves trades unread even after the bucket gate thins it below the cap, so every
// fiat-quoted point surface must still say truncated.
func TestFiatPoint_CappedHeldBackReadIsTruncated(t *testing.T) {
	_ = installUSDCSACRegistry(t)
	aqua := mustParseAsset(t, aquaClassicID)
	usdcClassic := mustParseAsset(t, usdcClassicID)
	usdcSAC := mustParseAsset(t, pegAliasUSDCSAC)
	bookPair, err := canonical.NewPair(aqua, usdcClassic)
	if err != nil {
		t.Fatalf("NewPair book: %v", err)
	}
	poolPair, err := canonical.NewPair(aqua, usdcSAC)
	if err != nil {
		t.Fatalf("NewPair pool: %v", err)
	}
	t0 := fiatParityBucketStart()
	const poolN = 10500 // above the point path's 10000-trade cap
	pool := make([]canonical.Trade, 0, poolN)
	for i := range poolN {
		ts := t0.Add(time.Duration(i) * time.Hour / poolN)
		pool = append(pool, fiatParityTrade(poolPair, uint32(10+i), ts, 1000_0000000, 200_0000000))
	}
	// One floored book print in the pool's newest minute: the gate drops that minute's pool rows.
	bookTrade := fiatParityTrade(bookPair, 1, t0.Add(59*time.Minute+30*time.Second), 1000_0000000, 200_0000000)
	bookTrade.ClearsNotionalFloor = true
	reader := cappedConstituentReader{&fiatConstituentReader{tradesByPair: map[string][]canonical.Trade{
		fiatParityPairKey(bookPair): {bookTrade},
		fiatParityPairKey(poolPair): pool,
	}}}
	ts := httpTestServer(t, v1.New(v1.Options{
		History:           reader,
		USDPeggedClassics: []canonical.Asset{usdcClassic},
	}))
	win := "&from=" + t0.Format(time.RFC3339) + "&to=" + t0.Add(time.Hour).Format(time.RFC3339)

	for _, path := range []string{"/v1/ohlc", "/v1/vwap", "/v1/twap"} {
		resp := mustGet(t, ts.URL+path+"?base="+aquaClassicID+"&quote=fiat:USD&outlier_sigma=0"+win)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s status = %d, want 200", path, resp.StatusCode)
		}
		var env struct {
			Data struct {
				Truncated bool `json:"truncated"`
			} `json:"data"`
		}
		mustDecode(t, resp, &env)
		if !env.Data.Truncated {
			t.Errorf("%s truncated = false, want true: the pool read hit the cap and older trades were never read", path)
		}
	}
}

// A capped held-back read cuts the other books only where its unread rows could enter: every minute of its unread span that the
// established book answered is gated anyway, so when all of them are answered nothing is cut and the book is served whole.
func TestFiatPoint_CappedHeldBackReadCutsOnlyWhereItsUnreadRowsCouldEnter(t *testing.T) {
	_ = installUSDCSACRegistry(t)
	aqua := mustParseAsset(t, aquaClassicID)
	usdcClassic := mustParseAsset(t, usdcClassicID)
	bookPair, err := canonical.NewPair(aqua, usdcClassic)
	if err != nil {
		t.Fatalf("NewPair book: %v", err)
	}
	poolPair, err := canonical.NewPair(aqua, mustParseAsset(t, pegAliasUSDCSAC))
	if err != nil {
		t.Fatalf("NewPair pool: %v", err)
	}
	t0 := fiatParityBucketStart()
	// The book prints once a minute: 0.9 for minutes 0-49, 0.2 for 50-59. The pool's 10500 trades all fall in minutes 50-59.
	book := make([]canonical.Trade, 0, 60)
	for m := range 60 {
		quote := int64(900_0000000)
		if m >= 50 {
			quote = 200_0000000
		}
		tr := fiatParityTrade(bookPair, uint32(1+m), t0.Add(time.Duration(m)*time.Minute+30*time.Second), 1000_0000000, quote)
		tr.ClearsNotionalFloor = true
		book = append(book, tr)
	}
	const poolN = 10500
	pool := make([]canonical.Trade, 0, poolN)
	for i := range poolN {
		ts := t0.Add(50*time.Minute + time.Duration(i)*10*time.Minute/poolN)
		pool = append(pool, fiatParityTrade(poolPair, uint32(100+i), ts, 1000_0000000, 200_0000000))
	}
	reader := cappedConstituentReader{&fiatConstituentReader{tradesByPair: map[string][]canonical.Trade{
		fiatParityPairKey(bookPair): book,
		fiatParityPairKey(poolPair): pool,
	}}}
	ts := httpTestServer(t, v1.New(v1.Options{History: reader, USDPeggedClassics: []canonical.Asset{usdcClassic}}))
	win := "?base=" + aquaClassicID + "&quote=fiat:USD&outlier_sigma=0&from=" + t0.Format(time.RFC3339) + "&to=" + t0.Add(time.Hour).Format(time.RFC3339)
	for path, want := range map[string]map[string]*big.Rat{
		"/v1/ohlc": {"open": big.NewRat(9, 10), "high": big.NewRat(9, 10), "low": big.NewRat(1, 5)},
		"/v1/vwap": {"price": big.NewRat(47, 60)}, // (50*0.9 + 10*0.2) / 60
	} {
		resp := mustGet(t, ts.URL+path+win)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s status = %d, want 200", path, resp.StatusCode)
		}
		var env struct {
			Data map[string]any `json:"data"`
		}
		mustDecode(t, resp, &env)
		if env.Data["truncated"] != true {
			t.Errorf("%s truncated = %v, want true: the pool read hit the cap", path, env.Data["truncated"])
		}
		for field, w := range want {
			got, _ := env.Data[field].(string)
			if mustRat(t, got).Cmp(mustRat(t, w.FloatString(10))) != 0 { // prices are served at 10 dp
				t.Errorf("%s %s = %s, want %s: the book's fully read trades were cut to the gated pool's span", path, field, got, w.FloatString(10))
			}
		}
	}
}

// When a capped held-back read's unread span holds a minute no established book answered, its unread rows could enter there, so
// the merged answer must not keep another book's trades from before that read's earliest row.
func TestFiatPoint_CappedHeldBackReadCutsBooksWhereItsUnreadRowsCouldEnter(t *testing.T) {
	_ = installUSDCSACRegistry(t)
	aqua := mustParseAsset(t, aquaClassicID)
	usdcClassic := mustParseAsset(t, usdcClassicID)
	usdcSAC := mustParseAsset(t, pegAliasUSDCSAC)
	bookPair, err := canonical.NewPair(aqua, usdcClassic)
	if err != nil {
		t.Fatalf("NewPair book: %v", err)
	}
	poolPair, err := canonical.NewPair(aqua, usdcSAC)
	if err != nil {
		t.Fatalf("NewPair pool: %v", err)
	}
	t0 := fiatParityBucketStart()
	const poolN = 10500 // the newest 10000 are read, so the pool is read only from ~t0+2m51s
	pool := make([]canonical.Trade, 0, poolN)
	for i := range poolN {
		ts := t0.Add(time.Duration(i) * time.Hour / poolN)
		pool = append(pool, fiatParityTrade(poolPair, uint32(10+i), ts, 1000_0000000, 200_0000000))
	}
	// The book's old print (price 0.9) predates every pool row read; its new one matches the pool's price.
	oldBook := fiatParityTrade(bookPair, 1, t0.Add(time.Minute), 1000_0000000, 900_0000000)
	newBook := fiatParityTrade(bookPair, 2, t0.Add(59*time.Minute+30*time.Second), 1000_0000000, 200_0000000)
	oldBook.ClearsNotionalFloor, newBook.ClearsNotionalFloor = true, true
	reader := cappedConstituentReader{&fiatConstituentReader{tradesByPair: map[string][]canonical.Trade{
		fiatParityPairKey(bookPair): {oldBook, newBook},
		fiatParityPairKey(poolPair): pool,
	}}}
	ts := httpTestServer(t, v1.New(v1.Options{
		History:           reader,
		USDPeggedClassics: []canonical.Asset{usdcClassic},
	}))
	win := "?base=" + aquaClassicID + "&quote=fiat:USD&outlier_sigma=0&from=" + t0.Format(time.RFC3339) + "&to=" + t0.Add(time.Hour).Format(time.RFC3339)
	for path, field := range map[string]string{"/v1/ohlc": "high", "/v1/vwap": "price"} {
		resp := mustGet(t, ts.URL+path+win)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s status = %d, want 200", path, resp.StatusCode)
		}
		var env struct {
			Data map[string]any `json:"data"`
		}
		mustDecode(t, resp, &env)
		if env.Data["truncated"] != true {
			t.Errorf("%s truncated = %v, want true", path, env.Data["truncated"])
		}
		got, _ := env.Data[field].(string)
		if mustRat(t, got).Cmp(big.NewRat(1, 5)) != 0 {
			t.Errorf("%s %s = %s, want 0.2: a book's trade older than the capped read's span leaked into the answer", path, field, got)
		}
	}
}
