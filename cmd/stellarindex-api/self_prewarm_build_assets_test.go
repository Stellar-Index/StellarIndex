// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sort"
	"sync"
	"testing"
)

func TestWarmExplorerBuildAssets_WarmsEveryListingRow(t *testing.T) {
	var (
		mu  sync.Mutex
		got []string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.RequestURI() == explorerBuildListingPath {
			_, _ = w.Write([]byte(`{"data":[{"asset_id":"native"},{"asset_id":"USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"},{"asset_id":""}]}`))
			return
		}
		mu.Lock()
		got = append(got, r.URL.RequestURI())
		mu.Unlock()
	}))
	defer srv.Close()

	get := func(path string) *http.Response {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		return resp
	}
	warm := func(path string) { _ = get(path).Body.Close() }
	warmExplorerBuildAssets(context.Background(), get, warm)

	sort.Strings(got)
	want := []string{"/v1/assets/USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN", "/v1/assets/native"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("warmed %v, want %v", got, want)
	}
}

func TestWarmExplorerBuildAssets_SkipsOnListingError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.RequestURI() != explorerBuildListingPath {
			t.Errorf("unexpected warm of %s after a failed listing", r.URL.RequestURI())
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	get := func(path string) *http.Response {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		return resp
	}
	warmExplorerBuildAssets(context.Background(), get, func(string) { t.Error("warm called") })
}
