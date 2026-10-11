package platform

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// WebAuthnCredential is one registered passkey for a dashboard user; deleting the user cascades.
type WebAuthnCredential struct {
	ID     uuid.UUID
	UserID uuid.UUID
	// Name is the user-chosen label ("MacBook Touch ID", "YubiKey").
	// Display-only; never part of any authentication decision.
	Name string
	// CredentialID is the login-assertion lookup key; globally unique per the WebAuthn spec and
	// enforced unique in storage.
	CredentialID []byte
	// PublicKey is the COSE-encoded credential public key.
	PublicKey []byte
	// AttestationType as conveyed at registration ("none" for browser-mediated passkeys).
	AttestationType string
	// Transports the authenticator reported (usb, nfc, ble, internal, hybrid). Hints only.
	Transports []string
	// SignCount is compared against the assertion's counter on every login; a regression signals
	// a cloned authenticator. Providers such as iCloud Keychain always report 0, which is
	// spec-legal.
	SignCount int64
	// BackupEligible and BackupState mirror the BE/BS flags: whether the credential is synced.
	BackupEligible bool
	BackupState    bool
	// AAGUID identifies the authenticator model; the zero UUID for anonymized attestation.
	AAGUID     []byte
	CreatedAt  time.Time
	LastUsedAt time.Time // zero = never used for a login
}

// WebAuthnCredentialStore persists [WebAuthnCredential] rows; postgresstore implements it.
type WebAuthnCredentialStore interface {
	// CreateWebAuthnCredential inserts and returns the stored row. ErrConflict if the credential
	// ID is already registered (the spec requires global uniqueness).
	CreateWebAuthnCredential(ctx context.Context, c WebAuthnCredential) (WebAuthnCredential, error)

	// ListWebAuthnCredentialsForUser returns the user's passkeys, newest first; empty, not an
	// error, when none.
	ListWebAuthnCredentialsForUser(ctx context.Context, userID uuid.UUID) ([]WebAuthnCredential, error)

	// GetWebAuthnCredentialByCredentialID looks up by the login-assertion key; ErrNotFound when
	// absent.
	GetWebAuthnCredentialByCredentialID(ctx context.Context, credentialID []byte) (WebAuthnCredential, error)

	// UpdateWebAuthnCredentialSignCount stores the post-login counter and last-used time; the
	// stored count is a high-water mark and never decreases. ErrNotFound when the row is gone.
	UpdateWebAuthnCredentialSignCount(ctx context.Context, id uuid.UUID, signCount int64, lastUsedAt time.Time) error

	// DeleteWebAuthnCredential removes a passkey. A row owned by another user is ErrNotFound, so
	// there is no cross-user probe oracle.
	DeleteWebAuthnCredential(ctx context.Context, id, userID uuid.UUID) error
}
