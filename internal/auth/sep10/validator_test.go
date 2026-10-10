package sep10_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
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

const (
	testWebDomain  = "auth.stellarindex.test"
	testHomeDomain = "stellarindex.test"
)

var testJWTSecret = []byte("test-jwt-secret-must-be-32-bytes-or-more!!")

// newTestValidator constructs a Validator with a freshly-generated
// server keypair, testnet passphrase, and a deterministic clock.
// Returns the validator + the server keypair (so tests can introspect
// the server account address) + a clock-mover. Every client account
// is absent from its signer lookup, i.e. not yet on chain.
func newTestValidator(t *testing.T) (*sep10.Validator, *keypair.Full, *fakeClock) {
	t.Helper()
	return newTestValidatorWithAccounts(t, fakeAccounts{})
}

// fakeAccounts is an in-memory [sep10.AccountLoader]; an account missing
// from the map does not exist on chain.
type fakeAccounts map[string]sep10.AccountSigners

func (f fakeAccounts) LoadAccountSigners(_ context.Context, accountID string) (sep10.AccountSigners, error) {
	return f[accountID], nil
}

func newTestValidatorWithAccounts(t *testing.T, accounts sep10.AccountLoader) (*sep10.Validator, *keypair.Full, *fakeClock) {
	t.Helper()
	server, err := keypair.Random()
	if err != nil {
		t.Fatalf("keypair.Random: %v", err)
	}
	clk := &fakeClock{now: time.Date(2026, 4, 28, 12, 0, 0, 0, time.UTC)}
	v, err := sep10.NewValidator(sep10.Options{
		ServerSeed:        server.Seed(),
		NetworkPassphrase: network.TestNetworkPassphrase,
		WebAuthDomain:     testWebDomain,
		HomeDomain:        testHomeDomain,
		ChallengeTTL:      15 * time.Minute,
		JWTTTL:            1 * time.Hour,
		JWTSecret:         testJWTSecret,
		Now:               clk.Now,
		AccountLoader:     accounts,
	})
	if err != nil {
		t.Fatalf("NewValidator: %v", err)
	}
	return v, server, clk
}

// fakeClock returns a configurable time.Now for tests.
type fakeClock struct {
	now time.Time
}

func (c *fakeClock) Now() time.Time          { return c.now }
func (c *fakeClock) Advance(d time.Duration) { c.now = c.now.Add(d) }

// signChallenge signs a challenge XDR with the supplied client
// keypair using the txnbuild API. Mirrors what a real client SDK
// would do.
func signChallenge(t *testing.T, xdr string, client *keypair.Full) string {
	t.Helper()
	tx, err := txnbuild.TransactionFromXDR(xdr)
	if err != nil {
		t.Fatalf("TransactionFromXDR: %v", err)
	}
	innerTx, ok := tx.Transaction()
	if !ok {
		t.Fatal("expected inner transaction")
	}
	signed, err := innerTx.Sign(network.TestNetworkPassphrase, client)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	signedXDR, err := signed.Base64()
	if err != nil {
		t.Fatalf("Base64: %v", err)
	}
	return signedXDR
}

