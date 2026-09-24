package sep10_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"testing"

	"github.com/stellar/go-stellar-sdk/keypair"
	"github.com/stellar/go-stellar-sdk/network"
	"github.com/stellar/go-stellar-sdk/txnbuild"

	"github.com/Stellar-Index/StellarIndex/internal/auth"
	"github.com/Stellar-Index/StellarIndex/internal/auth/sep10"
)

// TestVerifyJWT_RejectsOtherNetworksToken pins the network binding: two
// deployments sharing home_domain and jwt_secret but on different Stellar
// networks must not accept each other's tokens.
func TestVerifyJWT_RejectsOtherNetworksToken(t *testing.T) {
	testnet, _, clk := newTestValidator(t)
	server, _ := keypair.Random()
	pubnet, err := sep10.NewValidator(sep10.Options{
		ServerSeed:        server.Seed(),
		NetworkPassphrase: network.PublicNetworkPassphrase,
		WebAuthDomain:     testWebDomain,
		HomeDomain:        testHomeDomain,
		JWTSecret:         testJWTSecret,
		AccountLoader:     fakeAccounts{},
		Now:               clk.Now, // same clock, so only the network differs
	})
	if err != nil {
		t.Fatal(err)
	}

	client, _ := keypair.Random()
	ch, err := pubnet.Challenge(context.Background(), client.Address())
	if err != nil {
		t.Fatal(err)
	}
	tok, err := pubnet.Verify(context.Background(), signPubnetChallenge(t, ch.TransactionXDR, client))
	if err != nil {
		t.Fatalf("pubnet Verify: %v", err)
	}
	if _, err := pubnet.VerifyJWT(context.Background(), tok.JWT); err != nil {
		t.Fatalf("pubnet rejected its own token: %v", err)
	}
	if _, err := testnet.VerifyJWT(context.Background(), tok.JWT); !errors.Is(err, auth.ErrUnauthorized) {
		t.Errorf("testnet validator accepted a pubnet token; err = %v, want wrap of ErrUnauthorized", err)
	}
}

// TestVerifyJWT_RejectsTokenWithoutNetworkClaim — a correctly signed token
// that carries no network claim is refused (fail closed), while the same
// token carrying this validator's network id verifies.
func TestVerifyJWT_RejectsTokenWithoutNetworkClaim(t *testing.T) {
	v, _, clk := newTestValidator(t)
	client, _ := keypair.Random()
	now := clk.Now().Unix()
	body := map[string]any{
		"iss": testHomeDomain,
		"sub": client.Address(),
		"iat": now,
		"exp": now + 3600,
		"nbf": now,
	}

	if _, err := v.VerifyJWT(context.Background(), forgeHS256(t, body)); !errors.Is(err, auth.ErrUnauthorized) {
		t.Errorf("token without network claim: err = %v, want wrap of ErrUnauthorized", err)
	}

	id := network.ID(network.TestNetworkPassphrase)
	body["network_id"] = hex.EncodeToString(id[:])
	subj, err := v.VerifyJWT(context.Background(), forgeHS256(t, body))
	if err != nil {
		t.Fatalf("token with matching network claim: %v", err)
	}
	if subj.Identifier != client.Address() {
		t.Errorf("Subject.Identifier = %q, want %q", subj.Identifier, client.Address())
	}
}

// forgeHS256 signs body with the shared test secret in the exact shape the
// validator issues.
func forgeHS256(t *testing.T, body map[string]any) string {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	enc := base64.RawURLEncoding
	input := enc.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`)) + "." + enc.EncodeToString(raw)
	mac := hmac.New(sha256.New, testJWTSecret)
	mac.Write([]byte(input))
	return input + "." + enc.EncodeToString(mac.Sum(nil))
}

func signPubnetChallenge(t *testing.T, xdr string, client *keypair.Full) string {
	t.Helper()
	tx, err := txnbuild.TransactionFromXDR(xdr)
	if err != nil {
		t.Fatalf("TransactionFromXDR: %v", err)
	}
	inner, ok := tx.Transaction()
	if !ok {
		t.Fatal("expected inner transaction")
	}
	signed, err := inner.Sign(network.PublicNetworkPassphrase, client)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	out, err := signed.Base64()
	if err != nil {
		t.Fatalf("Base64: %v", err)
	}
	return out
}
