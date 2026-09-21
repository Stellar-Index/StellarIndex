package client_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Stellar-Index/StellarIndex/pkg/client"
)

// TestAdminCreateKey_SendsReasonHeader — F148. The server's POST
// /v1/admin/keys (internal/api/v1/admin_keys.go, handleAdminKeysCreate)
// requires an `X-Reason` header and 400s without one — every admin
// write captures a reason into the audit log. Pre-fix,
// AdminCreateKeyRequest carried no Reason field and doJSON had no way
// to attach an extra header, so [client.Client.AdminCreateKey] could
// never satisfy the server's contract: every real call 400'd.
func TestAdminCreateKey_SendsReasonHeader(t *testing.T) {
	var gotReason string
	var gotBody map[string]any
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotReason = r.Header.Get("X-Reason")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"data": {"key_id":"kid_new","plaintext":"rek_freshly_minted","label":"staff-rotation"},
			"as_of": "2026-04-28T10:00:00Z",
			"flags": {}
		}`))
	}))
	t.Cleanup(ts.Close)

	c := client.New(client.Options{BaseURL: ts.URL, APIKey: "rek_operator"})
	env, err := c.AdminCreateKey(context.Background(), client.AdminCreateKeyRequest{
		Identifier: "acct:some-customer",
		Label:      "staff-rotation",
		Reason:     "customer opened ticket #4821, rotating a leaked key",
	})
	if err != nil {
		t.Fatalf("AdminCreateKey: %v", err)
	}
	if env.Data.Plaintext != "rek_freshly_minted" {
		t.Errorf("Plaintext = %q", env.Data.Plaintext)
	}
	if gotReason != "customer opened ticket #4821, rotating a leaked key" {
		t.Errorf("server saw X-Reason = %q, want the request's Reason", gotReason)
	}
	// Reason must NOT also appear in the JSON body — the server reads
	// it from the header only.
	if _, ok := gotBody["reason"]; ok {
		t.Errorf("reason leaked into the JSON body: %+v", gotBody)
	}
}

// TestAdminCreateKey_ReasonRequired — client-side validation catches
// a missing Reason without sending the request, same posture as the
// existing Identifier / Label checks.
func TestAdminCreateKey_ReasonRequired(t *testing.T) {
	c := client.New(client.Options{})
	_, err := c.AdminCreateKey(context.Background(), client.AdminCreateKeyRequest{
		Identifier: "acct:some-customer",
		Label:      "staff-rotation",
	})
	if err == nil {
		t.Fatal("expected error for empty reason")
	}
}
