// Package metadata fetches and parses SEP-1 stellar.toml records from
// https://<home-domain>/.well-known/stellar.toml.
//
// [Resolver] returns the served fields as a [SEP1] and keeps the full TOML in [SEP1.Raw]. It is
// stateless and uncached: production resolves on the `stellarindex-ops sep1-refresh` rotation into
// `issuers.sep1_payload`, and the API never fetches a stellar.toml on the request path.
//
// Signatures are NOT verified: we serve what the home domain claims. sep1_status "verified" means
// only that the on-chain home-domain link matched a [[CURRENCIES]] entry.
//
// Home domains are attacker-supplied, so this is an SSRF surface: [Resolver] refuses resolved
// addresses that are private (RFC 1918, RFC 4193), loopback, link-local or multicast before any
// request.
package metadata
