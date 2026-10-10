//go:build integration

package integration_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4"
	"github.com/google/uuid"

	"github.com/Stellar-Index/StellarIndex/internal/customerwebhook"
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

// preSealMigration is the newest migration before 0204 on this branch.
const preSealMigration = 203

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

const dupWebhookURL = "https://hooks.example/stellarindex"

// TestCustomerWebhooksURLUnique pins migration 0180: one webhook
// per (account, url), surfaced by the store as platform.ErrConflict; the
// migration refuses — rather than merges or deletes — pre-existing
// duplicates; and a quota of 0 (TierAnon) admits nothing.
func TestCustomerWebhooksURLUnique(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrationsUpTo(t, dsn, 179)
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	store := postgresstore.New(db)
	accounts := postgresstore.NewAccountStore(store)
	webhooks := postgresstore.NewWebhookStore(store)
	acct := insertRawAccount(t, ctx, db, "a")

	t.Run("MigrationRefusesExistingDuplicates", func(t *testing.T) {
		first := insertRawWebhook(t, ctx, db, acct, dupWebhookURL)
		second := insertRawWebhook(t, ctx, db, acct, dupWebhookURL)
		err := execUpMigration(t, ctx, db, "0180_customer_webhooks_account_url_unique.up.sql")
		if err == nil || !strings.Contains(err.Error(), "0180:") {
			t.Fatalf("0180 over duplicates: err = %v, want its pre-flight refusal", err)
		}
		var n int
		if err := db.QueryRowContext(ctx,
			`SELECT count(*) FROM customer_webhooks WHERE id IN ($1, $2)`, first, second).Scan(&n); err != nil {
			t.Fatalf("count: %v", err)
		}
		if n != 2 {
			t.Fatalf("refused migration left %d of the 2 duplicate rows, want both untouched", n)
		}
		if _, err := db.ExecContext(ctx, `DELETE FROM customer_webhooks WHERE id = $1`, second); err != nil {
			t.Fatalf("resolve duplicate: %v", err)
		}
	})

	applyMigrations(t, dsn)

	t.Run("StoreMapsDuplicateToConflict", func(t *testing.T) {
		_, err := webhooks.CreateWebhook(ctx, uniqueURLHook(acct, dupWebhookURL), 10)
		if !errors.Is(err, platform.ErrConflict) {
			t.Fatalf("second create of the same url: err = %v, want ErrConflict", err)
		}
		other := uniqueURLAccount(t, ctx, accounts, "b")
		if _, err := webhooks.CreateWebhook(ctx, uniqueURLHook(other, dupWebhookURL), 10); err != nil {
			t.Fatalf("same url on a different account: %v, want allowed", err)
		}
		sibling, err := webhooks.CreateWebhook(ctx, uniqueURLHook(acct, "https://hooks.example/other"), 10)
		if err != nil {
			t.Fatalf("create sibling: %v", err)
		}
		sibling.URL = dupWebhookURL
		if err := webhooks.UpdateWebhook(ctx, sibling); !errors.Is(err, platform.ErrConflict) {
			t.Fatalf("update onto a sibling's url: err = %v, want ErrConflict", err)
		}
	})

	t.Run("ZeroQuotaAdmitsNothing", func(t *testing.T) {
		anon := uniqueURLAccount(t, ctx, accounts, "anon")
		_, err := webhooks.CreateWebhook(ctx, uniqueURLHook(anon, "https://hooks.example/anon"), 0)
		if !errors.Is(err, platform.ErrWebhookQuotaExceeded) {
			t.Fatalf("maxPerAccount=0: err = %v, want ErrWebhookQuotaExceeded", err)
		}
		listed, err := webhooks.ListWebhooksForAccount(ctx, anon)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if len(listed) != 0 {
			t.Fatalf("maxPerAccount=0 stored %d webhooks, want 0", len(listed))
		}
	})
}

func uniqueURLAccount(t *testing.T, ctx context.Context, accounts *postgresstore.AccountStore, tag string) uuid.UUID {
	t.Helper()
	suffix := tag + "-" + strings.ToLower(uuid.New().String()[:8])
	acct, err := accounts.Create(ctx, platform.Account{
		Name:         "Webhook URL " + suffix,
		Slug:         "wu-" + suffix,
		BillingEmail: "wu-" + suffix + "@k.example",
		Tier:         platform.TierFree,
		Status:       platform.AccountActive,
	})
	if err != nil {
		t.Fatalf("create account: %v", err)
	}
	return acct.ID
}

