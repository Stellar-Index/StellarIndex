// Package metadata resolves SEP-1 stellar.toml records per
// [docs/discovery/data-sources/sep1-home-domain.md].
//
// # What SEP-1 is
//
// SEP-1 ("stellar.toml") is Stellar's standard for asset issuers +
// anchor operators to self-publish metadata at a well-known URL:
//
//	https://<home-domain>/.well-known/stellar.toml
//
// The TOML file declares issuer identity, asset details (name,
// image, decimals, supply), documentation URLs, and operator
// contact info. Wallets use these to populate asset-detail UIs
// (Freighter V1 §Asset Metadata) with trustworthy metadata.
//
// # What this package does
//
// [Resolver] fetches + parses stellar.toml for a given home-domain
// and returns the relevant subset as a [SEP1] struct. Full TOML is
// preserved in [SEP1.Raw] for callers that need the uncommon
// fields.
//
// # What this package deliberately doesn't do
//
//   - Value-serving caching is the caller's job (through
//     [internal/cachekeys.TOML] keys at the Redis layer). This
//     package is stateless.
//   - Asset-metadata overlay (attaching SEP-1 fields to
//     [canonical.Asset]) happens in the API handlers +
//     aggregator, not here.
//   - Verification of issuer ↔ home-domain ↔ stellar.toml trust
//     chain is a post-launch concern. For now we trust the domain
//     owner; a future ADR introduces signature verification.
//
// # Security posture
//
// SEP-1 URLs are arbitrary operator-supplied. That makes this
// package a server-side-request-forgery (SSRF) risk surface — a
// malicious home-domain pointing at `169.254.169.254` could read
// cloud-instance metadata. Guard: [Resolver] rejects resolved IPs
// in the RFC 1918 private ranges + RFC 4193 private v6 + loopback
// + link-local + multicast before issuing the HTTP request.
//
// # References
//
//   - SEP-1 v2 spec: <https://github.com/stellar/stellar-protocol/blob/master/ecosystem/sep-0001.md>
//   - Phase-1 design: docs/discovery/data-sources/sep1-home-domain.md
package metadata