// TestNewValidator_RequiredFields — every required Option field
// must be present; missing one fails loud at construction.
func TestNewValidator_RequiredFields(t *testing.T) {
	server, _ := keypair.Random()
	base := sep10.Options{
		ServerSeed:        server.Seed(),
		NetworkPassphrase: network.TestNetworkPassphrase,
		WebAuthDomain:     testWebDomain,
		HomeDomain:        testHomeDomain,
		JWTSecret:         testJWTSecret,
		AccountLoader:     fakeAccounts{},
	}

	mutate := func(f func(*sep10.Options), wantSubstr string) {
		t.Helper()
		opts := base
		f(&opts)
		_, err := sep10.NewValidator(opts)
		if err == nil {
			t.Errorf("expected error for missing %s; got nil", wantSubstr)
			return
		}
		if !strings.Contains(err.Error(), wantSubstr) {
			t.Errorf("error %q lacks %q", err, wantSubstr)
		}
	}
	mutate(func(o *sep10.Options) { o.ServerSeed = "" }, "ServerSeed")
	mutate(func(o *sep10.Options) { o.NetworkPassphrase = "" }, "NetworkPassphrase")
	mutate(func(o *sep10.Options) { o.WebAuthDomain = "" }, "WebAuthDomain")
	mutate(func(o *sep10.Options) { o.HomeDomain = "" }, "HomeDomain")
	mutate(func(o *sep10.Options) { o.JWTSecret = []byte("short") }, "32 bytes")
	mutate(func(o *sep10.Options) { o.ServerSeed = "not-a-strkey" }, "parse ServerSeed")
	mutate(func(o *sep10.Options) { o.AccountLoader = nil }, "AccountLoader")
}

// TestChallenge_HappyPath — Challenge produces a valid SEP-10 XDR
// the SDK's verifier can read back.
func TestChallenge_HappyPath(t *testing.T) {
	v, server, clk := newTestValidator(t)
	client, _ := keypair.Random()

	ch, err := v.Challenge(context.Background(), client.Address())
	if err != nil {
		t.Fatalf("Challenge: %v", err)
	}
	if ch.TransactionXDR == "" {
		t.Error("TransactionXDR is empty")
	}
	if ch.NetworkPassphrase != network.TestNetworkPassphrase {
		t.Errorf("NetworkPassphrase = %q", ch.NetworkPassphrase)
	}
	if !ch.IssuedAt.Equal(clk.Now()) {
		t.Errorf("IssuedAt = %v, want %v", ch.IssuedAt, clk.Now())
	}
	if got := ch.ValidUntil.Sub(ch.IssuedAt); got != 15*time.Minute {
		t.Errorf("validity window = %v, want 15m", got)
	}

	// SDK round-trip — confirm what we built parses as a SEP-10 challenge.
	_, clientAddrBack, _, _, err := txnbuild.ReadChallengeTx(
		ch.TransactionXDR,
		server.Address(),
		network.TestNetworkPassphrase,
		testWebDomain,
		[]string{testHomeDomain},
	)
	if err != nil {
		t.Fatalf("ReadChallengeTx round-trip: %v", err)
	}
	if clientAddrBack != client.Address() {
		t.Errorf("client account back = %q, want %q", clientAddrBack, client.Address())
	}
}

// TestChallenge_RejectsBadClientAccount — feeding a non-G-strkey
// must fail at challenge issuance, not propagate to verify.
func TestChallenge_RejectsBadClientAccount(t *testing.T) {
	v, _, _ := newTestValidator(t)
	_, err := v.Challenge(context.Background(), "not-a-strkey")
	if !errors.Is(err, auth.ErrUnauthorized) {
		t.Errorf("err = %v, want ErrUnauthorized", err)
	}
}

// TestVerify_HappyPath — full round-trip: Challenge → client signs
// → Verify accepts + returns a JWT bearing the client's account.
func TestVerify_HappyPath(t *testing.T) {
	v, _, _ := newTestValidator(t)
	client, _ := keypair.Random()

	ch, err := v.Challenge(context.Background(), client.Address())
	if err != nil {
		t.Fatalf("Challenge: %v", err)
	}
	signedXDR := signChallenge(t, ch.TransactionXDR, client)

	tok, err := v.Verify(context.Background(), signedXDR)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if tok.JWT == "" {
		t.Error("JWT is empty")
	}
	if tok.Subject.Identifier != client.Address() {
		t.Errorf("Subject.Identifier = %q, want %q", tok.Subject.Identifier, client.Address())
	}
	if tok.Subject.Tier != auth.TierSEP10 {
		t.Errorf("Subject.Tier = %q, want %q", tok.Subject.Tier, auth.TierSEP10)
	}
	if got := tok.ExpiresAt.Sub(tok.IssuedAt); got != 1*time.Hour {
		t.Errorf("token TTL = %v, want 1h", got)
	}
}

