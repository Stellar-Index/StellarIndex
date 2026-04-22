# Changelog

All notable changes to Rates Engine will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to dual versioning — SemVer for `pkg/*`
and CalVer (`YYYY.MM.DD`) for binary releases. See
[docs/discovery/repo-structure-plan.md §10](docs/discovery/repo-structure-plan.md)
for the rationale.

Every release lists the Stellar protocol version it was tested
against.

---

## [Unreleased]

### Added

- Repository foundation: `LICENSE` (Apache-2.0), `README.md`,
  `CLAUDE.md`, `CHANGELOG.md`, `CONTRIBUTING.md`,
  `CODE_OF_CONDUCT.md`, `SECURITY.md`, `CODEOWNERS`.
- ADRs 0001–0005 extracted from Phase-1 `decisions.md`.
- Root-level `VERSIONS.md` — pinned SHAs of all audited
  upstream deps.
- Makefile skeleton with `help`, `lint`, `test`, `build`,
  `docs-all` targets.
- `.golangci.yml` strict lint config per
  [engineering-standards.md §8](docs/discovery/engineering-standards.md).
- GitHub Actions `ci.yml`, PR template, CODEOWNERS,
  `dependabot.yml`.
- `.gitignore` including secrets / env file guards.
- `internal/canonical/` package skeleton — `Amount` type as
  `*big.Int` wrapper (ADR-0003 enforcement).

### Tested against

- Stellar protocol 25.x (mainnet passphrase
  `"Public Global Stellar Network ; September 2015"`).
- stellar-core `v26.0.1`, stellar-rpc `v26.0.0`,
  stellar-galexie `v26.0.0`.
- `go-stellar-sdk v0.5.0`, `withObsrvr/stellar-extract v0.1.2`.

---

<!--
Release sections will be added here as versions ship. Keep the
[Unreleased] block at the top; the release workflow moves it
under the new version header on tag push.

Example of a future release entry:

## [2026.06.30.1] — 2026-06-30 — Initial public release

### Added
- Full SDEX / Soroswap / Aquarius / Phoenix / Comet / Blend indexing.
- Reflector / Redstone / Band oracle integration.
- Since-inception OHLC for top-20 pairs.
- REST + SSE API v1.

### Tested against
- Stellar protocol 25.x.
- stellar-core v26.0.1, stellar-rpc v26.0.0.
-->