func uniqueURLHook(accountID uuid.UUID, url string) platform.CustomerWebhook {
	signing := sha256.Sum256([]byte("url-unique-signing-material"))
	return platform.CustomerWebhook{
		AccountID:  accountID,
		Name:       "hook",
		URL:        url,
		SigningKey: signing[:],
		Events:     []string{string(platform.WebhookEventIncidentSEV1)},
		Enabled:    true,
	}
}

// insertRawAccount bypasses AccountStore.Create, which (since migration
// 0188) refuses a slug erased_account_slugs records — a table this test's
// pre-0180 schema doesn't have yet.
func insertRawAccount(t *testing.T, ctx context.Context, db *sql.DB, tag string) uuid.UUID {
	t.Helper()
	suffix := tag + "-" + strings.ToLower(uuid.New().String()[:8])
	var id uuid.UUID
	if err := db.QueryRowContext(ctx, `
		INSERT INTO accounts (name, slug, billing_email)
		VALUES ($1, $2, $3) RETURNING id`,
		"Account "+suffix, suffix, suffix+"@k.example").Scan(&id); err != nil {
		t.Fatalf("insert raw account: %v", err)
	}
	return id
}

// insertRawWebhook bypasses the store: the pre-0180 schema is the only
// place a duplicate can exist.
func insertRawWebhook(t *testing.T, ctx context.Context, db *sql.DB, accountID uuid.UUID, url string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := db.QueryRowContext(ctx, `
		INSERT INTO customer_webhooks (account_id, name, url, secret_hash, events)
		VALUES ($1, 'dup', $2, '\x00'::bytea, ARRAY['incident.sev1'])
		RETURNING id`, accountID, url).Scan(&id); err != nil {
		t.Fatalf("insert raw webhook: %v", err)
	}
	return id
}

