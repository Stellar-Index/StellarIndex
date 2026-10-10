// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// priceAtReadFailStub fails reads with err and serves every other read
// from a fresh bucket. failPair, when set, limits the failure to that
// "<base>/<quote>" orientation; failOlderThan, when set, limits it to
// instants at least that far in the past (one /v1/price/changes horizon).
type priceAtReadFailStub struct {
	err           error
	failPair      string
	failOlderThan time.Duration
}

func (s priceAtReadFailStub) PriceAt(
	_ context.Context, pair canonical.Pair, ts time.Time, _ time.Duration,
) (string, time.Time, int, error) {
	fails := s.failPair == "" || pair.Base.String()+"/"+pair.Quote.String() == s.failPair
	if s.failOlderThan > 0 && time.Since(ts) < s.failOlderThan {
		fails = false
	}
	if fails {
		return "", time.Time{}, 0, s.err
	}
	return "0.1", ts.Add(-time.Minute), 60, nil
}

func assertReadFailureProblem(t *testing.T, rec *httptest.ResponseRecorder, wantStatus int, wantType string) {
	t.Helper()
	body := rec.Body.String()
	if rec.Code != wantStatus {
		t.Fatalf("status %d, want %d: %s", rec.Code, wantStatus, body)
	}
	if !strings.Contains(body, `"type":"https://api.stellarindex.io/errors/`+wantType+`"`) {
		t.Errorf("body does not carry errors/%s: %s", wantType, body)
	}
	if strings.Contains(body, "price-not-found") || strings.Contains(body, `"available":false`) {
		t.Errorf("a failed read was reported as an absence of data: %s", body)
	}
}
