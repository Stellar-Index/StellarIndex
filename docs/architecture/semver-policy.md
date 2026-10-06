---
title: SemVer policy for Stellar Index
last_verified: 2026-10-06
status: ratified
---

# SemVer policy

One Go module ([ADR-0005](../adr/0005-monorepo.md)), so **one tag clock**: the root `vX.Y.Z` tag.
Binaries and `pkg/*` both ship inside it. There is no `pkg/*` tag namespace, because a
`pkg/client/vX.Y.Z` tag would version nothing (`pkg/client` is a package, not a module). Do not create
`pkg/*` tags and do not add a `pkg/client/go.mod`: that would remove the package from the root module
for every consumer pinned on a root tag.

| Surface | Tag | Bump rule |
|---|---|---|
| `pkg/*` (today `pkg/client`) | root `vX.Y.Z` | A `pkg/*` break bumps the root minor (pre-v1.0) or major (post-v1.0) |
| Binaries (`stellarindex-api`, `stellarindex-indexer`, ...) | root `vX.Y.Z` | Operator-impact SemVer (config, wire, behaviour) |

Consumers pin the SDK on the root clock:

```sh
go get github.com/Stellar-Index/StellarIndex/pkg/client          # latest root tag
go get github.com/Stellar-Index/StellarIndex@v0.105.0            # explicit pin
```

A root minor bump says "something changed", not "the SDK broke". The `CHANGELOG.md` entry is the only
place an SDK break is announced, so it is mandatory.

Nothing enforces these rules mechanically: there is no apidiff or gorelease check in CI. Review and the
CHANGELOG carry them.

## SemVer rules for `pkg/*`

Every package under `pkg/` is public API. `internal/*` is not: refactor, rename or delete it in any PR.

`pkg/client` is the Go SDK for the public API. Its wire-shape types (`Envelope`, `Flags`, `Pagination`,
`AssetDetail`, ...) live in `pkg/client/types.go`. The server's `internal/api/v1` keeps its own envelope on
purpose: the duplication is the firewall between the SDK surface and handler shapes.

### What constitutes a breaking change

Major (minor while pre-v1.0):

1. Removing or renaming an exported identifier (type, function, variable, constant, method).
2. Removing a struct field or an interface method.
3. A non-additive signature change (parameter or return types, or their order).
4. Adding a method to an interface.
5. Changing the JSON wire shape a public type marshals to.
6. Tightening input validation so previously accepted inputs are rejected.
7. Changing documented error semantics (for example `nil, ErrNotFound` becoming `nil, nil`).

Minor: a new exported identifier; a new struct field with a sensible zero value; loosened validation; a
new optional config field; a new error sentinel that still matches the old one under `errors.Is`.

Patch: behaviour-preserving fixes, performance work, docs, tests, internal refactors.

### Pre-v1.0 (`v0.x`)

- Breaking changes are allowed and bump the root **minor**. They MUST be called out in `CHANGELOG.md`
  under the version where they land.
- At `v1.0.0` the contract becomes binding; a break then needs `v2.0.0`.

### Deprecation

1. Mark the identifier `// Deprecated: <reason>. Use <replacement>.` in the same release.
2. Keep it for at least one minor version.
3. Remove it only at the next major boundary.
4. The CHANGELOG entry of the deprecating release calls it out, and so does the removing release.

### Invariants

- `pkg/client/client.go`'s `userAgent` constant is **hand-bumped**. Wiring it to `internal/version.Version`
  would report an empty version to SDK consumers: the ldflags only fire on our own binary builds.
- `pkg/client` imports nothing from this repo.

## SemVer rules for binary releases

`vX.Y.Z`, tagged once at the release commit. The Makefile's `git describe --tags --always --dirty`
fills `internal/version.Version` through `-ldflags`, so every binary reports the same version.

- **Major**: an operator must act beyond a restart.
- **Minor**: additive, no operator action.
- **Patch**: operator-invisible.

Pre-v1.0, a breaking change bumps the **minor** and the CHANGELOG entry names the operator action.

Major-class (operator-breaking) changes:

1. Config schema change that breaks existing configs.
2. API wire-shape change for an existing endpoint.
3. API endpoint removal or rename.
4. CLI flag removal.
5. A migration that needs a manual backfill beyond `stellarindex-migrate up`.
6. Removing a source connector.
7. A behaviour change in fallback semantics (VWAP, TWAP, last-trade chain).

Minor-class: a new endpoint, CLI flag, config field (safe default), source connector (`enabled = false`
by default), opt-in aggregation feature, forward-only migration, or metric.

Patch-class: fixes that keep documented behaviour, performance, `internal/*` churn, docs, tests,
behaviour-neutral dependency bumps, rebuilds on a newer Go toolchain.

### Release notes

Each `## [<version>]` CHANGELOG section MUST have:

1. **Operator action required: yes/no** on the first line.
2. The Stellar protocol version the release was tested against.
3. Any breaking `pkg/*` change, in prose (omit if none).
4. Migration notes, or "None."
5. The Added / Changed / Deprecated / Removed / Fixed / Security sections.

Section headers carry the UTC date (`## [v0.2.0] - 2026-07-15`). The runbook is
[release-process.md](../operations/release-process.md); the GitHub Release body follows
[RELEASE_NOTES_TEMPLATE.md](../../.github/RELEASE_NOTES_TEMPLATE.md).

## Cross-references

- [ADR-0005](../adr/0005-monorepo.md): one Go module; the `pkg/*` SemVer commitment.
- [`pkg/client/doc.go`](../../pkg/client/doc.go): the SDK's stated v0.x stability promise.