// execUpMigration runs one up file on a dedicated connection and rolls
// back whatever transaction a failure leaves open.
func execUpMigration(t *testing.T, ctx context.Context, db *sql.DB, name string) error {
	t.Helper()
	_, thisFile, _, _ := runtime.Caller(0)
	body, err := os.ReadFile(filepath.Join(filepath.Dir(thisFile), "..", "..", "migrations", name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatalf("conn: %v", err)
	}
	defer func() { _ = conn.Close() }()
	_, execErr := conn.ExecContext(ctx, string(body))
	if execErr != nil {
		_, _ = conn.ExecContext(ctx, "ROLLBACK")
	}
	return execErr
}

// The account kill switch was INBOUND-ONLY.
//
// Suspending or closing an account stopped its API keys authenticating
// (internal/auth), but every webhook query was blind to account status:
// the resolver (ListWebhooksSubscribedTo), both enqueue writers
// (EnqueueDelivery, AppendDelivery) and the claim query
// (ListPendingDeliveries). A suspended or closed customer therefore kept
// accruing queued rows AND kept receiving our data at the endpoints they
// had registered.
//
// This test executes the whole path against real Postgres for an ACTIVE,
// a SUSPENDED and a CLOSED account, and asserts what each one ENQUEUES
// as well as what each one DELIVERS. The Go-level worker gate is pinned
// separately in internal/customerwebhook/account_killswitch_test.go.

const killSwitchEvent = platform.WebhookEventIncidentSEV1

// killSwitchAccount is one account under test: its lifecycle status, its
// registered webhook, and — where the test needs it — the endpoint that
// counts what that webhook actually received.
type killSwitchAccount struct {
	name      string
	status    platform.AccountStatus
	accountID uuid.UUID
	webhookID uuid.UUID
	posts     *int64
}

func TestCustomerWebhookAccountKillSwitch(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	store := postgresstore.New(db)
	accounts := postgresstore.NewAccountStore(store)
	webhooks := postgresstore.NewWebhookStore(store)

	cases := seedKillSwitchTrio(t, ctx, accounts, webhooks, "queue", false)

	t.Run("resolver hands the fan-out only active accounts", func(t *testing.T) {
		assertResolverSkipsInactive(t, ctx, webhooks, cases)
	})

	t.Run("enqueue writers refuse a non-active account", func(t *testing.T) {
		assertEnqueueRefusedForInactive(t, ctx, webhooks, cases)
		assertQueuedRows(t, ctx, db, cases)
	})

	t.Run("WebhookAccountStatus reports the owning account", func(t *testing.T) {
		for _, c := range cases {
			got, statusErr := webhooks.WebhookAccountStatus(ctx, c.webhookID)
			if statusErr != nil {
				t.Fatalf("%s account: WebhookAccountStatus: %v", c.name, statusErr)
			}
			if got != c.status {
				t.Errorf("%s account: status = %q, want %q", c.name, got, c.status)
			}
		}
		if _, missing := webhooks.WebhookAccountStatus(ctx, uuid.New()); !errors.Is(missing, platform.ErrNotFound) {
			t.Errorf("unknown webhook: err = %v, want ErrNotFound", missing)
		}
	})

	t.Run("claim query parks rows queued before a suspension", func(t *testing.T) {
		assertSuspensionParksBacklog(t, ctx, db, accounts, webhooks)
	})

	t.Run("worker POSTs to the active account only", func(t *testing.T) {
		assertWorkerDeliversToActiveOnly(t, ctx, accounts, webhooks)
	})

	t.Run("fan-out reports a mid-publish suspension as suppressed", func(t *testing.T) {
		assertMidPublishSuspensionIsSuppressed(t, ctx, accounts, webhooks)
	})
}

// ─── assertions ─────────────────────────────────────────────────

func assertResolverSkipsInactive(
	t *testing.T, ctx context.Context,
	webhooks *postgresstore.WebhookStore, cases []*killSwitchAccount,
) {
	t.Helper()
	subs, err := webhooks.ListWebhooksSubscribedTo(ctx, killSwitchEvent)
	if err != nil {
		t.Fatalf("ListWebhooksSubscribedTo: %v", err)
	}
	got := map[uuid.UUID]bool{}
	for _, s := range subs {
		got[s.ID] = true
	}
	for _, c := range cases {
		want := c.status == platform.AccountActive
		if got[c.webhookID] != want {
			t.Errorf("%s account: subscriber present = %v, want %v — "+
				"a non-active account must not be fanned out to",
				c.name, got[c.webhookID], want)
		}
	}
}

func assertEnqueueRefusedForInactive(
	t *testing.T, ctx context.Context,
	webhooks *postgresstore.WebhookStore, cases []*killSwitchAccount,
) {
	t.Helper()
	for _, c := range cases {
		enqErr := webhooks.EnqueueDelivery(ctx, dueDelivery(c.webhookID, "enqueue-probe"))
		_, appErr := webhooks.AppendDelivery(ctx, platform.WebhookDelivery{
			WebhookID: c.webhookID,
			EventType: string(killSwitchEvent),
			Payload:   []byte(`{"incident_id":"append-probe"}`),
		})
		if c.status == platform.AccountActive {
			if enqErr != nil {
				t.Errorf("active account: EnqueueDelivery: %v", enqErr)
			}
			if appErr != nil {
				t.Errorf("active account: AppendDelivery: %v", appErr)
			}
			continue
		}
		if !errors.Is(enqErr, postgresstore.ErrWebhookAccountInactive) {
			t.Errorf("%s account: EnqueueDelivery err = %v, want ErrWebhookAccountInactive — "+
				"rows must not be queued against it", c.name, enqErr)
		}
		if !errors.Is(appErr, postgresstore.ErrWebhookAccountInactive) {
			t.Errorf("%s account: AppendDelivery err = %v, want ErrWebhookAccountInactive",
				c.name, appErr)
		}
	}
}

// assertQueuedRows pins what each account has in webhook_deliveries: the
// active account accrues rows, the non-active ones accrue nothing beyond
// the one seeded before their status changed.
func assertQueuedRows(t *testing.T, ctx context.Context, db *sql.DB, cases []*killSwitchAccount) {
	t.Helper()
	for _, c := range cases {
		var n int
		if err := db.QueryRowContext(ctx,
			`SELECT count(*) FROM webhook_deliveries WHERE webhook_id = $1`,
			c.webhookID).Scan(&n); err != nil {
			t.Fatalf("%s: count deliveries: %v", c.name, err)
		}
		// 1 seeded while still active, plus the EnqueueDelivery and the
		// AppendDelivery probes — which only land for an active account.
		want := 1
		if c.status == platform.AccountActive {
			want = 3
		}
		if n != want {
			t.Errorf("%s account has %d queued delivery row(s), want %d", c.name, n, want)
		}
	}
}

// assertSuspensionParksBacklog: a row queued while the account was
// ACTIVE, with the account suspended afterwards, must be withheld from
// the claim — and must SURVIVE, because suspension is reversible.
func assertSuspensionParksBacklog(
	t *testing.T, ctx context.Context, db *sql.DB,
	accounts *postgresstore.AccountStore, webhooks *postgresstore.WebhookStore,
) {
	t.Helper()
	acct, hook := freshKillSwitchWebhook(t, ctx, accounts, webhooks, "https://parked.example/hook")
	if err := webhooks.EnqueueDelivery(ctx, dueDelivery(hook, "parked")); err != nil {
		t.Fatalf("park: EnqueueDelivery: %v", err)
	}
	suspendAccount(t, ctx, accounts, acct)

	claimed, err := webhooks.ListPendingDeliveries(ctx, 100)
	if err != nil {
		t.Fatalf("ListPendingDeliveries: %v", err)
	}
	for _, d := range claimed {
		if d.WebhookID == hook {
			t.Fatal("claim handed the worker a delivery for a suspended account")
		}
	}
	var still int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM webhook_deliveries WHERE webhook_id = $1 AND delivered_at IS NULL`,
		hook).Scan(&still); err != nil {
		t.Fatalf("count parked: %v", err)
	}
	if still != 1 {
		t.Errorf("parked deliveries = %d, want 1 — suspension is reversible, "+
			"the backlog must be withheld, not destroyed", still)
	}
}

// assertWorkerDeliversToActiveOnly is the end of the line: drain a fresh
// queue with the real worker against the real store and count the HTTP
// POSTs each customer endpoint actually received.
func assertWorkerDeliversToActiveOnly(
	t *testing.T, ctx context.Context,
	accounts *postgresstore.AccountStore, webhooks *postgresstore.WebhookStore,
) {
	t.Helper()
	cases := seedKillSwitchTrio(t, ctx, accounts, webhooks, "deliver", true)

	// httptest listens on 127.0.0.1 with a self-signed cert, both of
	// which the production client rejects by design (SSRF guard,
	// certificate verification).
	w := customerwebhook.NewUnguardedForIntegrationTest(webhooks, customerwebhook.Options{
		PollInterval: 50 * time.Millisecond,
		HTTPClient: &http.Client{
			Timeout: 5 * time.Second,
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
			},
		},
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	runCtx, stop := context.WithTimeout(ctx, 800*time.Millisecond)
	defer stop()
	_ = w.Run(runCtx)

	for _, c := range cases {
		got := atomic.LoadInt64(c.posts)
		want := int64(0)
		if c.status == platform.AccountActive {
			want = 1
		}
		if got != want {
			t.Errorf("%s account's endpoint received %d POST(s), want %d — "+
				"a non-active account must never be sent our data", c.name, got, want)
		}
	}
}

// assertMidPublishSuspensionIsSuppressed drives the narrow window the
// enqueue-side gate exists for: the resolver saw an active account and
// the insert happened after the suspension landed. That is a deliberate
// withholding, not a lost customer event.
func assertMidPublishSuspensionIsSuppressed(
	t *testing.T, ctx context.Context,
	accounts *postgresstore.AccountStore, webhooks *postgresstore.WebhookStore,
) {
	t.Helper()
	acct, hook := freshKillSwitchWebhook(t, ctx, accounts, webhooks, "https://suppressed.example/hook")
	f := customerwebhook.NewFanout(&suspendBetweenResolveAndInsert{
		WebhookStore: webhooks,
		suspend:      func() { suspendAccount(t, ctx, accounts, acct) },
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))

	res, err := f.Publish(ctx, killSwitchEvent, []byte(`{"incident_id":"race"}`))
	if err != nil {
		t.Fatalf("Publish reported a lost event for a deliberate suppression: %v", err)
	}
	if res.Suppressed != 1 {
		t.Errorf("Suppressed = %d, want 1 (webhook %s)", res.Suppressed, hook)
	}
	if res.Failed != 0 {
		t.Errorf("Failed = %d, want 0 — a suppression must not reach the lost-event alert",
			res.Failed)
	}
	if res.Enqueued+res.Failed+res.Suppressed != res.Subscribers {
		t.Errorf("counts do not partition the subscriber set: %+v", res)
	}
}

// ─── fixtures ───────────────────────────────────────────────────

// suspendBetweenResolveAndInsert resolves subscribers through the real
// store, then suspends the account before the fan-out gets to insert —
// reproducing the race the enqueue-side gate exists to close.
type suspendBetweenResolveAndInsert struct {
	*postgresstore.WebhookStore
	suspend func()
	once    atomic.Bool
}

func (s *suspendBetweenResolveAndInsert) ListWebhooksSubscribedTo(
	ctx context.Context, eventType platform.WebhookEventType,
) ([]platform.CustomerWebhook, error) {
	subs, err := s.WebhookStore.ListWebhooksSubscribedTo(ctx, eventType)
	if err == nil && s.once.CompareAndSwap(false, true) {
		s.suspend()
	}
	return subs, err
}

// seedKillSwitchTrio creates one active, one suspended and one closed
// account, each with an enabled webhook and one DUE delivery queued
// while the account was still active — so the only difference between
// the three is the account's status. With `withEndpoints` each webhook
// points at a live endpoint that counts the POSTs it receives.
func seedKillSwitchTrio(
	t *testing.T, ctx context.Context,
	accounts *postgresstore.AccountStore, webhooks *postgresstore.WebhookStore,
	tag string, withEndpoints bool,
) []*killSwitchAccount {
	t.Helper()
	cases := []*killSwitchAccount{
		{name: "active", status: platform.AccountActive},
		{name: "suspended", status: platform.AccountSuspended},
		{name: "closed", status: platform.AccountClosed},
	}
	for _, c := range cases {
		url := fmt.Sprintf("https://%s-%s.example/hook", tag, c.name)
		if withEndpoints {
			var posts int64
			c.posts = &posts
			// TLS, not plain http: the customer_webhooks.url CHECK
			// constraint (migration 0027) only accepts `^https://`.
			ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				atomic.AddInt64(&posts, 1)
				w.WriteHeader(http.StatusOK)
			}))
			t.Cleanup(ts.Close)
			url = ts.URL
		}
		c.accountID, c.webhookID = freshKillSwitchWebhook(t, ctx, accounts, webhooks, url)
		if err := webhooks.EnqueueDelivery(ctx, dueDelivery(c.webhookID, tag)); err != nil {
			t.Fatalf("%s/%s: seed EnqueueDelivery: %v", tag, c.name, err)
		}
		switch c.status {
		case platform.AccountActive:
			// Already created active; nothing to change.
		case platform.AccountSuspended:
			suspendAccount(t, ctx, accounts, c.accountID)
		case platform.AccountClosed:
			closeAccount(t, ctx, accounts, c.accountID)
		}
	}
	return cases
}

