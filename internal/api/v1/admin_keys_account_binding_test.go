// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1_test

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/auth"
	"github.com/Stellar-Index/StellarIndex/internal/platform"
)

// An admin-minted key's identifier is its metering subject
// (middleware.UsageKeyForSubject), so an acct:<slug> identifier draws
// down that account's monthly quota. Such a mint must name the account
// explicitly and the account must exist; nothing is created otherwise.
func TestAdminKeysCreate_AccountBinding(t *testing.T) {
	cases := []struct {
		name   string
		body   string
		status int
	}{
		{"acct identifier without confirmation", `{"identifier":"acct:partner-co","label":"l"}`, http.StatusBadRequest},
		{"confirmation names another account", `{"identifier":"acct:partner-co","account":"ok","label":"l"}`, http.StatusBadRequest},
		{"empty slug", `{"identifier":"acct:","account":"","label":"l"}`, http.StatusBadRequest},
		{"account on a non-account identifier", `{"identifier":"signup-abc","account":"partner-co","label":"l"}`, http.StatusBadRequest},
		{"account does not exist", `{"identifier":"acct:ghost","account":"ghost","label":"l"}`, http.StatusNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := &fakeAccountStore{}
			ts := newAdminTestServer(t, operatorSubject(), store, nil)
			resp := postJSON(t, ts.URL+"/v1/admin/keys", tc.body)
			if resp.StatusCode != tc.status {
				t.Errorf("status = %d, want %d", resp.StatusCode, tc.status)
			}
			if store.calls != 0 {
				t.Errorf("store.Create called %d times; a refused binding must mint nothing", store.calls)
			}
		})
	}
}

func TestAdminKeysCreate_NonAccountIdentifierNeedsNoBinding(t *testing.T) {
	store := &fakeAccountStore{rec: auth.APIKeyRecord{KeyID: "kid_ops"}, plain: "sip_x"}
	ts := newAdminTestServer(t, operatorSubject(), store, nil)
	resp := postJSON(t, ts.URL+"/v1/admin/keys", `{"identifier":"operator:staff-2","label":"l","tier":"operator"}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201", resp.StatusCode)
	}
	if store.gotReq.Identifier != "operator:staff-2" {
		t.Errorf("Identifier = %q", store.gotReq.Identifier)
	}
}

// Without a platform account store the binding cannot be proven, so an
// acct:<slug> mint fails closed.
func TestAdminKeysCreate_AccountBindingFailsClosed(t *testing.T) {
	for name, accounts := range map[string]v1.PlatformAccountStore{
		"no account store": nil,
		"lookup error": &fakePlatformAccountStore{
			byID: map[uuid.UUID]platform.Account{}, getErr: errors.New("pg down"),
		},
	} {
		t.Run(name, func(t *testing.T) {
			store := &fakeAccountStore{}
			srv := v1.New(v1.Options{
				Auth:             fakeAuthMiddleware(operatorSubject()),
				Accounts:         store,
				PlatformAccounts: accounts,
			})
			ts := httptest.NewServer(srv.Handler())
			t.Cleanup(ts.Close)
			resp := postJSON(t, ts.URL+"/v1/admin/keys", `{"identifier":"acct:partner-co","account":"partner-co","label":"l"}`)
			if resp.StatusCode != http.StatusServiceUnavailable {
				t.Errorf("status = %d, want 503", resp.StatusCode)
			}
			if store.calls != 0 {
				t.Errorf("store.Create called %d times without a proven account", store.calls)
			}
		})
	}
}
