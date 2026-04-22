module github.com/RatesEngine/rates-engine

go 1.25

// Direct production dependencies.
//
// Every entry below has either an ADR justifying it or a one-line
// comment explaining its role. No unaudited deps.
// See docs/discovery/engineering-standards.md §2.5.

require (
	// Stellar network SDK — ledger meta reading, XDR, ingest.
	// Audited at SHA 9d52d04 / pseudo-version pinned above the cut.
	github.com/stellar/go-stellar-sdk v0.5.0

	// Typed extraction of Stellar ledger meta into row structs.
	// Audited correct incl. i128 handling. SHA e3658ce.
	github.com/withObsrvr/stellar-extract v0.1.2
)

// NOTE: this file is the skeleton for Phase 2 build work. Additional
// deps (database driver, Redis client, HTTP framework, Prometheus,
// TOML parser, k6 harness, etc.) land alongside the packages that
// use them. See CHANGELOG [Unreleased].
