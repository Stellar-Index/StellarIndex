package auth

import (
	"context"
	"time"
)

// SEP10Validator is the SEP-10 server side: Challenge issues a tx to sign, Verify accepts it and issues a JWT, VerifyJWT checks it per request.
// NoopSEP10Validator (all ErrNotImplemented, so /v1/auth/sep10/* is 404) covers missing config; auth_mode=sep10 with missing config fails startup.
type SEP10Validator interface {
	// Challenge generates a SEP-10 challenge transaction for the account: unsigned and never
	// submitted, a structured nonce. The stub returns [ErrNotImplemented].
	Challenge(ctx context.Context, account string) (Challenge, error)

	// Verify accepts a challenge transaction signed by the account in Challenge and returns a JWT for it; ErrUnauthorized for bad signatures, ErrTokenMalformed for an unparseable transaction.
	Verify(ctx context.Context, signedTransactionXDR string) (Token, error)

	// VerifyJWT validates a JWT from Verify (or a delegated SEP-10 server) and returns its Subject; ErrTokenExpired when exp has passed, ErrUnauthorized for any other failure.
	VerifyJWT(ctx context.Context, jwt string) (Subject, error)
}

// Challenge is a SEP-10 challenge transaction; the client signs TransactionXDR and the other fields are echoed for its diagnostics.
type Challenge struct {
	// TransactionXDR — base64 XDR of the unsigned transaction, signed by the client's account key.
	TransactionXDR string

	// NetworkPassphrase — the network crafted for ("Public Global Stellar Network ; September 2015"
	// for pubnet), echoed so the client can refuse a challenge for the wrong network.
	NetworkPassphrase string

	// IssuedAt + ValidUntil — the challenge has a short lifetime
	// (typically 15 minutes). Verify rejects challenges past
	// ValidUntil.
	IssuedAt   time.Time
	ValidUntil time.Time
}

// Token is what [SEP10Validator.Verify] returns: the JWT plus metadata the client uses to know when to refresh.
type Token struct {
	// JWT — the bearer token the client sends on subsequent
	// requests as `Authorization: Bearer <JWT>`.
	JWT string

	// IssuedAt + ExpiresAt — token lifetime; clients should refresh
	// before ExpiresAt.
	IssuedAt  time.Time
	ExpiresAt time.Time

	// Subject — the authenticated identity. Same value VerifyJWT
	// returns; convenience for clients that want it without
	// decoding the JWT themselves.
	Subject Subject
}

// NoopSEP10Validator is the fallback when the sep10 validator cannot be built and auth_mode is not sep10: every method returns ErrNotImplemented and the handlers answer 404 sep10-unavailable.
type NoopSEP10Validator struct{}

// Challenge implements [SEP10Validator].
func (NoopSEP10Validator) Challenge(_ context.Context, _ string) (Challenge, error) {
	return Challenge{}, ErrNotImplemented
}

// Verify implements [SEP10Validator].
func (NoopSEP10Validator) Verify(_ context.Context, _ string) (Token, error) {
	return Token{}, ErrNotImplemented
}

// VerifyJWT implements [SEP10Validator].
func (NoopSEP10Validator) VerifyJWT(_ context.Context, _ string) (Subject, error) {
	return Subject{}, ErrNotImplemented
}

// Compile-time check.
var _ SEP10Validator = NoopSEP10Validator{}