// freshKillSwitchWebhook creates an ACTIVE account with one enabled
// webhook subscribed to killSwitchEvent, and returns both ids.
func freshKillSwitchWebhook(
	t *testing.T, ctx context.Context,
	accounts *postgresstore.AccountStore, webhooks *postgresstore.WebhookStore,
	url string,
) (uuid.UUID, uuid.UUID) {
	t.Helper()
	suffix := strings.ToLower(uuid.New().String()[:8])
	acct, err := accounts.Create(ctx, platform.Account{
		Name:         "Kill Switch " + suffix,
		Slug:         "ks-" + suffix,
		BillingEmail: fmt.Sprintf("ks-%s@k.example", suffix),
		Tier:         platform.TierFree,
		Status:       platform.AccountActive,
	})
	if err != nil {
		t.Fatalf("create account: %v", err)
	}
	secret := sha256.Sum256([]byte("kill-switch-signing-material-" + suffix))
	hook, err := webhooks.CreateWebhook(ctx, platform.CustomerWebhook{
		AccountID:  acct.ID,
		Name:       "hook-" + suffix,
		URL:        url,
		SigningKey: secret[:],
		Events:     []string{string(killSwitchEvent)},
		Enabled:    true,
	}, 10)
	if err != nil {
		t.Fatalf("CreateWebhook: %v", err)
	}
	return acct.ID, hook.ID
}

