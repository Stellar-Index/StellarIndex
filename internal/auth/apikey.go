package auth

import "context"

// APIKeyValidator looks up an API key and returns the Subject it
// authenticates. Implementations live behind a backing store
// (Redis or Postgres); the interface keeps the middleware
// store-agnostic.
//
// Production implementation: [RedisAPIKeyValidator] (records under
// `apikey:<sha256-hex>`). The [NoopAPIKeyValidator] remains the
// fallback when auth_mode=apikey but the validator can't be
// constructed (e.g. Redis unavailable at startup) — every Lookup
// returns [ErrNotImplemented] which the middleware translates to
// 503 Service Unavailable, surfacing the misconfiguration on the
// first failed request rather than silently demoting to anonymous.
type APIKeyValidator interface {
	// Lookup resolves the supplied key bytes to a Subject. Returns
	// [ErrUnauthorized] if the key isn't recognised, [ErrTokenExpired]
	// if it's been revoked, [ErrNotImplemented] if the validator is
	// a stub.
	//
	// The key bytes are passed verbatim (no whitespace strip — that
	// happens in the middleware). Implementations MUST NOT log the
	// key value; treat it as a secret throughout.
	Lookup(ctx context.Context, key string) (Subject, error)
}

// AccountIdentifierPrefix namespaces a [Subject.Identifier] that
// belongs to a platform account (as opposed to a legacy
// `signup-<emailhash>` identifier minted by POST /v1/signup).
const AccountIdentifierPrefix = "acct:"

// AccountIdentifier renders the [Subject.Identifier] the platform
// account with the given slug authenticates as. It is the single
// definition of that shape: [PostgresAPIKeyValidator.Lookup] stamps it
// onto every Subject it resolves, POST /v1/account/keys copies the
// Subject's identifier onto the Redis records it mints, and the
// admin tier-clamp fan-out looks records up by it. Reconstructing the
// string inline in any of those places is how the three drift apart.
func AccountIdentifier(slug string) string { return AccountIdentifierPrefix + slug }

// NoopAPIKeyValidator is the placeholder used when auth_mode=apikey
// is configured but no validator implementation is wired. Every
// Lookup returns [ErrNotImplemented]; the middleware translates
// that to 503 Service Unavailable.
//
// This is intentionally not a "permissive" stub — silently
// authorising any key would be far worse than failing the request.
type NoopAPIKeyValidator struct{}

// Lookup implements [APIKeyValidator]. Always returns
// [ErrNotImplemented].
func (NoopAPIKeyValidator) Lookup(_ context.Context, _ string) (Subject, error) {
	return Subject{}, ErrNotImplemented
}

// Compile-time check.
var _ APIKeyValidator = NoopAPIKeyValidator{}