// TestVerify_RejectsUnsignedChallenge — a transaction without the
// client's signature MUST NOT produce a token. The SDK's
// VerifyChallengeTxSigners is what catches this.
func TestVerify_RejectsUnsignedChallenge(t *testing.T) {
	v, _, _ := newTestValidator(t)
	client, _ := keypair.Random()

	ch, err := v.Challenge(context.Background(), client.Address())
	if err != nil {
		t.Fatalf("Challenge: %v", err)
	}
	// Don't sign — pass the unsigned XDR as if a malicious client
	// tried to skip the signing step.
	_, err = v.Verify(context.Background(), ch.TransactionXDR)
	if err == nil {
		t.Fatal("expected error on unsigned challenge; got nil")
	}
	if !errors.Is(err, auth.ErrUnauthorized) {
		t.Errorf("err = %v, want wrap of ErrUnauthorized", err)
	}
}

// TestVerify_RejectsWrongSigner — a challenge issued for client A but
// signed by client B must fail. SEP-10's whole point is to bind the
// JWT to the account that proved key ownership.
func TestVerify_RejectsWrongSigner(t *testing.T) {
	v, _, _ := newTestValidator(t)
	clientA, _ := keypair.Random()
	clientB, _ := keypair.Random()

	ch, err := v.Challenge(context.Background(), clientA.Address())
	if err != nil {
		t.Fatal(err)
	}
	signedXDR := signChallenge(t, ch.TransactionXDR, clientB) // wrong signer
	if _, err := v.Verify(context.Background(), signedXDR); err == nil {
		t.Error("expected error when wrong key signs; got nil")
	}
}

// TestVerify_RejectsExpiredChallenge — a challenge whose time-bound
// window has already passed (constructed with a ~200ms TTL + sleep)
// fails with ErrTokenExpired. Real-time-based because the SDK's
// VerifyChallengeTxSigners reads time.Now() directly rather than an
// injected clock.
func TestVerify_RejectsExpiredChallenge(t *testing.T) {
	server, _ := keypair.Random()
	v, err := sep10.NewValidator(sep10.Options{
		ServerSeed:        server.Seed(),
		NetworkPassphrase: network.TestNetworkPassphrase,
		WebAuthDomain:     testWebDomain,
		HomeDomain:        testHomeDomain,
		ChallengeTTL:      2 * time.Second, // SDK enforces ≥1s; 2s gives CI headroom past sleep granularity
		JWTTTL:            1 * time.Hour,
		JWTSecret:         testJWTSecret,
		AccountLoader:     fakeAccounts{},
	})
	if err != nil {
		t.Fatalf("NewValidator: %v", err)
	}
	client, _ := keypair.Random()

	ch, err := v.Challenge(context.Background(), client.Address())
	if err != nil {
		t.Fatal(err)
	}
	signedXDR := signChallenge(t, ch.TransactionXDR, client)

	// Wait past the time-bound window. 4 s past a 2 s TTL leaves
	// generous slack for slow-CI clock granularity (the txnbuild
	// SDK reads wall clock directly, not our injected one).
	time.Sleep(4 * time.Second)

	_, err = v.Verify(context.Background(), signedXDR)
	if err == nil {
		t.Fatal("expected error on expired challenge; got nil")
	}
	// classifyVerifyError maps the SDK's "not within range" / "expired"
	// substring to ErrTokenExpired.
	if !errors.Is(err, auth.ErrTokenExpired) {
		t.Errorf("err = %v, want wrap of ErrTokenExpired", err)
	}
}

// TestVerify_RejectsMalformedXDR — random garbage as the signed XDR
// surfaces ErrTokenMalformed (not ErrUnauthorized).
func TestVerify_RejectsMalformedXDR(t *testing.T) {
	v, _, _ := newTestValidator(t)
	if _, err := v.Verify(context.Background(), "not-a-base64-xdr"); err == nil {
		t.Error("expected error on malformed XDR; got nil")
	} else if !errors.Is(err, auth.ErrTokenMalformed) {
		t.Errorf("err = %v, want wrap of ErrTokenMalformed", err)
	}
}

