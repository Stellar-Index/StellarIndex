//go:build integration

package integration_test

import (
	"bytes"
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/platform"
	"github.com/Stellar-Index/StellarIndex/internal/platform/postgresstore"
)

// TestCustomerWebhooksUnopenablePreviousKey pins that a rotation-previous
// key that cannot be opened never takes the current key down with it:
// an expired one is not opened at all, an unexpired one is dropped.
func TestCustomerWebhooksUnopenablePreviousKey(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	acct := insertRawAccount(t, ctx, db, "prevkey")

	sealer, err := platform.NewWebhookKeySealer([]byte(strings.Repeat("k", platform.MinWebhookSealSecretLen)))
	if err != nil {
		t.Fatal(err)
	}
	store := postgresstore.NewSealingWebhookStore(postgresstore.New(db), sealer)

	cases := []struct {
		name   string
		expiry string
	}{
		{"expired", "now() - interval '1 hour'"},
		{"unexpired", "now() + interval '1 hour'"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			current := []byte("whsec_current_0123456789abcdef0123")
			w, err := store.CreateWebhook(ctx, platform.CustomerWebhook{
				AccountID: acct, Name: tc.name, URL: "https://prevkey.example/" + tc.name, SigningKey: []byte("whsec_old_0123456789abcdef01234567"),
				Events: []string{string(platform.WebhookEventIncidentSEV1)}, Enabled: true,
			}, 10)
			if err != nil {
				t.Fatalf("CreateWebhook: %v", err)
			}
			if err := store.RotateWebhookSecret(ctx, w.ID, current, time.Now().Add(time.Hour)); err != nil {
				t.Fatalf("RotateWebhookSecret: %v", err)
			}
			// A previous blob sealed under a different key.
			if _, err := db.ExecContext(ctx, `
				UPDATE customer_webhooks
				   SET previous_signing_key_sealed = $2, previous_secret_expires_at = `+tc.expiry+`
				 WHERE id = $1`, w.ID, []byte("not-a-sealed-blob-from-this-key")); err != nil {
				t.Fatalf("corrupt previous key: %v", err)
			}
			got, err := store.GetWebhook(ctx, w.ID)
			if err != nil {
				t.Fatalf("GetWebhook: %v", err)
			}
			if !bytes.Equal(got.SigningKey, current) {
				t.Errorf("SigningKey = %q, want the current key %q", got.SigningKey, current)
			}
			if got.PreviousSigningKey != nil {
				t.Errorf("PreviousSigningKey = %q, want nil", got.PreviousSigningKey)
			}
		})
	}
}
