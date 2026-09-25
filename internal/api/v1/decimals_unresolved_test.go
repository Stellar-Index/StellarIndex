// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1_test

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// A FAILED decimals() read (error or deadline) is not "no declaration": the
// scale is unknown, and every surface below publishes a figure scaled by it.
// Pre-fix each one silently served the default 7 as if it had been read.

var errDecimalsRead = errors.New("clickhouse: context deadline exceeded")

type countingDecStub struct {
	calls atomic.Int32
	d     uint32
	found bool
	err   error
}

func (s *countingDecStub) TokenDecimals(_ context.Context, _ string) (uint32, bool, error) {
	s.calls.Add(1)
	return s.d, s.found, s.err
}

type oneRowSEP41Transfers struct{}

func (oneRowSEP41Transfers) ListSEP41Transfers(
	_ context.Context, _, _, _ string, _ int,
) ([]timescale.SEP41TransferRow, error) {
	return []timescale.SEP41TransferRow{{Kind: "transfer"}}, nil
}

// /v1/assets/{id}: no projection row and a failed lake read → the cap and
// FDV (supply ÷ 10^decimals × price) are refused, the body is flagged
// stale, and it is NOT cached — the next request reads again.
func TestAssetDetail_DecimalsReadFailed_RefusesCapStaleUncached(t *testing.T) {
	dec := &countingDecStub{err: errDecimalsRead}
	srv := v1.New(v1.Options{
		Prices:        lockstepPrices(),
		Supply:        lockstepSupply(),
		TokenDecimals: dec,
	})
	body := lockstepGet(t, srv)

	for _, field := range []string{`"market_cap_usd":"`, `"fdv_usd":"`} {
		if strings.Contains(body, field) {
			t.Errorf("%s published on a guessed scale of 7: %s", field, body)
		}
	}
	if !strings.Contains(body, `"stale":true`) {
		t.Errorf("flags.stale must be set on an unresolved scale: %s", body)
	}
	first := dec.calls.Load()
	_ = lockstepGet(t, srv)
	if dec.calls.Load() == first {
		t.Error("degraded body was cached: the second request never re-read decimals")
	}
}

// /v1/contracts/{id}/transfers: `decimals` is the conversion factor for
// every amount, so a failed read is a retryable 503, not decimals: 7. An
// undeclared scale (found=false) keeps the documented default.
func TestSEP41Transfers_DecimalsReadFailed_Returns503(t *testing.T) {
	for _, tc := range []struct {
		name     string
		dec      *countingDecStub
		status   int
		contains string
	}{
		{"read failed", &countingDecStub{err: errDecimalsRead}, http.StatusServiceUnavailable, "decimals"},
		{"not declared", &countingDecStub{}, http.StatusOK, `"decimals":7`},
		{"read", &countingDecStub{d: 18, found: true}, http.StatusOK, `"decimals":18`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := v1.New(v1.Options{SEP41Transfers: oneRowSEP41Transfers{}, TokenDecimals: tc.dec})
			ts := httpTestServer(t, srv)
			resp := mustGet(t, ts.URL+"/v1/contracts/"+decTestContract+"/transfers")
			body, _ := readAll(resp)
			if resp.StatusCode != tc.status || !strings.Contains(body, tc.contains) {
				t.Errorf("status = %d, want %d; body must contain %s: %s",
					resp.StatusCode, tc.status, tc.contains, body)
			}
		})
	}
}

// /v1/history: base_decimals is stamped on every row from the same read.
func TestHistory_DecimalsReadFailed_Returns503(t *testing.T) {
	pair, err := canonical.NewPair(mustParseAsset(t, decTestContract), mustParseAsset(t, "native"))
	if err != nil {
		t.Fatalf("NewPair: %v", err)
	}
	tr := mkHistTrade(100)
	tr.Pair = pair
	reader := &stubHistoryReader{trades: []canonical.Trade{tr}}
	srv := v1.New(v1.Options{History: reader, TokenDecimals: &countingDecStub{err: errDecimalsRead}})
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/history?base="+decTestContract+"&quote=native&limit=50")
	body, _ := readAll(resp)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503 rather than base_decimals 7 on a failed read: %s",
			resp.StatusCode, body)
	}
}
