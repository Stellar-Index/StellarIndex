// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package chainlink

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// redirectKeyPath stands in for the key-bearing path of a keyed RPC
// endpoint. Deliberately an obviously-fake, low-entropy string.
const redirectKeyPath = "/v2/not-a-real-key-redirect" // gitleaks:allow

// redirectingRPC returns an RPC endpoint that answers every call with
// a 307 to a second server, and a counter of requests that reached it.
func redirectingRPC(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var hops atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hops.Add(1)
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":"0x10"}`))
	}))
	t.Cleanup(target.Close)
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/rpc", http.StatusTemporaryRedirect)
	}))
	t.Cleanup(origin.Close)
	return origin, &hops
}

func TestClient_RefusesRPCRedirect(t *testing.T) {
	origin, hops := redirectingRPC(t)
	cases := map[string]*http.Client{
		"default client":  nil,
		"injected client": origin.Client(),
	}
	for name, hc := range cases {
		t.Run(name, func(t *testing.T) {
			hops.Store(0)
			c := NewClient(origin.URL+redirectKeyPath, hc)
			_, err := c.EthBlockNumber(context.Background())
			if n := hops.Load(); n != 0 {
				t.Fatalf("redirect target received %d request(s); want 0", n)
			}
			if err == nil || !strings.Contains(err.Error(), ErrRedirectRefused.Error()) {
				t.Fatalf("EthBlockNumber err = %v; want %q", err, ErrRedirectRefused)
			}
			if strings.Contains(err.Error(), redirectKeyPath) {
				t.Errorf("refusal error leaks the keyed endpoint path: %s", err)
			}
		})
	}
}

func TestDoWithoutRedirects_LeavesCallerClientUntouched(t *testing.T) {
	origin, _ := redirectingRPC(t)
	hc := origin.Client()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, origin.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp, err := DoWithoutRedirects(hc, req); err == nil {
		_ = resp.Body.Close()
		t.Fatal("DoWithoutRedirects followed or accepted a 307; want an error")
	}
	if hc.CheckRedirect != nil {
		t.Fatal("DoWithoutRedirects mutated the caller's client")
	}
}
