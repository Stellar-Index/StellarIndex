package sep10_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stellar/go-stellar-sdk/keypair"
	"github.com/stellar/go-stellar-sdk/network"
	"github.com/stellar/go-stellar-sdk/txnbuild"
	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/auth"
	"github.com/Stellar-Index/StellarIndex/internal/auth/sep10"
)

// newReplayValidator builds a Validator wired to a real Redis-backed
// replay guard (miniredis), the production configuration for SEP-10 auth.
func newReplayValidator(t *testing.T) (*sep10.Validator, *keypair.Full) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	server, err := keypair.Random()
	if err != nil {
		t.Fatalf("keypair.Random: %v", err)
	}
	v, err := sep10.NewValidator(sep10.Options{
		ServerSeed:        server.Seed(),
		NetworkPassphrase: network.TestNetworkPassphrase,
		WebAuthDomain:     testWebDomain,
		HomeDomain:        testHomeDomain,
		ChallengeTTL:      15 * time.Minute,
		JWTTTL:            time.Hour,
		JWTSecret:         testJWTSecret,
		ReplayGuard:       sep10.NewRedisReplayGuard(rdb),
	})
	if err != nil {
		t.Fatalf("NewValidator: %v", err)
	}
	return v, server
}

// TestVerify_ReplayGuard_RejectsSecondRedemption pins the F-1224
// baseline the malleability test below builds on: the *same* signed XDR,
// submitted twice, only mints one JWT.
func TestVerify_ReplayGuard_RejectsSecondRedemption(t *testing.T) {
	v, _ := newReplayValidator(t)
	client, _ := keypair.Random()
	ctx := context.Background()

	ch, err := v.Challenge(ctx, client.Address())
	if err != nil {
		t.Fatalf("Challenge: %v", err)
	}
	signedXDR := signChallenge(t, ch.TransactionXDR, client)

	if _, err := v.Verify(ctx, signedXDR); err != nil {
		t.Fatalf("first redemption: %v", err)
	}
	if _, err := v.Verify(ctx, signedXDR); !errors.Is(err, auth.ErrUnauthorized) {
		t.Fatalf("second redemption: want auth.ErrUnauthorized, got %v", err)
	}
}

// reorderSignatures re-serialises signedXDR with its two decorated
// signatures swapped. Same transaction, same signatures, same canonical
// hash — a different byte string. No forgery is involved: the envelope's
// signature list simply has no canonical order, and the SEP-10 verifier
// accepts either arrangement (confirmed against the SDK in-tree).
func reorderSignatures(t *testing.T, signedXDR string) string {
	t.Helper()
	gtx, err := txnbuild.TransactionFromXDR(signedXDR)
	if err != nil {
		t.Fatalf("TransactionFromXDR: %v", err)
	}
	inner, ok := gtx.Transaction()
	if !ok {
		t.Fatal("expected inner transaction")
	}
	env := inner.ToXDR()
	sigs := env.Signatures()
	if len(sigs) != 2 {
		t.Fatalf("expected 2 signatures (server + client), got %d", len(sigs))
	}
	env.V1.Signatures = []xdr.DecoratedSignature{sigs[1], sigs[0]}
	out, err := xdr.MarshalBase64(env)
	if err != nil {
		t.Fatalf("MarshalBase64: %v", err)
	}
	return out
}

// TestVerify_ReplayGuard_RejectsReEncodedChallenge is the CON-05
// regression, expressed as the attack.
//
// Attack: an attacker who captures ONE signed challenge XDR (the F-1224
// threat model — e.g. an XSS exfil from a client wallet) redeems it,
// then re-submits the SAME transaction under a different SPELLING. Two
// independent re-spellings are verified here, both confirmed against
// this SDK to sail through ReadChallengeTx + VerifyChallengeTxSigners
// unchanged:
//
//   - swapping the order of the envelope's two decorated signatures
//     (XDR imposes no canonical order and the verifier is order-blind);
//   - inserting a newline into the base64 (Go's decoder, which the SDK's
//     XDR unmarshal uses, skips "\r"/"\n").
//
// Pre-fix the dedupe key was SHA-256 of the submitted STRING, so every
// re-spelling claimed a fresh unused slot and the replay guard could be
// walked past for the whole challenge window, minting a JWT per
// submission — precisely the stolen-XDR JWT stream F-1224 exists to stop.
// Post-fix the key is the parsed transaction's canonical hash, which no
// re-encoding changes.
func TestVerify_ReplayGuard_RejectsReEncodedChallenge(t *testing.T) {
	v, _ := newReplayValidator(t)
	client, _ := keypair.Random()
	ctx := context.Background()

	ch, err := v.Challenge(ctx, client.Address())
	if err != nil {
		t.Fatalf("Challenge: %v", err)
	}
	signedXDR := signChallenge(t, ch.TransactionXDR, client)

	// Legitimate redemption — burns the challenge.
	if _, err := v.Verify(ctx, signedXDR); err != nil {
		t.Fatalf("first redemption: %v", err)
	}

	variants := map[string]string{
		"reordered signatures": reorderSignatures(t, signedXDR),
		"embedded newline":     signedXDR[:8] + "\n" + signedXDR[8:],
		"leading newline":      "\n" + signedXDR,
	}
	for name, variant := range variants {
		t.Run(name, func(t *testing.T) {
			if variant == signedXDR {
				t.Fatal("variant is identical to the original — the test would be vacuous")
			}
			tok, err := v.Verify(ctx, variant)
			if err == nil {
				t.Fatalf("re-encoded challenge (%s) minted a fresh JWT (sub=%s) — a redeemed "+
					"challenge must stay redeemed however the XDR is spelled (CON-05)",
					name, tok.Subject.Identifier)
			}
			if !errors.Is(err, auth.ErrUnauthorized) {
				t.Fatalf("re-encoded challenge (%s): want auth.ErrUnauthorized (replay), got %v",
					name, err)
			}
		})
	}
}
