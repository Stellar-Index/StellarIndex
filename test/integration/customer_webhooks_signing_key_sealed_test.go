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

// preSealMigration is the newest migration before 0204 on this branch.
const preSealMigration = 201

// TestCustomerWebhooksSigningKeySealed executes migration 0204 up and down
// and the store paths around it: a raw pre-0204 key pair (current and
// rotation-previous) keeps signing, the startup sweep seals both and
// clears the raw copies, new and rotated keys are written sealed-only, a
// store without the seal key fails closed on a sealed row while its
// key-free lists keep working, and down refuses to drop a column that
// holds the only copy of a key.
func TestCustomerWebhooksSigningKeySealed(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrationsUpTo(t, dsn, preSealMigration)
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	acct := insertRawAccount(t, ctx, db, "sealed")

	legacyKey := []byte("whsec_legacy_0123456789abcdef0123")
	legacyPrev := []byte("whsec_legacy_prev_0123456789abcdef")
	var legacyID uuid.UUID
	if err := db.QueryRowContext(ctx, `
		INSERT INTO customer_webhooks
		    (account_id, name, url, secret_hash, previous_secret, previous_secret_expires_at, events)
		VALUES ($1, 'legacy', 'https://legacy.example/hook', $2, $3, now() + interval '1 day',
		        ARRAY['incident.sev1'])
		RETURNING id`, acct, legacyKey, legacyPrev).Scan(&legacyID); err != nil {
		t.Fatalf("seed pre-0204 webhook: %v", err)
	}
	applyMigrations(t, dsn)

	sealer, err := platform.NewWebhookKeySealer([]byte(strings.Repeat("k", platform.MinWebhookSealSecretLen)))
	if err != nil {
		t.Fatal(err)
	}
	pg := postgresstore.New(db)
	sealing := postgresstore.NewSealingWebhookStore(pg, sealer)
	keyless := postgresstore.NewWebhookStore(pg)

	keys := func(store *postgresstore.WebhookStore, id uuid.UUID) (current, previous []byte) {
		t.Helper()
		w, err := store.GetWebhook(ctx, id)
		if err != nil {
			t.Fatalf("GetWebhook %s: %v", id, err)
		}
		return w.SigningKey, w.PreviousSigningKey
	}
	signingKey := func(store *postgresstore.WebhookStore, id uuid.UUID) []byte {
		t.Helper()
		k, _ := keys(store, id)
		return k
	}
	previousColumns := func(id uuid.UUID) (raw, sealed []byte) {
		t.Helper()
		if err := db.QueryRowContext(ctx,
			`SELECT previous_secret, previous_signing_key_sealed FROM customer_webhooks WHERE id = $1`, id).
			Scan(&raw, &sealed); err != nil {
			t.Fatalf("read previous key columns of %s: %v", id, err)
		}
		return raw, sealed
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
	for name, store := range map[string]*postgresstore.WebhookStore{"sealing": sealing, "keyless": keyless} {
		if cur, prev := keys(store, legacyID); !bytes.Equal(cur, legacyKey) || !bytes.Equal(prev, legacyPrev) {
			t.Fatalf("legacy keys via %s store = (%q, %q), want (%q, %q)", name, cur, prev, legacyKey, legacyPrev)
		}
	}

	if n, err := sealing.SealLegacySigningKeys(ctx); err != nil || n != 2 {
		t.Fatalf("SealLegacySigningKeys = (%d, %v), want (2, nil): the current and the previous key", n, err)
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
	raw, sealed = previousColumns(legacyID)
	if raw != nil || len(sealed) == 0 || bytes.Contains(sealed, legacyPrev) {
		t.Errorf("legacy previous key stored (raw=%q, sealed=%x), want sealed-only", raw, sealed)
	}
	if cur, prev := keys(sealing, legacyID); !bytes.Equal(cur, legacyKey) || !bytes.Equal(prev, legacyPrev) {
		t.Errorf("sealed legacy keys open to (%q, %q), want (%q, %q)", cur, prev, legacyKey, legacyPrev)
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
		if w.SigningKey != nil || w.PreviousSigningKey != nil {
			t.Errorf("list returned a signing key for %s", w.ID)
		}
	}

	assertLostSealKeyRecovery(ctx, t, db, sealing, keyless, acct)
	assertRotationStaysSealed(ctx, t, db, sealing, keyless, acct, created.ID, newKey)

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

	refusedDown := func(why string) {
		t.Helper()
		if err := withMigrator(t, dsn, func(m *migrate.Migrate) error { return m.Migrate(preSealMigration) }); err == nil {
			t.Fatalf("0204 down ran while %s", why)
		}
		// The refused down changed nothing; clear the dirty mark it left.
		if err := withMigrator(t, dsn, func(m *migrate.Migrate) error { return m.Force(204) }); err != nil {
			t.Fatalf("force 204 after the refused down: %v", err)
		}
	}
	refusedDown("rows held only a sealed key")
	if _, err := db.ExecContext(ctx, `DELETE FROM customer_webhooks WHERE secret_hash IS NULL`); err != nil {
		t.Fatalf("delete sealed-only rows: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO customer_webhooks
		    (account_id, name, url, secret_hash, previous_signing_key_sealed, previous_secret_expires_at, events)
		VALUES ($1, 'sealed-prev', 'https://sealed-prev.example/hook', $2, $3, now() + interval '1 day',
		        ARRAY['incident.sev1'])`, acct, []byte("whsec_raw_current"), []byte{1, 2, 3}); err != nil {
		t.Fatalf("seed a raw current key with a sealed previous key: %v", err)
	}
	refusedDown("a row held only a sealed previous key")
	if _, err := db.ExecContext(ctx, `DELETE FROM customer_webhooks WHERE previous_signing_key_sealed IS NOT NULL`); err != nil {
		t.Fatalf("delete sealed-previous rows: %v", err)
	}
	applyMigrationsUpTo(t, dsn, preSealMigration)
	var cols int
	if err := db.QueryRowContext(ctx, `
		SELECT count(*) FROM information_schema.columns
		 WHERE table_name = 'customer_webhooks'
		   AND column_name IN ('signing_key_sealed', 'previous_signing_key_sealed')`).Scan(&cols); err != nil {
		t.Fatalf("read columns: %v", err)
	}
	if cols != 0 {
		t.Error("0204 down left a sealed-key column on customer_webhooks")
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO customer_webhooks (account_id, name, url, secret_hash, previous_secret_expires_at, events)
		VALUES ($1, 'half-pair', 'https://half-pair.example/hook', $2, now(), ARRAY['incident.sev1'])`,
		acct, []byte("whsec_half")); err == nil {
		t.Error("0204 down did not restore 0200's previous_secret pair CHECK")
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

// assertRotationStaysSealed pins the rotation half of the at-rest
// invariant: rotating a sealed row keeps both keys sealed and both raw
// columns empty while the outgoing key still signs through the overlap,
// a raw row rotated by a sealing store comes out sealed, a store without
// the seal key refuses to rotate a sealed row, and the schema rejects a
// previous key held in both forms or without its expiry.
func assertRotationStaysSealed(ctx context.Context, t *testing.T, db *sql.DB, sealing, keyless *postgresstore.WebhookStore, acct, id uuid.UUID, outgoing []byte) {
	t.Helper()
	assertSealedOnly := func(id uuid.UUID, current, previous []byte) {
		t.Helper()
		var raw, sealed, prevRaw, prevSealed []byte
		if err := db.QueryRowContext(ctx, `
			SELECT secret_hash, signing_key_sealed, previous_secret, previous_signing_key_sealed
			  FROM customer_webhooks WHERE id = $1`, id).Scan(&raw, &sealed, &prevRaw, &prevSealed); err != nil {
			t.Fatalf("read key columns of %s: %v", id, err)
		}
		if raw != nil || prevRaw != nil {
			t.Errorf("rotated row %s holds a raw key at rest (secret_hash=%q, previous_secret=%q)", id, raw, prevRaw)
		}
		if len(sealed) == 0 || len(prevSealed) == 0 || bytes.Contains(sealed, current) || bytes.Contains(prevSealed, previous) {
			t.Errorf("rotated row %s sealed columns = (%x, %x), want both sealed", id, sealed, prevSealed)
		}
		w, err := sealing.GetWebhook(ctx, id)
		if err != nil {
			t.Fatalf("GetWebhook %s after rotation: %v", id, err)
		}
		if !bytes.Equal(w.SigningKey, current) || !bytes.Equal(w.ActivePreviousSigningKey(time.Now()), previous) {
			t.Errorf("rotated row %s opens to (%q, %q), want (%q, %q) inside the overlap",
				id, w.SigningKey, w.ActivePreviousSigningKey(time.Now()), current, previous)
		}
	}
	expiry := time.Now().Add(24 * time.Hour)

	incoming := []byte("whsec_rotated_0123456789abcdef0123")
	if err := sealing.RotateWebhookSecret(ctx, id, incoming, expiry); err != nil {
		t.Fatalf("rotate a sealed row: %v", err)
	}
	assertSealedOnly(id, incoming, outgoing)

	// A second rotation inside the overlap replaces the older previous key.
	third := []byte("whsec_third_0123456789abcdef012345")
	if err := sealing.RotateWebhookSecret(ctx, id, third, expiry); err != nil {
		t.Fatalf("rotate a sealed row again: %v", err)
	}
	assertSealedOnly(id, third, incoming)

	if err := keyless.RotateWebhookSecret(ctx, id, []byte("whsec_keyless"), expiry); !errors.Is(err, platform.ErrWebhookSealKeyMissing) {
		t.Errorf("keyless rotation of a sealed row: err = %v, want ErrWebhookSealKeyMissing", err)
	}

	rawKey := []byte("whsec_raw_0123456789abcdef01234567")
	rawRow, err := keyless.CreateWebhook(ctx, platform.CustomerWebhook{
		AccountID: acct, Name: "raw", URL: "https://raw.example/hook", SigningKey: rawKey,
		Events: []string{string(platform.WebhookEventIncidentSEV1)}, Enabled: true,
	}, 10)
	if err != nil {
		t.Fatalf("CreateWebhook raw: %v", err)
	}
	if err := sealing.RotateWebhookSecret(ctx, rawRow.ID, incoming, expiry); err != nil {
		t.Fatalf("sealing rotation of a raw row: %v", err)
	}
	assertSealedOnly(rawRow.ID, incoming, rawKey)
	if err := keyless.DeleteWebhook(ctx, rawRow.ID); err != nil {
		t.Fatalf("DeleteWebhook raw: %v", err)
	}

	for name, q := range map[string]string{
		"previous key in both forms":             `UPDATE customer_webhooks SET previous_secret = 'x' WHERE id = $1`,
		"sealed previous key without its expiry": `UPDATE customer_webhooks SET previous_secret_expires_at = NULL WHERE id = $1`,
	} {
		if _, err := db.ExecContext(ctx, q, id); err == nil {
			t.Errorf("schema accepted a %s", name)
		}
	}
}
