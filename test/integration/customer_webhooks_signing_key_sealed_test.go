//go:build integration

package integration_test

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4"
	"github.com/google/uuid"

	"github.com/Stellar-Index/StellarIndex/internal/platform"
	"github.com/Stellar-Index/StellarIndex/internal/platform/postgresstore"
)

// TestCustomerWebhooksSigningKeySealed executes migration 0199 up and down
// and the store paths around it: a raw pre-0199 key keeps signing, the
// startup sweep seals it and clears the raw copy, new keys are written
// sealed-only, a store without the seal key fails closed on a sealed row
// while its key-free lists keep working, and down refuses to drop a
// column that holds the only copy of a key.
func TestCustomerWebhooksSigningKeySealed(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrationsUpTo(t, dsn, 198)
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	acct := insertRawAccount(t, ctx, db, "sealed")

	legacyKey := []byte("whsec_legacy_0123456789abcdef0123")
	var legacyID uuid.UUID
	if err := db.QueryRowContext(ctx, `
		INSERT INTO customer_webhooks (account_id, name, url, secret_hash, events)
		VALUES ($1, 'legacy', 'https://legacy.example/hook', $2, ARRAY['incident.sev1'])
		RETURNING id`, acct, legacyKey).Scan(&legacyID); err != nil {
		t.Fatalf("seed pre-0199 webhook: %v", err)
	}
	applyMigrations(t, dsn)

	sealer, err := platform.NewWebhookKeySealer([]byte(strings.Repeat("k", platform.MinWebhookSealSecretLen)))
	if err != nil {
		t.Fatal(err)
	}
	pg := postgresstore.New(db)
	sealing := postgresstore.NewSealingWebhookStore(pg, sealer)
	keyless := postgresstore.NewWebhookStore(pg)

	signingKey := func(store *postgresstore.WebhookStore, id uuid.UUID) []byte {
		t.Helper()
		w, err := store.GetWebhook(ctx, id)
		if err != nil {
			t.Fatalf("GetWebhook %s: %v", id, err)
		}
		return w.SigningKey
	}
	columns := func(id uuid.UUID) (raw, sealed []byte) {
		t.Helper()
		if err := db.QueryRowContext(ctx,
			`SELECT secret_hash, signing_key_sealed FROM customer_webhooks WHERE id = $1`, id).
			Scan(&raw, &sealed); err != nil {
			t.Fatalf("read key columns of %s: %v", id, err)
		}
		return raw, sealed
	}

	// The previous binary's raw row still signs, through either store.
	if got := signingKey(sealing, legacyID); !bytes.Equal(got, legacyKey) {
		t.Fatalf("legacy key via sealing store = %q, want %q", got, legacyKey)
	}
	if got := signingKey(keyless, legacyID); !bytes.Equal(got, legacyKey) {
		t.Fatalf("legacy key via keyless store = %q, want %q", got, legacyKey)
	}

	if n, err := sealing.SealLegacySigningKeys(ctx); err != nil || n != 1 {
		t.Fatalf("SealLegacySigningKeys = (%d, %v), want (1, nil)", n, err)
	}
	if n, err := sealing.SealLegacySigningKeys(ctx); err != nil || n != 0 {
		t.Errorf("second SealLegacySigningKeys = (%d, %v), want (0, nil)", n, err)
	}
	raw, sealed := columns(legacyID)
	if raw != nil {
		t.Error("sealing left the raw key in secret_hash")
	}
	if len(sealed) == 0 || bytes.Contains(sealed, legacyKey) {
		t.Errorf("signing_key_sealed = %x, want a sealed copy that does not contain the key", sealed)
	}
	if got := signingKey(sealing, legacyID); !bytes.Equal(got, legacyKey) {
		t.Errorf("sealed legacy key opens to %q, want %q", got, legacyKey)
	}

	newKey := []byte("whsec_new_0123456789abcdef01234567")
	created, err := sealing.CreateWebhook(ctx, platform.CustomerWebhook{
		AccountID: acct, Name: "new", URL: "https://new.example/hook", SigningKey: newKey,
		Events: []string{string(platform.WebhookEventIncidentSEV1)}, Enabled: true,
	}, 10)
	if err != nil {
		t.Fatalf("CreateWebhook: %v", err)
	}
	raw, sealed = columns(created.ID)
	if raw != nil || len(sealed) == 0 || bytes.Contains(sealed, newKey) {
		t.Errorf("created row stores (raw=%q, sealed=%x), want sealed-only", raw, sealed)
	}
	if got := signingKey(sealing, created.ID); !bytes.Equal(got, newKey) {
		t.Errorf("created key opens to %q, want %q", got, newKey)
	}

	// Without the seal key a sealed row is an error, never an empty key.
	if _, err := keyless.GetWebhook(ctx, created.ID); !errors.Is(err, platform.ErrWebhookKeyUnsealable) {
		t.Errorf("keyless GetWebhook on a sealed row: err = %v, want ErrWebhookKeyUnsealable", err)
	}
	// The fan-out and dashboard lists never need the key, so they work without it.
	subs, err := keyless.ListWebhooksSubscribedTo(ctx, platform.WebhookEventIncidentSEV1)
	if err != nil || len(subs) != 2 {
		t.Fatalf("keyless ListWebhooksSubscribedTo = (%d rows, %v), want 2", len(subs), err)
	}
	listed, err := keyless.ListWebhooksForAccount(ctx, acct)
	if err != nil || len(listed) != 2 {
		t.Fatalf("keyless ListWebhooksForAccount = (%d rows, %v), want 2", len(listed), err)
	}
	for _, w := range append(subs, listed...) {
		if w.SigningKey != nil {
			t.Errorf("list returned a signing key for %s", w.ID)
		}
	}

	assertLostSealKeyRecovery(ctx, t, db, sealing, keyless, acct)

	// A sealed key copied onto another row does not open there.
	if _, err := db.ExecContext(ctx, `
		UPDATE customer_webhooks SET signing_key_sealed =
		       (SELECT signing_key_sealed FROM customer_webhooks WHERE id = $2)
		 WHERE id = $1`, legacyID, created.ID); err != nil {
		t.Fatalf("copy sealed key: %v", err)
	}
	if _, err := sealing.GetWebhook(ctx, legacyID); !errors.Is(err, platform.ErrWebhookKeyUnsealable) {
		t.Errorf("GetWebhook with another row's sealed key: err = %v, want ErrWebhookKeyUnsealable", err)
	}

	if _, err := db.ExecContext(ctx, `
		INSERT INTO customer_webhooks (account_id, name, url, events)
		VALUES ($1, 'keyless', 'https://keyless.example/hook', ARRAY['incident.sev1'])`, acct); err == nil {
		t.Error("a row with neither a raw nor a sealed key was accepted")
	}

	if err := withMigrator(t, dsn, func(m *migrate.Migrate) error { return m.Migrate(198) }); err == nil {
		t.Fatal("0199 down dropped signing_key_sealed while rows held only a sealed key")
	}
	// The refused down changed nothing; clear the dirty mark it left.
	if err := withMigrator(t, dsn, func(m *migrate.Migrate) error { return m.Force(199) }); err != nil {
		t.Fatalf("force 199 after the refused down: %v", err)
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM customer_webhooks WHERE secret_hash IS NULL`); err != nil {
		t.Fatalf("delete sealed-only rows: %v", err)
	}
	applyMigrationsUpTo(t, dsn, 198)
	var cols int
	if err := db.QueryRowContext(ctx, `
		SELECT count(*) FROM information_schema.columns
		 WHERE table_name = 'customer_webhooks' AND column_name = 'signing_key_sealed'`).Scan(&cols); err != nil {
		t.Fatalf("read columns: %v", err)
	}
	if cols != 0 {
		t.Error("0199 down left customer_webhooks.signing_key_sealed in place")
	}
	applyMigrations(t, dsn)
}

// assertLostSealKeyRecovery pins the documented recovery from a lost or
// rotated seal key: a sealed row still reads back without its key, a
// missing seal key is told apart from a wrong one, and the webhook can be
// edited, deleted and recreated at the same URL without the key.
func assertLostSealKeyRecovery(ctx context.Context, t *testing.T, db *sql.DB, sealing, keyless *postgresstore.WebhookStore, acct uuid.UUID) {
	t.Helper()
	const url = "https://recover.example/hook"
	doomed, err := sealing.CreateWebhook(ctx, platform.CustomerWebhook{
		AccountID: acct, Name: "doomed", URL: url, SigningKey: []byte("whsec_doomed_0123456789abcdef0123"),
		Events: []string{string(platform.WebhookEventIncidentSEV1)}, Enabled: true,
	}, 10)
	if err != nil {
		t.Fatalf("CreateWebhook doomed: %v", err)
	}

	otherSealer, err := platform.NewWebhookKeySealer([]byte(strings.Repeat("r", platform.MinWebhookSealSecretLen)))
	if err != nil {
		t.Fatal(err)
	}
	rotated := postgresstore.NewSealingWebhookStore(postgresstore.New(db), otherSealer)
	_, err = rotated.GetWebhook(ctx, doomed.ID)
	if !errors.Is(err, platform.ErrWebhookKeyUnsealable) || errors.Is(err, platform.ErrWebhookSealKeyMissing) {
		t.Errorf("GetWebhook under a rotated seal key: err = %v, want unsealable but not seal-key-missing", err)
	}

	got, err := keyless.GetWebhook(ctx, doomed.ID)
	if !errors.Is(err, platform.ErrWebhookSealKeyMissing) {
		t.Errorf("keyless GetWebhook on a sealed row: err = %v, want ErrWebhookSealKeyMissing", err)
	}
	if got.ID != doomed.ID || got.AccountID != acct || got.URL != url || got.SigningKey != nil {
		t.Fatalf("keyless GetWebhook row = %+v, want the row's metadata without a key", got)
	}

	got.Name = "renamed"
	if err := keyless.UpdateWebhook(ctx, got); err != nil {
		t.Fatalf("keyless UpdateWebhook: %v", err)
	}
	if err := keyless.DeleteWebhook(ctx, doomed.ID); err != nil {
		t.Fatalf("keyless DeleteWebhook: %v", err)
	}
	again, err := keyless.CreateWebhook(ctx, platform.CustomerWebhook{
		AccountID: acct, Name: "again", URL: url, SigningKey: []byte("whsec_again_0123456789abcdef01234"),
		Events: []string{string(platform.WebhookEventIncidentSEV1)}, Enabled: true,
	}, 10)
	if err != nil {
		t.Fatalf("recreate at the same URL: %v", err)
	}
	if err := keyless.DeleteWebhook(ctx, again.ID); err != nil {
		t.Fatalf("DeleteWebhook again: %v", err)
	}
}

// withMigrator runs fn on a migrator it closes straight after, so a
// refused migration's session lock is gone before the next one starts.
func withMigrator(t *testing.T, dsn string, fn func(*migrate.Migrate) error) error {
	t.Helper()
	_, thisFile, _, _ := runtime.Caller(0)
	m, err := migrate.New("file://"+filepath.Join(filepath.Dir(thisFile), "..", "..", "migrations"), dsn)
	if err != nil {
		t.Fatalf("migrate.New: %v", err)
	}
	defer func() { _, _ = m.Close() }()
	return fn(m)
}