// TestVerifyJWT_RoundTrip — Verify issues a JWT; VerifyJWT validates
// it back to the same Subject.
func TestVerifyJWT_RoundTrip(t *testing.T) {
	v, _, _ := newTestValidator(t)
	client, _ := keypair.Random()

	ch, _ := v.Challenge(context.Background(), client.Address())
	signedXDR := signChallenge(t, ch.TransactionXDR, client)
	tok, err := v.Verify(context.Background(), signedXDR)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}

	subj, err := v.VerifyJWT(context.Background(), tok.JWT)
	if err != nil {
		t.Fatalf("VerifyJWT: %v", err)
	}
	if subj.Identifier != client.Address() {
		t.Errorf("Subject.Identifier = %q, want %q", subj.Identifier, client.Address())
	}
	if subj.Tier != auth.TierSEP10 {
		t.Errorf("Subject.Tier = %q", subj.Tier)
	}
}

// TestVerifyJWT_ExpiredToken — once the JWT's exp claim has passed,
// VerifyJWT returns ErrTokenExpired (not generic ErrUnauthorized).
func TestVerifyJWT_ExpiredToken(t *testing.T) {
	v, _, clk := newTestValidator(t)
	client, _ := keypair.Random()

	ch, _ := v.Challenge(context.Background(), client.Address())
	signedXDR := signChallenge(t, ch.TransactionXDR, client)
	tok, err := v.Verify(context.Background(), signedXDR)
	if err != nil {
		t.Fatal(err)
	}

	clk.Advance(2 * time.Hour) // past the 1h JWT TTL
	_, err = v.VerifyJWT(context.Background(), tok.JWT)
	if !errors.Is(err, auth.ErrTokenExpired) {
		t.Errorf("err = %v, want ErrTokenExpired", err)
	}
}

// TestVerifyJWT_RejectsFutureNbf pins that VerifyJWT checks the
// `nbf` (not-before) claim issueJWT stamps, not only `exp`, so a token whose
// validity window hasn't opened yet is rejected. Here we mint a
// token at t0 and verify it from a clock BEFORE t0 — its nbf (== iat == t0)
// is in the future, so it must be rejected. Mirrors the exp check's strict
// (no-leeway) posture; the error is a wrap of ErrUnauthorized per VerifyJWT's
// documented "any other validation failure" vocabulary.
func TestVerifyJWT_RejectsFutureNbf(t *testing.T) {
	v, _, clk := newTestValidator(t)
	client, _ := keypair.Random()

	// Issue at the validator's current time.
	ch, _ := v.Challenge(context.Background(), client.Address())
	signedXDR := signChallenge(t, ch.TransactionXDR, client)
	tok, err := v.Verify(context.Background(), signedXDR)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}

	// Move the clock BACK so the token's nbf (== its iat) is in the
	// future relative to "now". A well-behaved verifier must not accept a
	// token that isn't valid yet.
	clk.Advance(-10 * time.Minute)

	if _, err := v.VerifyJWT(context.Background(), tok.JWT); !errors.Is(err, auth.ErrUnauthorized) {
		t.Errorf("VerifyJWT on a token with future nbf: err = %v, want wrap of ErrUnauthorized", err)
	}

	// Sanity: at/after nbf the same token verifies — the check rejects only
	// the not-yet-valid window, it doesn't reject valid tokens.
	clk.Advance(10 * time.Minute)
	if _, err := v.VerifyJWT(context.Background(), tok.JWT); err != nil {
		t.Errorf("VerifyJWT at issuance time: unexpected error %v (token should be valid once nbf has passed)", err)
	}
}

