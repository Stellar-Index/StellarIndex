package sep10_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stellar/go-stellar-sdk/keypair"

	"github.com/Stellar-Index/StellarIndex/internal/auth"
	"github.com/Stellar-Index/StellarIndex/internal/auth/sep10"
)

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
// cosigner) can no longer sign for the account on chain, so it must not
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
