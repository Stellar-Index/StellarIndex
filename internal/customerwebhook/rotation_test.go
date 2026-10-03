package customerwebhook_test

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Stellar-Index/StellarIndex/internal/customerwebhook"
	"github.com/Stellar-Index/StellarIndex/internal/platform"
)

// deliverWithKeys runs one delivery for a webhook whose key was rotated
// from previous to current, with the overlap ending at expiresAt, and
// returns the headers the receiver saw plus the payload sent.
func deliverWithKeys(t *testing.T, current, previous []byte, expiresAt time.Time) (http.Header, uuid.UUID, []byte) {
	t.Helper()
	var hdr http.Header
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hdr = r.Header.Clone()
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	store := newFakeStore()
	webhookID := uuid.New()
	store.addWebhook(platform.CustomerWebhook{
		ID: webhookID, URL: ts.URL, SecretHash: current, Enabled: true,
		PreviousSecret: previous, PreviousSecretExpiresAt: expiresAt,
	})
	deliveryID := uuid.New()
	payload := []byte(`{"event":"incident.sev1"}`)
	store.enqueue(platform.WebhookDelivery{
		ID: deliveryID, WebhookID: webhookID,
		EventType: string(platform.WebhookEventIncidentSEV1),
		Payload:   payload, NextAttemptAt: time.Now().Add(-time.Second),
	})
	runOneTick(t, store, customerwebhook.Options{PollInterval: 30 * time.Millisecond})
	if hdr == nil {
		t.Fatal("no delivery reached the endpoint")
	}
	return hdr, deliveryID, payload
}

func macHex(key []byte, parts ...string) string {
	mac := hmac.New(sha256.New, key)
	for _, p := range parts {
		mac.Write([]byte(p))
	}
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// TestWorker_RotationOverlapSignsWithBothKeys: inside the overlap a
// receiver holding EITHER key can verify, so rotating never forces a
// window in which every delivery is rejected.
func TestWorker_RotationOverlapSignsWithBothKeys(t *testing.T) {
	current, previous := []byte("wsec_current"), []byte("wsec_previous")
	hdr, deliveryID, payload := deliverWithKeys(t, current, previous, time.Now().Add(time.Hour))

	ts := hdr.Get("X-StellarIndex-Timestamp")
	v1 := func(key []byte) string { return macHex(key, ts, ".", string(payload)) }
	v2 := func(key []byte) string {
		return macHex(key, ts, ".", deliveryID.String(), ".", string(platform.WebhookEventIncidentSEV1), ".", string(payload))
	}
	for _, c := range []struct {
		header string
		want   string
	}{
		{"X-StellarIndex-Signature", v1(current)},
		{"X-StellarIndex-Signature-V2", v2(current)},
		{"X-StellarIndex-Signature-Previous", v1(previous)},
		{"X-StellarIndex-Signature-V2-Previous", v2(previous)},
	} {
		if got := hdr.Get(c.header); got != c.want {
			t.Errorf("%s = %q, want %q", c.header, got, c.want)
		}
	}
}

// TestWorker_ExpiredPreviousKeyStopsSigning: once the overlap ends the old
// key signs nothing, so a leaked old key stops verifying anywhere that
// checks only the current headers.
func TestWorker_ExpiredPreviousKeyStopsSigning(t *testing.T) {
	hdr, _, _ := deliverWithKeys(t, []byte("wsec_current"), []byte("wsec_previous"), time.Now().Add(-time.Second))
	for _, h := range []string{"X-StellarIndex-Signature-Previous", "X-StellarIndex-Signature-V2-Previous"} {
		if got := hdr.Get(h); got != "" {
			t.Errorf("%s = %q after the overlap ended, want absent", h, got)
		}
	}
	if hdr.Get("X-StellarIndex-Signature") == "" {
		t.Error("current-key signature missing")
	}
}