func dueDelivery(webhookID uuid.UUID, tag string) platform.WebhookDelivery {
	return platform.WebhookDelivery{
		WebhookID:     webhookID,
		EventType:     string(killSwitchEvent),
		Payload:       []byte(fmt.Sprintf(`{"incident_id":%q}`, tag)),
		NextAttemptAt: time.Now().UTC().Add(-time.Second),
	}
}

func suspendAccount(t *testing.T, ctx context.Context, accounts *postgresstore.AccountStore, id uuid.UUID) {
	t.Helper()
	if err := accounts.Suspend(ctx, id, "kill-switch test"); err != nil {
		t.Fatalf("Suspend: %v", err)
	}
}

func closeAccount(t *testing.T, ctx context.Context, accounts *postgresstore.AccountStore, id uuid.UUID) {
	t.Helper()
	acct, err := accounts.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get before close: %v", err)
	}
	acct.Status = platform.AccountClosed
	if err := accounts.Update(ctx, acct); err != nil {
		t.Fatalf("close account: %v", err)
	}
}

// A plain FIFO claim lets one endpoint's backlog fill the
// batch and every other customer's events waited behind it (and, with the
// worker serial per endpoint, behind every one of its timeouts). The claim
// now takes each due endpoint's oldest rows first and at most five per
// endpoint per claim. Executes ListPendingDeliveries against real Postgres.
func TestCustomerWebhookClaimIsFairAcrossEndpoints(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	store := postgresstore.New(db)
	accounts := postgresstore.NewAccountStore(store)
	webhooks := postgresstore.NewWebhookStore(store)

	_, backlogged := freshKillSwitchWebhook(t, ctx, accounts, webhooks, "https://fair-backlog.example/hook")
	_, other := freshKillSwitchWebhook(t, ctx, accounts, webhooks, "https://fair-other.example/hook")

	// Nine rows for the backlogged endpoint, all older than the other
	// endpoint's single row: FIFO alone would claim them first.
	for i := range 9 {
		d := dueDelivery(backlogged, fmt.Sprintf("backlog-%d", i))
		d.NextAttemptAt = time.Now().UTC().Add(-time.Duration(20-i) * time.Minute)
		if err := webhooks.EnqueueDelivery(ctx, d); err != nil {
			t.Fatalf("enqueue backlog %d: %v", i, err)
		}
	}
	if err := webhooks.EnqueueDelivery(ctx, dueDelivery(other, "other")); err != nil {
		t.Fatalf("enqueue other: %v", err)
	}

	// A batch smaller than the backlog: FIFO would hand it only the
	// backlogged endpoint's rows.
	first, err := webhooks.ListPendingDeliveries(ctx, 4)
	if err != nil {
		t.Fatalf("first claim: %v", err)
	}
	counts := map[uuid.UUID]int{}
	for _, d := range first {
		counts[d.WebhookID]++
	}
	if counts[other] != 1 || counts[backlogged] != 3 {
		t.Errorf("first claim (limit 4) = %d other + %d backlogged, want 1 + 3", counts[other], counts[backlogged])
	}

	second, err := webhooks.ListPendingDeliveries(ctx, 25)
	if err != nil {
		t.Fatalf("second claim: %v", err)
	}
	if len(second) != 5 {
		t.Errorf("second claim returned %d rows, want 5 (the per-endpoint cap, with 6 still due)", len(second))
	}
	third, err := webhooks.ListPendingDeliveries(ctx, 25)
	if err != nil {
		t.Fatalf("third claim: %v", err)
	}
	if len(third) != 1 {
		t.Errorf("third claim returned %d rows, want the 1 left", len(third))
	}
	fourth, err := webhooks.ListPendingDeliveries(ctx, 25)
	if err != nil {
		t.Fatalf("fourth claim: %v", err)
	}
	if len(fourth) != 0 {
		t.Errorf("fourth claim returned %d rows, want 0 (every row is claimed and leased)", len(fourth))
	}
	seen := map[uuid.UUID]bool{}
	for _, batch := range [][]platform.WebhookDelivery{first, second, third} {
		for _, d := range batch {
			if seen[d.ID] {
				t.Errorf("delivery %s claimed twice while leased", d.ID)
			}
			seen[d.ID] = true
		}
	}
	if len(seen) != 10 {
		t.Errorf("claimed %d distinct rows across all claims, want all 10", len(seen))
	}
}

