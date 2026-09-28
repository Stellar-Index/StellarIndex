package dashboardauth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/notify"
	"github.com/Stellar-Index/StellarIndex/internal/platform"
)

// rootKeyed is what each MAC would be if it were keyed by the root secret
// directly: domain || parts under HMAC(root).
func rootKeyed(root []byte, domain string, parts ...string) []byte {
	m := hmac.New(sha256.New, root)
	m.Write([]byte(domain))
	for _, p := range parts {
		m.Write([]byte(p))
	}
	return m.Sum(nil)
}

// No consumer may MAC under the root secret itself: each derives its own
// key, so the four purposes share no key material.
func TestServerSecretConsumersUseDerivedKeys(t *testing.T) {
	root := []byte("purpose-key-test-root-secret-0123456789")
	hash := sha256.Sum256([]byte("token"))

	codeRoot := rootKeyed(root, loginCodeDomain, string(hash[:]))
	if codeFromHashKeyed(root, hash[:]) == fmt.Sprintf("%06d", binary.BigEndian.Uint32(codeRoot[:4])%1_000_000) {
		t.Error("login code is keyed by the root secret, not a derived key")
	}

	payload := `{"purpose":"login"}`
	if hmac.Equal(passkeyCeremonyMAC(root, []byte(payload)), rootKeyed(root, passkeyCeremonyDomain, payload)) {
		t.Error("passkey ceremony cookie is keyed by the root secret, not a derived key")
	}

	nonce := strings.Repeat("0a", MagicLinkPlaintextLen/2)
	browser := strings.Repeat("1b", MagicLinkPlaintextLen)
	intentRoot := hex.EncodeToString(rootKeyed(root, loginIntentDomain, nonce, "|", browser)[:MagicLinkPlaintextLen/2])
	if loginIntentTag(root, nonce, browser) == intentRoot {
		t.Error("login-intent tag is keyed by the root secret, not a derived key")
	}

	const email, expires = "a@example.com", int64(1_900_000_000)
	deviceRoot := hex.EncodeToString(rootKeyed(root, loginDeviceDomain, strconv.FormatInt(expires, 10), "|", email))
	if loginDeviceMAC(root, email, expires) == deviceRoot {
		t.Error("login-device marker is keyed by the root secret, not a derived key")
	}

	seen := map[string]string{}
	for _, label := range []string{loginCodeDomain, passkeyCeremonyDomain, loginIntentDomain, loginDeviceDomain} {
		k, err := purposeKey(root, label)
		if err != nil {
			t.Fatalf("purposeKey(%q): %v", label, err)
		}
		if hmac.Equal(k, root) {
			t.Errorf("%q key equals the root secret", label)
		}
		if prev, dup := seen[string(k)]; dup {
			t.Errorf("%q and %q derive the same key", prev, label)
		}
		seen[string(k)] = label
	}
}

// A per-process fallback secret breaks every passkey ceremony that crosses
// instances or a restart with a 400 indistinguishable from tampering, so
// wiring passkeys without a configured secret must fail at construction.
func TestNewHandlers_PasskeysRequireConfiguredSecret(t *testing.T) {
	base := func() Config {
		return Config{
			Accounts:         newFakeAccountStore(),
			Users:            newFakeUserStore(),
			Tokens:           struct{ platform.TokenStore }{},
			Sender:           &notify.NoopSender{},
			Logger:           slog.New(slog.NewTextHandler(io.Discard, nil)),
			DashboardBaseURL: "https://app.stellarindex.io",
			EmailFrom:        "Stellar Index <hello@stellarindex.io>",
		}
	}

	cfg := base()
	cfg.Passkeys = struct {
		platform.WebAuthnCredentialStore
	}{}
	if _, err := NewHandlers(&cfg); err == nil {
		t.Fatal("NewHandlers accepted passkeys with no server secret")
	}

	cfg = base()
	cfg.Passkeys = struct {
		platform.WebAuthnCredentialStore
	}{}
	cfg.Generator = &Generator{Read: NewGenerator().Read, Secret: []byte("configured-secret")}
	if _, err := NewHandlers(&cfg); err != nil {
		t.Fatalf("NewHandlers with passkeys and a secret: %v", err)
	}

	cfg = base()
	if _, err := NewHandlers(&cfg); err != nil {
		t.Fatalf("NewHandlers without passkeys must keep the per-process fallback: %v", err)
	}
	if len(cfg.Generator.Secret) == 0 {
		t.Fatal("fallback secret was not installed")
	}
}
