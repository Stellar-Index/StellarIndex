// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package httpx

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/httpx/httpxtest"
)

func TestKeyedSameOriginRedirect_Policy(t *testing.T) {
	check := NewKeyedClient("t", time.Second).CheckRedirect
	if check == nil {
		t.Fatal("CheckRedirect is nil; the key would follow any redirect")
	}
	const origin = "https://api.example.test/x"
	cases := []struct {
		name    string
		target  string
		hops    int
		wantErr string
	}{
		{"https to http downgrade", "http://api.example.test/x", 1, "refusing to follow a redirect"},
		{"port change", "https://api.example.test:8443/x", 1, "refusing to follow a redirect"},
		{"host change", "https://elsewhere.example.test/x", 1, "refusing to follow a redirect"},
		{"same origin at the cap", origin, 10, "stopped after 10 redirects"},
		{"same origin under the cap", origin, 9, ""},
	}
	for _, tc := range cases {
		req, err := http.NewRequest(http.MethodGet, tc.target, nil)
		if err != nil {
			t.Fatal(err)
		}
		via := make([]*http.Request, tc.hops)
		for i := range via {
			if via[i], err = http.NewRequest(http.MethodGet, origin, nil); err != nil {
				t.Fatal(err)
			}
		}
		err = check(req, via)
		switch {
		case tc.wantErr == "" && err != nil:
			t.Errorf("%s: err = %v, want the hop followed", tc.name, err)
		case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
			t.Errorf("%s: err = %v, want %q", tc.name, err, tc.wantErr)
		}
	}
}

func TestNewKeyedClient_KeepsAuthorizationOnOrigin(t *testing.T) {
	trap := httpxtest.NewRedirectTrap(t, "Authorization")
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, trap.URL+"/v1/x", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer probe")
	resp, err := NewKeyedClient("t", 5*time.Second).Do(req)
	if err == nil {
		_ = resp.Body.Close()
		t.Fatal("an off-origin redirect was followed")
	}
	trap.AssertKeyStayedOnOrigin(t)
}
