// Package metadata fetches and parses SEP-1 stellar.toml records from
// https://<home-domain>/.well-known/stellar.toml
// (spec: https://github.com/stellar/stellar-protocol/blob/master/ecosystem/sep-0001.md).
//
// [Resolver] returns the fields we serve as a [SEP1]; the full TOML stays
// in [SEP1.Raw]. It is stateless and uncached: production resolves on the
// `stellarindex-ops sep1-refresh` rotation into `issuers.sep1_payload`,
// and the API never fetches a stellar.toml on the request path
// (docs/operations/sep1-resolution.md).
//
// Signatures are NOT verified: we serve what the home domain claims.
// The API's sep1_status "verified" means only that the on-chain
// home-domain link matched a [[CURRENCIES]] entry. Overlaying SEP-1
// fields onto assets happens in the API and aggregator, not here.
//
// Home domains are attacker-supplied, so this is an SSRF surface:
// [Resolver] refuses resolved addresses that are private (RFC 1918,
// RFC 4193), loopback, link-local or multicast before any request.
package metadata
