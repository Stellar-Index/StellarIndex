// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package chainlink

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A JSON-RPC body over maxRPCBodyBytes is refused, not decoded: the pad
// keeps the JSON valid, so only the cap can reject it.
func TestClientDo_OversizedBodyRefused(t *testing.T) {
	pad := strings.Repeat("a", maxRPCBodyBytes)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":1,"result":"0x1","pad":"%s"}`, pad)
	}))
	t.Cleanup(srv.Close)

	_, err := NewClient(srv.URL, srv.Client()).EthBlockNumber(context.Background())
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("oversized body: err = %v, want a body-cap refusal", err)
	}
}
