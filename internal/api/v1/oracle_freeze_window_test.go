// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1_test

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

type freezeWindowPriceReader struct {
	*stubPriceReader
	windows []timescale.FreezeWindow
	err     error
	asked   []string
}

func (r *freezeWindowPriceReader) FreezeWindows(_ context.Context, assets []canonical.Asset, quote canonical.Asset, _ time.Time) ([]timescale.FreezeWindow, error) {
	for _, a := range assets {
		r.asked = append(r.asked, a.String()+"/"+quote.String())
	}
	return r.windows, r.err
}

// A freeze that has recovered still refused the buckets it covered: prices() must not serve them afterwards.
func recoveredFreezeServer(t *testing.T, err error) (*freezeWindowPriceReader, string) {
	t.Helper()
	b0 := time.Date(2026, 3, 4, 12, 0, 0, 0, time.UTC)
	bucket := func(start time.Time, price string) v1.PriceSnapshot {
		return v1.PriceSnapshot{
			AssetID: "crypto:XLM", Quote: "fiat:USD", Price: price, PriceType: "vwap",
			WindowSeconds: 60, ObservedAt: v1.WireTime(start.Add(time.Minute)),
		}
	}
	recovered := b0.Add(25 * time.Minute)
	reader := &freezeWindowPriceReader{
		stubPriceReader: &stubPriceReader{recent: map[string][]v1.PriceSnapshot{xlmUSD: {
			bucket(b0.Add(30*time.Minute), "0.1100000"),
			bucket(b0.Add(10*time.Minute), "0.9900000"),
			bucket(b0, "0.1000000"),
		}}},
		windows: []timescale.FreezeWindow{{FrozenAt: b0.Add(12 * time.Minute), RecoveredAt: &recovered}},
		err:     err,
	}
	srv := v1.New(v1.Options{Prices: reader, Freeze: frozenPairs{}})
	ts := startHTTPTest(t, srv.Handler())
	return reader, ts.URL + "/v1/oracle/prices?asset=crypto:XLM&records=3"
}

func TestOraclePrices_RecoveredFreezeBucketsAreNotServed(t *testing.T) {
	reader, url := recoveredFreezeServer(t, nil)
	resp, err := http.Get(url) //nolint:noctx // test server
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	status, body := getBody(t, url)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", status, body)
	}
	if strings.Contains(body, "0.9900000") {
		t.Fatalf("served the bucket a recorded freeze refused: %s", body)
	}
	for _, want := range []string{`"price":"0.1100000"`, `"price":"0.1000000"`} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %s: %s", want, body)
		}
	}
	if cc := resp.Header.Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store for a partial series", cc)
	}
	if !strings.Contains(strings.Join(reader.asked, ","), "crypto:XLM/fiat:USD") {
		t.Errorf("freeze windows not read for the served pair: %v", reader.asked)
	}
}

func TestOraclePrices_FreezeWindowReadFailureIsStaleNotClean(t *testing.T) {
	_, url := recoveredFreezeServer(t, errors.New("pq: connection reset"))
	resp, err := http.Get(url) //nolint:noctx // test server
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if cc := resp.Header.Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store when the freeze windows were not read", cc)
	}
	status, body := getBody(t, url)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", status, body)
	}
	if !strings.Contains(body, `"stale":true`) || strings.Contains(body, `"frozen_checked":true`) {
		t.Errorf("an unread freeze history must read stale and not frozen-checked: %s", body)
	}
}
