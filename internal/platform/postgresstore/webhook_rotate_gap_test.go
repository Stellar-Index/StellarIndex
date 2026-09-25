package postgresstore

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// TestRotateWebhookSecret_IsNotImplemented records GH-665 as it stands:
// the declared in-place rotation returns a not-implemented error without
// touching the database, so the only rotation path is delete + recreate,
// whose DELETE cascades away the endpoint's queued deliveries and log.
// It passes while the gap exists; replace it with the executing
// integration test the real implementation needs when rotation lands.
func TestRotateWebhookSecret_IsNotImplemented(t *testing.T) {
	secret, err := (&WebhookStore{}).RotateWebhookSecret(context.Background(), uuid.New())
	if err == nil || !strings.Contains(err.Error(), "not yet implemented") {
		t.Fatalf("RotateWebhookSecret = (%q, %v): rotation now exists — replace this gap record with a real test", secret, err)
	}
	if secret != "" {
		t.Errorf("returned a secret %q alongside the not-implemented error", secret)
	}
}
