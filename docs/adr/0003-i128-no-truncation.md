---
adr: 0003
title: i128 / u128 values preserved end-to-end; never truncated to int64
status: Accepted
date: 2026-04-22
supersedes: []
superseded_by: null
---

# ADR-0003: i128 / u128 values preserved end-to-end; never truncated to int64

## Context

Soroban stores token quantities as `i128`/`u128`; above about 922 billion tokens at 7 decimals an
`int64` overflows. Adjacent tooling shipped this bug in production (KALIEN balance 40,000,005,972,900,000,000
shown as its low 64 bits), so it is the most important correctness invariant in the project.

## Decision

Every `i128`/`u128` is held at full 128-bit precision from on-chain event to API response:
`xdr.Int128Parts`/`UInt128Parts` upstream, `canonical.Amount` (wrapping `*big.Int`) in Go, `NUMERIC` in Postgres, and a decimal string in JSON (`pkg/client` exposes amounts as `string` fields), never a JSON number
(IEEE 754 doubles keep 53 bits). In OpenAPI a 128-bit value is `type: string` with the
decimal-string contract in its `description`; there is no `format: i128` tag. No code path may hold
one of these values in `int64`, `uint64`, `float32` or `float64`, with no exceptions. Two's-complement
sign handling lives in `internal/canonical/amount.go`.

## Invariant

No Go code lossily converts the word fields of `xdr.Int128Parts`, `UInt128Parts`, `Int256Parts` or
`UInt256Parts`, nor feeds `MustI128()`/`MustU128()` into a numeric conversion
(`internal/canonical/i128_truncation_guard_test.go`; `scripts/ci/lint-i128.sh` rejects
`int64(<x>.Lo)`). Escape: `//i128:ok <reason>`; stale markers fail.

Every `migrations/*.up.sql` column with a monetary name is `NUMERIC`, never BIGINT, INT8, DOUBLE
PRECISION, FLOAT or REAL (`scripts/ci/lint-migrations.sh`). Escape: `-- lint-money:ok <reason>`;
the `lint-money:ok` markers under `migrations/` are the authoritative list.

No named API response struct field with a monetary JSON name marshals to a JSON number without
`,string` (`internal/canonical/wire_money_guard_test.go`; named structs only, not `map[string]any`).

Amount round-trip fixtures, including the KALIEN regression, must pass and a failing one blocks the release
(`internal/canonical/amount_test.go`); changes to `internal/canonical/` need CODEOWNERS review.

## Consequences

Pricing stays correct for RWA and high-supply tokens, at negligible memory cost. JSON clients that
parse amount strings as numbers lose precision; that is documented in the API and SDK docs and is
theirs to fix. Any observed `errors.Is(err, canonical.ErrI128Overflow)` in production fires a SEV-1. It indicates an
`int64` sneaking in somewhere on one of our own amount paths, never a property of the chain data.

## Evidence

The four checks named in Invariant, all in CI; `lint-i128.sh` is wired into `make verify`.
