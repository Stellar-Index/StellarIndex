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

// ErrWebhookKeyUnsealable is returned when a stored signing key cannot
// be opened: no seal key is configured, or the sealed bytes fail
// authentication under the configured one.
var ErrWebhookKeyUnsealable = errors.New("platform: webhook signing key cannot be unsealed")

// ErrWebhookSealKeyMissing is the [ErrWebhookKeyUnsealable] case where no
// seal key is configured: setting one can still open the key, whereas an
// authentication failure under a configured key never will.
var ErrWebhookSealKeyMissing = fmt.Errorf("%w: no seal key is configured", ErrWebhookKeyUnsealable)

// WebhookKeySealer seals customer-webhook signing keys at rest with
// AES-256-GCM. The webhook id is the associated data, so a sealed key
// copied onto another row does not open there.
type WebhookKeySealer struct{ aead cipher.AEAD }

// NewWebhookKeySealer derives the sealing key from secret, which must
// be at least [MinWebhookSealSecretLen] bytes.
func NewWebhookKeySealer(secret []byte) (*WebhookKeySealer, error) {
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
	return &WebhookKeySealer{aead: aead}, nil
}

// Seal returns the at-rest form of signingKey for webhook id.
func (s *WebhookKeySealer) Seal(id uuid.UUID, signingKey []byte) []byte {
	out := make([]byte, 1, 1+len(signingKey)+s.aead.Overhead())
	out[0] = webhookKeySealVersion
	return s.aead.Seal(out, nil, signingKey, id[:])
}

// Open reverses [WebhookKeySealer.Seal] for the same webhook id.
func (s *WebhookKeySealer) Open(id uuid.UUID, sealed []byte) ([]byte, error) {
	if len(sealed) < 1+s.aead.Overhead() || sealed[0] != webhookKeySealVersion {
		return nil, fmt.Errorf("webhook %s: unrecognised sealed key format: %w", id, ErrWebhookKeyUnsealable)
	}
	key, err := s.aead.Open(nil, nil, sealed[1:], id[:])
	if err != nil {
		return nil, fmt.Errorf("webhook %s: %w", id, ErrWebhookKeyUnsealable)
	}
	return key, nil
}