// EnqueueDelivery must not drop the caller's delivery ID and nothing
// else was unique, so re-running `stellarindex-ops emit-incident` after a
// partial fan-out queued a second copy, under a fresh delivery id, for
// every subscriber the first run had already reached. This executes the
// INSERT … ON CONFLICT path and the fan-out re-run against real Postgres.
func TestCustomerWebhookIdempotentEnqueue(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	store := postgresstore.New(db)
	accounts := postgresstore.NewAccountStore(store)
	webhooks := postgresstore.NewWebhookStore(store)

	t.Run("a repeated delivery id inserts nothing and says so", func(t *testing.T) {
		_, hook := freshKillSwitchWebhook(t, ctx, accounts, webhooks, "https://idem-direct.example/hook")
		_, otherHook := freshKillSwitchWebhook(t, ctx, accounts, webhooks, "https://idem-other.example/hook")
		d := dueDelivery(hook, "idem-direct")
		d.ID = uuid.New()
		if err := webhooks.EnqueueDelivery(ctx, d); err != nil {
			t.Fatalf("first enqueue: %v", err)
		}
		if err := webhooks.EnqueueDelivery(ctx, d); !errors.Is(err, platform.ErrDeliveryAlreadyEnqueued) {
			t.Fatalf("second enqueue err = %v, want ErrDeliveryAlreadyEnqueued", err)
		}
		collide := dueDelivery(otherHook, "idem-collide")
		collide.ID = d.ID
		err := webhooks.EnqueueDelivery(ctx, collide)
		if err == nil || errors.Is(err, platform.ErrDeliveryAlreadyEnqueued) ||
			errors.Is(err, postgresstore.ErrWebhookAccountInactive) {
			t.Fatalf("id reused on another webhook: err = %v, want a collision error", err)
		}
		if got := queuedFor(t, ctx, db, hook); got != 1 {
			t.Errorf("rows for webhook = %d, want 1", got)
		}
		if got := queuedFor(t, ctx, db, otherHook); got != 0 {
			t.Errorf("rows for the colliding webhook = %d, want 0", got)
		}
	})

	t.Run("a zero id still gets a fresh row each time", func(t *testing.T) {
		_, hook := freshKillSwitchWebhook(t, ctx, accounts, webhooks, "https://idem-nil.example/hook")
		for range 2 {
			if err := webhooks.EnqueueDelivery(ctx, dueDelivery(hook, "idem-nil")); err != nil {
				t.Fatalf("enqueue: %v", err)
			}
		}
		if got := queuedFor(t, ctx, db, hook); got != 2 {
			t.Errorf("rows = %d, want 2", got)
		}
	})

	t.Run("a fan-out re-run reaches only the subscriber it missed", func(t *testing.T) {
		assertFanoutRerunIsIdempotent(t, ctx, db, accounts, webhooks)
	})
}