// TestVerifyJWT_TamperedSignature — flipping bits in the signature
// portion fails with ErrUnauthorized via constant-time compare.
func TestVerifyJWT_TamperedSignature(t *testing.T) {
	v, _, _ := newTestValidator(t)
	client, _ := keypair.Random()

	ch, _ := v.Challenge(context.Background(), client.Address())
	signedXDR := signChallenge(t, ch.TransactionXDR, client)
	tok, _ := v.Verify(context.Background(), signedXDR)

	// Replace the signature portion with junk.
	parts := strings.Split(tok.JWT, ".")
	if len(parts) != 3 {
		t.Fatalf("JWT shape: got %d parts", len(parts))
	}
	tampered := parts[0] + "." + parts[1] + ".AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"

	_, err := v.VerifyJWT(context.Background(), tampered)
	if !errors.Is(err, auth.ErrUnauthorized) {
		t.Errorf("err = %v, want ErrUnauthorized", err)
	}
}

// TestVerifyJWT_RejectsMalformedShape — JWT must be three
// dot-separated parts; anything else is ErrTokenMalformed.
func TestVerifyJWT_RejectsMalformedShape(t *testing.T) {
	v, _, _ := newTestValidator(t)
	for _, bad := range []string{"", "no-dots", "only.two", "four.parts.are.too.many"} {
		if _, err := v.VerifyJWT(context.Background(), bad); !errors.Is(err, auth.ErrTokenMalformed) {
			t.Errorf("input %q: err = %v, want wrap of ErrTokenMalformed", bad, err)
		}
	}
}

// TestVerifyJWT_RejectsWrongIssuer — a token whose iss claim doesn't
// match the validator's home_domain is unauthorized (different
// deployment / hostile signer with our secret).
func TestVerifyJWT_RejectsWrongIssuer(t *testing.T) {
	v1, _, _ := newTestValidator(t)
	// Build a second validator with a DIFFERENT home_domain but the
	// SAME jwt_secret. v1 should reject v2's tokens.
	server2, _ := keypair.Random()
	v2, err := sep10.NewValidator(sep10.Options{
		ServerSeed:        server2.Seed(),
		NetworkPassphrase: network.TestNetworkPassphrase,
		WebAuthDomain:     "other.example.test",
		HomeDomain:        "other.example.test",
		JWTSecret:         testJWTSecret,
		AccountLoader:     fakeAccounts{},
	})
	if err != nil {
		t.Fatal(err)
	}

	client, _ := keypair.Random()
	ch, _ := v2.Challenge(context.Background(), client.Address())
	signedXDR := signChallenge(t, ch.TransactionXDR, client)
	tok, err := v2.Verify(context.Background(), signedXDR)
	if err != nil {
		t.Fatal(err)
	}

	// v1 should reject v2's token even though the HMAC verifies —
	// the iss claim doesn't match v1.homeDomain.
	if _, err := v1.VerifyJWT(context.Background(), tok.JWT); !errors.Is(err, auth.ErrUnauthorized) {
		t.Errorf("v1 accepted v2's token; err = %v", err)
	}
}

func randomKP(t *testing.T) *keypair.Full {
	t.Helper()
	kp, err := keypair.Random()
	if err != nil {
		t.Fatalf("keypair.Random: %v", err)
	}
	return kp
}

// verifySignedBy issues a challenge for client, signs it with each of
// signers in turn, and submits it.
func verifySignedBy(t *testing.T, v *sep10.Validator, client *keypair.Full, signers ...*keypair.Full) (auth.Token, error) {
	t.Helper()
	ch, err := v.Challenge(context.Background(), client.Address())
	if err != nil {
		t.Fatalf("Challenge: %v", err)
	}
	xdr := ch.TransactionXDR
	for _, s := range signers {
		xdr = signChallenge(t, xdr, s)
	}
	return v.Verify(context.Background(), xdr)
}

func wantUnauthorized(t *testing.T, tok auth.Token, err error) {
	t.Helper()
	if !errors.Is(err, auth.ErrUnauthorized) {
		t.Fatalf("err = %v, want ErrUnauthorized", err)
	}
	if tok.JWT != "" {
		t.Fatal("a JWT was issued for a rejected challenge")
	}
}

