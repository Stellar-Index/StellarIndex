package auth

import "context"

// APIKeyValidator looks up an API key and returns the Subject it authenticates; NoopAPIKeyValidator (every Lookup is ErrNotImplemented, giving 503) surfaces misconfiguration instead of demoting to anonymous.
type APIKeyValidator interface {
	// Lookup resolves the key bytes (verbatim, no whitespace strip; the middleware does that) to a Subject: ErrUnauthorized if unrecognised, ErrTokenExpired if revoked, ErrNotImplemented for a stub.
	// Implementations MUST NOT log the key value; treat it as a secret.
	Lookup(ctx context.Context, key string) (Subject, error)
}

// AccountIdentifierPrefix namespaces a [Subject.Identifier] belonging to a platform account, as opposed to a `signup-<emailhash>` identifier.
const AccountIdentifierPrefix = "acct:"

// AccountIdentifier renders the Subject.Identifier a platform account authenticates as; it is the single definition, used by PostgresAPIKeyValidator.Lookup, POST /v1/account/keys and the admin tier-clamp fan-out, which drift apart if the string is rebuilt inline.
func AccountIdentifier(slug string) string { return AccountIdentifierPrefix + slug }

// NoopAPIKeyValidator is the placeholder when auth_mode=apikey has no validator wired: every Lookup returns ErrNotImplemented (503). Deliberately not permissive, since silently authorising any key is far worse than failing.
type NoopAPIKeyValidator struct{}

// Lookup implements [APIKeyValidator] and always returns [ErrNotImplemented].
func (NoopAPIKeyValidator) Lookup(_ context.Context, _ string) (Subject, error) {
	return Subject{}, ErrNotImplemented
}

// Compile-time check.
var _ APIKeyValidator = NoopAPIKeyValidator{}