func assertFanoutRerunIsIdempotent(
	t *testing.T, ctx context.Context, db *sql.DB,
	accounts *postgresstore.AccountStore, webhooks *postgresstore.WebhookStore,
) {
	t.Helper()
	// Only this sub-test's hooks may be subscribed to the event, so give
	// every earlier hook a different event.
	if _, err := db.ExecContext(ctx, `UPDATE customer_webhooks SET events = ARRAY['price.alert']`); err != nil {
		t.Fatalf("unsubscribe earlier hooks: %v", err)
	}
	_, reached := freshKillSwitchWebhook(t, ctx, accounts, webhooks, "https://idem-reached.example/hook")
	_, missed := freshKillSwitchWebhook(t, ctx, accounts, webhooks, "https://idem-missed.example/hook")

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	const key = "idem-incident@2026-09-01T10:00:00Z"
	flaky := &failOnceFor{WebhookStore: webhooks, webhookID: missed}
	first, err := customerwebhook.NewFanout(flaky, logger).
		PublishOnce(ctx, killSwitchEvent, key, []byte(`{"at":"run-1"}`))
	if err == nil || first.Enqueued != 1 || first.Failed != 1 {
		t.Fatalf("first run = %+v, %v; want Enqueued 1, Failed 1 and an error", first, err)
	}
	second, err := customerwebhook.NewFanout(webhooks, logger).
		PublishOnce(ctx, killSwitchEvent, key, []byte(`{"at":"run-2"}`))
	if err != nil {
		t.Fatalf("re-run: %v", err)
	}
	if second.Enqueued != 1 || second.AlreadyEnqueued != 1 || second.Failed != 0 {
		t.Errorf("re-run = %+v, want {Enqueued:1 AlreadyEnqueued:1 Failed:0}", second)
	}
	for name, hook := range map[string]uuid.UUID{"reached": reached, "missed": missed} {
		if got := queuedFor(t, ctx, db, hook); got != 1 {
			t.Errorf("%s webhook has %d rows, want exactly 1", name, got)
		}
	}
}

// failOnceFor fails the first EnqueueDelivery for one webhook with a
// transient-looking error, as a dropped connection mid-fan-out would.
type failOnceFor struct {
	*postgresstore.WebhookStore
	webhookID uuid.UUID
	failed    bool
}

func (f *failOnceFor) EnqueueDelivery(ctx context.Context, d platform.WebhookDelivery) error {
	if d.WebhookID == f.webhookID && !f.failed {
		f.failed = true
		return errors.New("injected: connection reset")
	}
	return f.WebhookStore.EnqueueDelivery(ctx, d)
}

