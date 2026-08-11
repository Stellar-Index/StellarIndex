package postgresstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/lib/pq"

	"github.com/Stellar-Index/StellarIndex/internal/platform"
)

// WebAuthnCredentialStore is the Postgres impl of
// [platform.WebAuthnCredentialStore] (webauthn_credentials,
// migration 0140).
type WebAuthnCredentialStore struct{ s *Store }

// NewWebAuthnCredentialStore wraps the shared handle.
func NewWebAuthnCredentialStore(s *Store) *WebAuthnCredentialStore {
	return &WebAuthnCredentialStore{s: s}
}

const webauthnCredentialCols = `id, user_id, name, credential_id, public_key,
	attestation_type, transports, sign_count, backup_eligible, backup_state,
	aaguid, created_at, last_used_at`

func scanWebAuthnCredential(row interface{ Scan(...any) error }) (platform.WebAuthnCredential, error) {
	var (
		c          platform.WebAuthnCredential
		transports pq.StringArray
		aaguid     []byte
		lastUsed   sql.NullTime
	)
	err := row.Scan(
		&c.ID, &c.UserID, &c.Name, &c.CredentialID, &c.PublicKey,
		&c.AttestationType, &transports, &c.SignCount, &c.BackupEligible,
		&c.BackupState, &aaguid, &c.CreatedAt, &lastUsed,
	)
	if err != nil {
		return platform.WebAuthnCredential{}, err
	}
	c.Transports = []string(transports)
	c.AAGUID = aaguid
	if lastUsed.Valid {
		c.LastUsedAt = lastUsed.Time
	}
	return c, nil
}

func (r *WebAuthnCredentialStore) CreateWebAuthnCredential(
	ctx context.Context, c platform.WebAuthnCredential,
) (platform.WebAuthnCredential, error) {
	row := r.s.db.QueryRowContext(ctx, `
		INSERT INTO webauthn_credentials
			(user_id, name, credential_id, public_key, attestation_type,
			 transports, sign_count, backup_eligible, backup_state, aaguid)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		RETURNING `+webauthnCredentialCols,
		c.UserID, c.Name, c.CredentialID, c.PublicKey, c.AttestationType,
		pq.Array(c.Transports), c.SignCount, c.BackupEligible, c.BackupState,
		c.AAGUID,
	)
	created, err := scanWebAuthnCredential(row)
	if err != nil {
		var pqErr *pq.Error
		if errors.As(err, &pqErr) && pqErr.Code == pgErrUniqueViolation {
			return platform.WebAuthnCredential{}, fmt.Errorf("create webauthn credential: %w", platform.ErrConflict)
		}
		return platform.WebAuthnCredential{}, fmt.Errorf("create webauthn credential: %w", err)
	}
	return created, nil
}

func (r *WebAuthnCredentialStore) ListWebAuthnCredentialsForUser(
	ctx context.Context, userID uuid.UUID,
) ([]platform.WebAuthnCredential, error) {
	rows, err := r.s.db.QueryContext(ctx, `
		SELECT `+webauthnCredentialCols+`
		FROM webauthn_credentials
		WHERE user_id = $1
		ORDER BY created_at DESC, id DESC`, userID)
	if err != nil {
		return nil, fmt.Errorf("list webauthn credentials: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := []platform.WebAuthnCredential{}
	for rows.Next() {
		c, err := scanWebAuthnCredential(rows)
		if err != nil {
			return nil, fmt.Errorf("list webauthn credentials scan: %w", err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list webauthn credentials rows: %w", err)
	}
	return out, nil
}

func (r *WebAuthnCredentialStore) GetWebAuthnCredentialByCredentialID(
	ctx context.Context, credentialID []byte,
) (platform.WebAuthnCredential, error) {
	row := r.s.db.QueryRowContext(ctx, `
		SELECT `+webauthnCredentialCols+`
		FROM webauthn_credentials
		WHERE credential_id = $1`, credentialID)
	c, err := scanWebAuthnCredential(row)
	if errors.Is(err, sql.ErrNoRows) {
		return platform.WebAuthnCredential{}, fmt.Errorf("get webauthn credential: %w", platform.ErrNotFound)
	}
	if err != nil {
		return platform.WebAuthnCredential{}, fmt.Errorf("get webauthn credential: %w", err)
	}
	return c, nil
}

func (r *WebAuthnCredentialStore) UpdateWebAuthnCredentialSignCount(
	ctx context.Context, id uuid.UUID, signCount int64, lastUsedAt time.Time,
) error {
	res, err := r.s.db.ExecContext(ctx, `
		UPDATE webauthn_credentials
		SET sign_count = $2, last_used_at = $3
		WHERE id = $1`, id, signCount, lastUsedAt)
	if err != nil {
		return fmt.Errorf("update webauthn sign count: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("update webauthn sign count: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("update webauthn sign count: %w", platform.ErrNotFound)
	}
	return nil
}

func (r *WebAuthnCredentialStore) DeleteWebAuthnCredential(
	ctx context.Context, id, userID uuid.UUID,
) error {
	res, err := r.s.db.ExecContext(ctx, `
		DELETE FROM webauthn_credentials
		WHERE id = $1 AND user_id = $2`, id, userID)
	if err != nil {
		return fmt.Errorf("delete webauthn credential: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("delete webauthn credential: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("delete webauthn credential: %w", platform.ErrNotFound)
	}
	return nil
}