func wantAuthenticatedAs(t *testing.T, tok auth.Token, err error, account string) {
	t.Helper()
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if tok.JWT == "" || tok.Subject.Identifier != account {
		t.Fatalf("token subject = %q (jwt empty=%v), want %q", tok.Subject.Identifier, tok.JWT == "", account)
	}
}

// A master key the holder rotated to weight 0 (control moved to a
// cosigner) cannot sign for the account on chain, so it must not
// authenticate here either.
func TestVerify_RotatedMasterKeyIsRejected(t *testing.T) {
	client, cosigner := randomKP(t), randomKP(t)
	v, _, _ := newTestValidatorWithAccounts(t, fakeAccounts{client.Address(): {
		Exists: true, MasterWeight: 0, MedThreshold: 1,
		Signers: []sep10.Signer{{Key: cosigner.Address(), Weight: 1}},
	}})

	tok, err := verifySignedBy(t, v, client, client)
	wantUnauthorized(t, tok, err)
}

// Medium threshold 0 must not let a weight-0 master key through as a
// "weight 0 >= threshold 0" match.
func TestVerify_ZeroWeightMasterRejectedAtZeroThreshold(t *testing.T) {
	client, cosigner := randomKP(t), randomKP(t)
	v, _, _ := newTestValidatorWithAccounts(t, fakeAccounts{client.Address(): {
		Exists: true, MasterWeight: 0, MedThreshold: 0,
		Signers: []sep10.Signer{{Key: cosigner.Address(), Weight: 1}},
	}})

	tok, err := verifySignedBy(t, v, client, client)
	wantUnauthorized(t, tok, err)
}

// A 2-of-2 account: the master key alone is below the medium threshold;
// master plus cosigner meets it.
func TestVerify_MultisigNeedsMediumThreshold(t *testing.T) {
	client, cosigner := randomKP(t), randomKP(t)
	v, _, _ := newTestValidatorWithAccounts(t, fakeAccounts{client.Address(): {
		Exists: true, MasterWeight: 1, MedThreshold: 2,
		Signers: []sep10.Signer{{Key: cosigner.Address(), Weight: 1}},
	}})

	tok, err := verifySignedBy(t, v, client, client)
	wantUnauthorized(t, tok, err)

	tok, err = verifySignedBy(t, v, client, client, cosigner)
	wantAuthenticatedAs(t, tok, err, client.Address())
}

// An account controlled by a cosigner (master weight 0) authenticates
// with that cosigner's signature alone when its weight meets the
// threshold.
func TestVerify_CosignerMeetingThresholdAuthenticates(t *testing.T) {
	client, cosigner := randomKP(t), randomKP(t)
	v, _, _ := newTestValidatorWithAccounts(t, fakeAccounts{client.Address(): {
		Exists: true, MasterWeight: 0, MedThreshold: 2,
		Signers: []sep10.Signer{{Key: cosigner.Address(), Weight: 2}},
	}})

	tok, err := verifySignedBy(t, v, client, cosigner)
	wantAuthenticatedAs(t, tok, err, client.Address())
}

// An account not yet on chain can only be proven by its master key.
func TestVerify_UnfundedAccountNeedsMasterKey(t *testing.T) {
	client, other := randomKP(t), randomKP(t)
	v, _, _ := newTestValidatorWithAccounts(t, fakeAccounts{})

	tok, err := verifySignedBy(t, v, client, other)
	wantUnauthorized(t, tok, err)

	tok, err = verifySignedBy(t, v, client, client)
	wantAuthenticatedAs(t, tok, err, client.Address())
}

type failingAccounts struct{ err error }

func (f failingAccounts) LoadAccountSigners(context.Context, string) (sep10.AccountSigners, error) {
	return sep10.AccountSigners{}, f.err
}

