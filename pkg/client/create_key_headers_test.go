package client_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Stellar-Index/StellarIndex/pkg/client"
)

// createKeyCapture records what one POST /v1/account/keys delivered.
type createKeyCapture struct {
	header http.Header
	body   map[string]any
}

func captureCreateKey(t *testing.T, req client.CreateKeyRequest) createKeyCapture {
	t.Helper()
	var got createKeyCapture
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.header = r.Header.Clone()
		_ = json.NewDecoder(r.Body).Decode(&got.body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"data":{"key_id":"kid_new","label":"ci"},"as_of":"2026-04-28T10:00:00Z","flags":{}}`))
	}))
	t.Cleanup(ts.Close)
	c := client.New(client.Options{BaseURL: ts.URL, APIKey: "rek_caller"})
	if _, err := c.CreateKey(context.Background(), req); err != nil {
		t.Fatalf("CreateKey: %v", err)
	}
	return got
}

// TestCreateKey_SendsIdempotencyKeyHeader: a retried CreateKey is only
// safe if the server can recognise it, so the SDK must carry the
// caller's key on the Idempotency-Key header (and never in the body).
func TestCreateKey_SendsIdempotencyKeyHeader(t *testing.T) {
	got := captureCreateKey(t, client.CreateKeyRequest{Label: "ci", IdempotencyKey: "mint-7f3a"})
	if v := got.header.Get("Idempotency-Key"); v != "mint-7f3a" {
		t.Errorf("server saw Idempotency-Key = %q, want mint-7f3a", v)
	}
	for k := range got.body {
		if k != "label" {
			t.Errorf("unexpected body member %q (header-only fields leaked into JSON): %+v", k, got.body)
		}
	}

	bare := captureCreateKey(t, client.CreateKeyRequest{Label: "ci"})
	if _, set := bare.header["Idempotency-Key"]; set {
		t.Error("Idempotency-Key sent although the request left it empty")
	}
}

// TestCreateKey_SendsReasonHeader: an operator-tier caller of
// POST /v1/account/keys is 400'd without X-Reason, so CreateKey must be
// able to send it — header only, never in the body.
func TestCreateKey_SendsReasonHeader(t *testing.T) {
	got := captureCreateKey(t, client.CreateKeyRequest{Label: "ci", Reason: "rotating staff credential per ticket 88"})
	if v := got.header.Get("X-Reason"); v != "rotating staff credential per ticket 88" {
		t.Errorf("server saw X-Reason = %q, want the request's Reason", v)
	}
	if _, leaked := got.body["reason"]; leaked {
		t.Errorf("reason leaked into the JSON body: %+v", got.body)
	}
	bare := captureCreateKey(t, client.CreateKeyRequest{Label: "ci"})
	if _, set := bare.header["X-Reason"]; set {
		t.Error("X-Reason sent although the request left it empty")
	}
}
