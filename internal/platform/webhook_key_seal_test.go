package platform_test

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/Stellar-Index/StellarIndex/internal/platform"
)

func TestWebhookKeySealer(t *testing.T) {
	secret := []byte(strings.Repeat("s", platform.MinWebhookSealSecretLen))
	sealer, err := platform.NewWebhookKeySealer(secret)
	if err != nil {
		t.Fatalf("NewWebhookKeySealer: %v", err)
	}
	id := uuid.New()
	key := []byte("whsec_0123456789abcdef0123456789abcdef")

	sealed := sealer.Seal(id, key)
	if bytes.Contains(sealed, key) {
		t.Fatal("sealed bytes contain the plaintext key")
	}
	if again := sealer.Seal(id, key); bytes.Equal(again, sealed) {
		t.Error("two seals of one key are identical — the nonce is not random")
	}
	got, err := sealer.Open(id, sealed)
	if err != nil || !bytes.Equal(got, key) {
		t.Fatalf("Open = (%q, %v), want the original key", got, err)
	}

	other, err := platform.NewWebhookKeySealer(append(secret, 'x'))
	if err != nil {
		t.Fatal(err)
	}
	tampered := append([]byte(nil), sealed...)
	tampered[len(tampered)-1] ^= 1
	for name, open := range map[string]func() ([]byte, error){
		"another webhook id": func() ([]byte, error) { return sealer.Open(uuid.New(), sealed) },
		"another secret":     func() ([]byte, error) { return other.Open(id, sealed) },
		"flipped bit":        func() ([]byte, error) { return sealer.Open(id, tampered) },
		"raw legacy key":     func() ([]byte, error) { return sealer.Open(id, key) },
		"empty":              func() ([]byte, error) { return sealer.Open(id, nil) },
	} {
		if k, err := open(); !errors.Is(err, platform.ErrWebhookKeyUnsealable) || k != nil {
			t.Errorf("%s: Open = (%q, %v), want ErrWebhookKeyUnsealable", name, k, err)
		}
	}

	if _, err := platform.NewWebhookKeySealer(secret[1:]); err == nil {
		t.Errorf("a %d-byte seal secret was accepted", len(secret)-1)
	}
}