// A signer lookup that fails must fail the verification closed, never
// fall back to accepting the master key.
func TestVerify_SignerLookupFailureFailsClosed(t *testing.T) {
	lakeDown := errors.New("lake unreachable")
	v, _, _ := newTestValidatorWithAccounts(t, failingAccounts{err: lakeDown})
	client := randomKP(t)

	tok, err := verifySignedBy(t, v, client, client)
	if !errors.Is(err, lakeDown) {
		t.Fatalf("err = %v, want the lookup error", err)
	}
	if tok.JWT != "" {
		t.Fatal("a JWT was issued without a signer lookup")
	}
}

// newReplayValidator builds a Validator wired to a real Redis-backed
// replay guard (miniredis), the production configuration for SEP-10 auth.
func newReplayValidator(t *testing.T) (*sep10.Validator, *keypair.Full) {
	t.Helper()
	v, server, _ := newReplayValidatorWithRedis(t)
	return v, server
}

// newReplayValidatorWithRedis is [newReplayValidator] that also hands
// back the miniredis instance so a test can simulate key eviction.
func newReplayValidatorWithRedis(t *testing.T) (*sep10.Validator, *keypair.Full, *miniredis.Miniredis) {
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
		AccountLoader:     fakeAccounts{},
	})
	if err != nil {
		t.Fatalf("NewValidator: %v", err)
	}
	return v, server, mr
}

// TestVerify_ReplayGuard_RejectsSecondRedemption pins the
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

// TestVerify_ReplayGuard_EvictedMarkerFailsClosed pins eviction
// handling. R1's Redis runs `maxmemory-policy allkeys-lru`, which can
// evict ANY key before its TTL. With a bare SETNX spent-marker, an
// evicted marker re-opens the slot: a captured signed XDR replayed after
// the eviction finds it free and mints a second JWT. The guard must
// instead require a marker reserved at challenge issuance, so an
// eviction refuses the redemption rather than re-admitting a replay.
func TestVerify_ReplayGuard_EvictedMarkerFailsClosed(t *testing.T) {
	ctx := context.Background()

	t.Run("replay after eviction of the spent state is refused", func(t *testing.T) {
		v, _, mr := newReplayValidatorWithRedis(t)
		client, _ := keypair.Random()
		ch, err := v.Challenge(ctx, client.Address())
		if err != nil {
			t.Fatalf("Challenge: %v", err)
		}
		signedXDR := signChallenge(t, ch.TransactionXDR, client)
		if _, err := v.Verify(ctx, signedXDR); err != nil {
			t.Fatalf("first redemption: %v", err)
		}

		mr.FlushAll() // allkeys-lru evicts every replay-guard key

		if _, err := v.Verify(ctx, signedXDR); !errors.Is(err, auth.ErrUnauthorized) {
			t.Fatalf("replay after eviction: want auth.ErrUnauthorized, got %v", err)
		}
	})

	t.Run("eviction before redemption refuses rather than admits", func(t *testing.T) {
		v, _, mr := newReplayValidatorWithRedis(t)
		client, _ := keypair.Random()
		ch, err := v.Challenge(ctx, client.Address())
		if err != nil {
			t.Fatalf("Challenge: %v", err)
		}
		signedXDR := signChallenge(t, ch.TransactionXDR, client)

		mr.FlushAll()

		if _, err := v.Verify(ctx, signedXDR); !errors.Is(err, auth.ErrUnauthorized) {
			t.Fatalf("redemption of an unreserved challenge: want auth.ErrUnauthorized, got %v", err)
		}
	})
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

// TestVerify_ReplayGuard_RejectsReEncodedChallenge is the
// re-encoding attack.
//
// Attack: an attacker who captures ONE signed challenge XDR (e.g. an XSS
// exfil from a client wallet) redeems it,
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
// A dedupe key of SHA-256 of the submitted STRING would let every
// re-spelling claim a fresh unused slot, minting a JWT per submission for
// the whole challenge window. The key is the parsed transaction's
// canonical hash, which no re-encoding changes.
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