func queuedFor(t *testing.T, ctx context.Context, db *sql.DB, webhookID uuid.UUID) int {
	t.Helper()
	var n int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM webhook_deliveries WHERE webhook_id = $1`, webhookID).Scan(&n); err != nil {
		t.Fatalf("count deliveries: %v", err)
	}
	return n
}

// TestCustomerWebhookRotateSecretKeepsQueue executes migration 0200 up and
// down and the in-place rotation it backs: the key changes on the
// SAME row, the outgoing key is kept until its expiry, and the webhook's
// queued deliveries survive — the delete + recreate it replaces cascaded
// them away.
func TestCustomerWebhookRotateSecretKeepsQueue(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrationsUpTo(t, dsn, 199)
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	acct := insertRawAccount(t, ctx, db, "rotate")
	hookID := insertRawWebhook(t, ctx, db, acct, "https://hooks.example/rotate")
	applyMigrations(t, dsn)

	webhooks := postgresstore.NewWebhookStore(postgresstore.New(db))
	before, err := webhooks.GetWebhook(ctx, hookID)
	if err != nil {
		t.Fatalf("get pre-0200 webhook: %v", err)
	}
	if before.PreviousSigningKey != nil || !before.PreviousSecretExpiresAt.IsZero() {
		t.Fatalf("pre-0200 webhook reads previous = (%x, %v), want none", before.PreviousSigningKey, before.PreviousSecretExpiresAt)
	}
	oldKey := before.SigningKey

	queued := uuid.New()
	if err := webhooks.EnqueueDelivery(ctx, platform.WebhookDelivery{
		ID: queued, WebhookID: hookID, EventType: string(platform.WebhookEventIncidentSEV1),
	}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	newKey := []byte("wsec_rotated")
	expiry := time.Now().Add(24 * time.Hour).UTC().Truncate(time.Microsecond)
	if err := webhooks.RotateWebhookSecret(ctx, hookID, newKey, expiry); err != nil {
		t.Fatalf("rotate: %v", err)
	}
	after, err := webhooks.GetWebhook(ctx, hookID)
	if err != nil {
		t.Fatalf("get after rotate: %v", err)
	}
	if string(after.SigningKey) != string(newKey) {
		t.Errorf("secret_hash = %q, want the new key", after.SigningKey)
	}
	if string(after.PreviousSigningKey) != string(oldKey) || !after.PreviousSecretExpiresAt.Equal(expiry) {
		t.Errorf("previous = (%x, %v), want the old key %x until %v", after.PreviousSigningKey, after.PreviousSecretExpiresAt, oldKey, expiry)
	}
	deliveries, err := webhooks.ListDeliveries(ctx, hookID, 10)
	if err != nil {
		t.Fatalf("list deliveries: %v", err)
	}
	if len(deliveries) != 1 || deliveries[0].ID != queued || deliveries[0].IsTerminal() {
		t.Errorf("deliveries after rotate = %+v, want the one queued row still pending", deliveries)
	}

	// A second rotation keeps only the most recent outgoing key.
	if err := webhooks.RotateWebhookSecret(ctx, hookID, []byte("wsec_third"), expiry); err != nil {
		t.Fatalf("second rotate: %v", err)
	}
	if got, _ := webhooks.GetWebhook(ctx, hookID); string(got.PreviousSigningKey) != string(newKey) {
		t.Errorf("previous after second rotate = %q, want %q", got.PreviousSigningKey, newKey)
	}

	if err := webhooks.RotateWebhookSecret(ctx, uuid.New(), newKey, expiry); !errors.Is(err, platform.ErrNotFound) {
		t.Errorf("rotate of a missing webhook: err = %v, want ErrNotFound", err)
	}
	if _, err := db.ExecContext(ctx,
		`UPDATE customer_webhooks SET previous_secret_expires_at = NULL WHERE id = $1`, hookID); err == nil {
		t.Error("previous_secret without an expiry was accepted, want the pair CHECK to refuse it")
	}

	applyMigrationsUpTo(t, dsn, 199)
	var cols int
	if err := db.QueryRowContext(ctx, `
		SELECT count(*) FROM information_schema.columns
		 WHERE table_name = 'customer_webhooks'
		   AND column_name IN ('previous_secret', 'previous_secret_expires_at')`).Scan(&cols); err != nil {
		t.Fatalf("read columns: %v", err)
	}
	if cols != 0 {
		t.Errorf("0200 down left %d previous_secret columns in place", cols)
	}
	applyMigrations(t, dsn)
}
