package platform

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// WebAuthnCredential is one registered passkey (WebAuthn public-key
// credential) for a dashboard user. The private key never leaves the
// user's authenticator; we store only the public key + the metadata
// the WebAuthn verification algorithm needs (sign count for clone
// detection, transports for allow-list hints, backup flags for
// risk display). Rows are user-scoped — deleting the user cascades.
type WebAuthnCredential struct {
	ID     uuid.UUID
	UserID uuid.UUID
	// Name is the user-chosen label ("MacBook Touch ID", "YubiKey").
	// Display-only; never part of any authentication decision.
	Name string
	// CredentialID is the authenticator-generated credential ID —
	// the lookup key during a login assertion. Globally unique per
	// the WebAuthn spec; enforced unique in storage.
	CredentialID []byte
	// PublicKey is the COSE-encoded credential public key.
	PublicKey []byte
	// AttestationType as conveyed at registration ("none" for
	// browser-mediated passkeys, typically).
	AttestationType string
	// Transports the authenticator reported (usb / nfc / ble /
	// internal / hybrid). Hints only.
	Transports []string
	// SignCount is the stored signature counter. Compared against
	// the assertion's counter on every login; a regression signals
	// a cloned authenticator. Many passkey providers (iCloud
	// Keychain) always report 0 — that is spec-legal.
	SignCount int64
	// BackupEligible / BackupState mirror the BE/BS authenticator
	// flags — whether the credential is synced (multi-device).
	BackupEligible bool
	BackupState    bool
	// AAGUID identifies the authenticator model. Zero UUID for
	// anonymized attestation.
	AAGUID     []byte
	CreatedAt  time.Time
	LastUsedAt time.Time // zero = never used for a login
}

// WebAuthnCredentialStore persists [WebAuthnCredential] rows.
// Implemented by postgresstore (webauthn_credentials, migration
// 0140) and by in-memory fakes in tests.
type WebAuthnCredentialStore interface {
	// CreateWebAuthnCredential inserts and returns the stored row
	// (ID + CreatedAt populated). ErrConflict if the credential ID
	// is already registered (spec-required global uniqueness).
	CreateWebAuthnCredential(ctx context.Context, c WebAuthnCredential) (WebAuthnCredential, error)

	// ListWebAuthnCredentialsForUser returns the user's passkeys,
	// newest first. Empty slice (not an error) when none.
	ListWebAuthnCredentialsForUser(ctx context.Context, userID uuid.UUID) ([]WebAuthnCredential, error)

	// GetWebAuthnCredentialByCredentialID looks up by the
	// authenticator-generated credential ID (the login-assertion
	// key). ErrNotFound when absent.
	GetWebAuthnCredentialByCredentialID(ctx context.Context, credentialID []byte) (WebAuthnCredential, error)

	// UpdateWebAuthnCredentialSignCount stores the post-assertion
	// signature counter + last-used timestamp after a successful
	// login. ErrNotFound when the row is gone.
	UpdateWebAuthnCredentialSignCount(ctx context.Context, id uuid.UUID, signCount int64, lastUsedAt time.Time) error

	// DeleteWebAuthnCredential removes a passkey. Scoped to its
	// owner: a row that exists but belongs to another user is
	// ErrNotFound (no cross-user probe oracle).
	DeleteWebAuthnCredential(ctx context.Context, id, userID uuid.UUID) error
}
