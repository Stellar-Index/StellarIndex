package postgresstore

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestRotateWebhookSecret_RefusesUnusableInput pins the pre-SQL guards: an empty key would sign forgeably and a zero expiry violates the previous-secret CHECK; the UPDATE itself is covered by test/integration.
func TestRotateWebhookSecret_RefusesUnusableInput(t *testing.T) {
	store := &WebhookStore{}
	ctx := context.Background()
	if err := store.RotateWebhookSecret(ctx, uuid.New(), nil, time.Now().Add(time.Hour)); err == nil || !strings.Contains(err.Error(), "newSecret is empty") {
		t.Errorf("empty new secret: err = %v, want a refusal before any SQL", err)
	}
	if err := store.RotateWebhookSecret(ctx, uuid.New(), []byte("wsec_x"), time.Time{}); err == nil || !strings.Contains(err.Error(), "previousExpiresAt is zero") {
		t.Errorf("zero expiry: err = %v, want a refusal before any SQL", err)
	}
}
