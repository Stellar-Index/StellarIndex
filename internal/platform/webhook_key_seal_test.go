package platform_test

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"

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

// TestWebhookKeySealer_Rotation walks a seal-secret rotation A→B and its
// rollback: a key sealed under either secret keeps opening while both are
// configured, and is re-sealed so the new secret alone opens it.
func TestWebhookKeySealer_Rotation(t *testing.T) {
	mk := func(c string, previous ...string) *platform.WebhookKeySealer {
		s, err := platform.NewWebhookKeySealer([]byte(strings.Repeat(c, platform.MinWebhookSealSecretLen)))
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range previous {
			if s, err = s.WithPrevious([]byte(strings.Repeat(p, platform.MinWebhookSealSecretLen))); err != nil {
				t.Fatal(err)
			}
		}
		return s
	}
	id := uuid.New()
	key := []byte("whsec_rotation_0123456789abcdef")
	underA := mk("a").Seal(id, key)

	rotating := mk("b", "a")
	if !rotating.Rotating() || mk("b").Rotating() {
		t.Fatal("Rotating() does not report the previous secret")
	}
	if got, err := rotating.Open(id, underA); err != nil || !bytes.Equal(got, key) {
		t.Fatalf("current=B previous=A: Open = (%q, %v), want the key", got, err)
	}
	underB, err := rotating.Reseal(id, underA)
	if err != nil || underB == nil {
		t.Fatalf("Reseal of an A-sealed key = (%x, %v), want fresh bytes", underB, err)
	}
	if again, err := rotating.Reseal(id, underB); err != nil || again != nil {
		t.Errorf("Reseal of a B-sealed key = (%x, %v), want nil: it is already current", again, err)
	}
	if got, err := mk("b").Open(id, underB); err != nil || !bytes.Equal(got, key) {
		t.Fatalf("current=B alone on the re-sealed key: Open = (%q, %v), want the key", got, err)
	}

	_, err = mk("b").Open(id, underA)
	if !errors.Is(err, platform.ErrWebhookKeyUnsealable) || errors.Is(err, platform.ErrWebhookSealKeyRotating) {
		t.Errorf("current=B alone on an A-sealed key: err = %v, want ErrWebhookKeyUnsealable, not rotating", err)
	}
	if err != nil && strings.Contains(err.Error(), string(key)) {
		t.Error("the unseal error carries the clear key")
	}
	if _, err := mk("b", "c").Open(id, underA); !errors.Is(err, platform.ErrWebhookSealKeyRotating) {
		t.Errorf("an A-sealed key under current=B previous=C: err = %v, want ErrWebhookSealKeyRotating", err)
	}

	rollback := mk("a", "b")
	for name, sealed := range map[string][]byte{"B-sealed": underB, "A-sealed": underA} {
		if got, err := rollback.Open(id, sealed); err != nil || !bytes.Equal(got, key) {
			t.Errorf("rollback current=A previous=B, %s: Open = (%q, %v), want the key", name, got, err)
		}
	}
	if _, err := mk("b").Open(id, rollback.Seal(id, key)); err == nil {
		t.Error("a seal by current=A previous=B opens under B: it sealed with the previous secret")
	}
}

// TestActivePreviousSigningKey pins the overlap window the worker signs
// the outgoing key through: open strictly before the expiry, shut at it.
func TestActivePreviousSigningKey(t *testing.T) {
	expiry := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	prev := []byte("whsec_previous")
	w := platform.CustomerWebhook{PreviousSigningKey: prev, PreviousSecretExpiresAt: expiry}
	if got := w.ActivePreviousSigningKey(expiry.Add(-time.Second)); !bytes.Equal(got, prev) {
		t.Errorf("inside the overlap = %q, want %q", got, prev)
	}
	if got := w.ActivePreviousSigningKey(expiry); got != nil {
		t.Errorf("at the expiry = %q, want nil", got)
	}
	if got := (platform.CustomerWebhook{PreviousSecretExpiresAt: expiry}).ActivePreviousSigningKey(expiry.Add(-time.Hour)); got != nil {
		t.Errorf("no previous key = %q, want nil", got)
	}
}
