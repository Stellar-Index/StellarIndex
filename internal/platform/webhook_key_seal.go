package platform

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/sha256"
	"errors"
	"fmt"

	"github.com/google/uuid"
)

// MinWebhookSealSecretLen is the shortest seal secret [NewWebhookKeySealer] accepts.
const MinWebhookSealSecretLen = 32

// webhookKeySealVersion prefixes every sealed key so a later key or
// cipher change can be told apart from this one on read.
const webhookKeySealVersion byte = 1

const webhookKeySealLabel = "stellarindex customer-webhook signing-key seal v1"

// ErrWebhookKeyUnsealable is returned when a stored signing key cannot be opened: no seal key is configured or the sealed bytes fail authentication.
var ErrWebhookKeyUnsealable = errors.New("platform: webhook signing key cannot be unsealed")

// ErrWebhookSealKeyMissing is the [ErrWebhookKeyUnsealable] case where no seal key is configured; setting one can still open the key, an authentication failure never will.
var ErrWebhookSealKeyMissing = fmt.Errorf("%w: no seal key is configured", ErrWebhookKeyUnsealable)

// ErrWebhookSealKeyRotating is the [ErrWebhookKeyUnsealable] case where a previous seal key is configured beside the current one and neither opens the key; the rotation is unfinished, so it is not proof the key is lost.
var ErrWebhookSealKeyRotating = fmt.Errorf("%w: under neither the current nor the previous seal key", ErrWebhookKeyUnsealable)

// WebhookKeySealer seals customer-webhook signing keys at rest with AES-256-GCM, using the webhook id as associated data so a sealed key copied onto another row does not open.
// A sealer built [WebhookKeySealer.WithPrevious] also opens keys sealed under the previous secret but never seals with it.
type WebhookKeySealer struct{ aead, previous cipher.AEAD }

// NewWebhookKeySealer derives the sealing key from secret, which must be at least [MinWebhookSealSecretLen] bytes.
func NewWebhookKeySealer(secret []byte) (*WebhookKeySealer, error) {
	aead, err := newWebhookSealAEAD(secret)
	if err != nil {
		return nil, err
	}
	return &WebhookKeySealer{aead: aead}, nil
}

// WithPrevious returns a sealer that seals under s's secret and opens under it or, failing that, under previous.
func (s *WebhookKeySealer) WithPrevious(previous []byte) (*WebhookKeySealer, error) {
	aead, err := newWebhookSealAEAD(previous)
	if err != nil {
		return nil, fmt.Errorf("previous: %w", err)
	}
	return &WebhookKeySealer{aead: s.aead, previous: aead}, nil
}

// Rotating reports whether a previous seal secret is configured.
func (s *WebhookKeySealer) Rotating() bool { return s.previous != nil }

func newWebhookSealAEAD(secret []byte) (cipher.AEAD, error) {
	if len(secret) < MinWebhookSealSecretLen {
		return nil, fmt.Errorf("platform: webhook seal secret is %d bytes, want at least %d",
			len(secret), MinWebhookSealSecretLen)
	}
	key, err := hkdf.Key(sha256.New, secret, nil, webhookKeySealLabel, 32)
	if err != nil {
		return nil, fmt.Errorf("platform: derive webhook seal key: %w", err)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("platform: webhook seal cipher: %w", err)
	}
	aead, err := cipher.NewGCMWithRandomNonce(block)
	if err != nil {
		return nil, fmt.Errorf("platform: webhook seal AEAD: %w", err)
	}
	return aead, nil
}

// Seal returns the at-rest form of signingKey for webhook id.
func (s *WebhookKeySealer) Seal(id uuid.UUID, signingKey []byte) []byte {
	out := make([]byte, 1, 1+len(signingKey)+s.aead.Overhead())
	out[0] = webhookKeySealVersion
	return s.aead.Seal(out, nil, signingKey, id[:]) //nolint:gosec // G407: NewGCMWithRandomNonce requires a nil nonce and prepends a fresh random one per Seal
}

// Open reverses [WebhookKeySealer.Seal] for the same webhook id, under the current secret or else the previous one.
func (s *WebhookKeySealer) Open(id uuid.UUID, sealed []byte) ([]byte, error) {
	key, _, err := s.open(id, sealed)
	return key, err
}

// Reseal returns sealed re-sealed under the current secret when only the previous secret opens it, and nil when it is already current.
func (s *WebhookKeySealer) Reseal(id uuid.UUID, sealed []byte) ([]byte, error) {
	key, stale, err := s.open(id, sealed)
	if err != nil || !stale {
		return nil, err
	}
	return s.Seal(id, key), nil
}

func (s *WebhookKeySealer) open(id uuid.UUID, sealed []byte) (key []byte, stale bool, err error) {
	if len(sealed) < 1+s.aead.Overhead() || sealed[0] != webhookKeySealVersion {
		return nil, false, fmt.Errorf("webhook %s: unrecognised sealed key format: %w", id, ErrWebhookKeyUnsealable)
	}
	if key, err := s.aead.Open(nil, nil, sealed[1:], id[:]); err == nil {
		return key, false, nil
	}
	if s.previous == nil {
		return nil, false, fmt.Errorf("webhook %s: %w", id, ErrWebhookKeyUnsealable)
	}
	if key, err := s.previous.Open(nil, nil, sealed[1:], id[:]); err == nil {
		return key, true, nil
	}
	return nil, false, fmt.Errorf("webhook %s: %w", id, ErrWebhookSealKeyRotating)
}
