# Q2 — Dependency & Supply-Chain Audit

**Scope:** read-only audit of the Go monorepo `github.com/StellarIndex/stellar-index`.
**Date:** 2026-06-15 · **Auditor:** automated (Claude).
**Module:** single Go module, `go 1.25.10`, no `replace` directives, `go mod verify` → `all modules verified`.

**Headline:** No license red flags (all permissive). No *reachable* third-party
vulnerabilities — the two reachable findings are Go **standard-library** issues fixed by a
**toolchain bump (1.25.10 → 1.25.11)**, not a dependency bump. `golang.org/x/crypto`
(a direct dep) is one minor behind the govulncheck-fixed version. One **VERSIONS.md drift**
on the go-stellar-sdk pin.

---

## Findings

| Severity | Dependency | Issue | Recommendation |
| -------- | ---------- | ----- | -------------- |
| **High** | Go stdlib (`net/textproto`, `crypto/x509`) | 2 **reachable** CVEs: GO-2026-5039 (textproto unescaped error inputs) reached via `dashboardwebhooks.HandleUpdate → io.ReadAll`; GO-2026-5037 (x509 inefficient hostname parsing) reached via `hashdb.Open` + `ledgerstream.IsNotFound`. Both fixed in **go1.25.11**. | Bump the build toolchain to **go1.25.11+** and rebuild/redeploy r1 binaries. No code change needed. |
| **Medium** | `golang.org/x/crypto` v0.51.0 (**direct**) | 14 vulns reported by govulncheck (GO-2026-5005/5006/5013-5021/5023/5033), all **fixed in v0.52.0**. govulncheck classes them as *imported but your code doesn't appear to call them* (not reachable), but this is a direct dep and `go list -u` shows **v0.53.0** available. | Bump `golang.org/x/crypto` → **v0.53.0** (≥v0.52.0 clears every flagged CVE). Low risk, x/* deps are conservative. |
| **Medium** | `github.com/stellar/go-stellar-sdk` v0.5.0 | **VERSIONS.md drift** (see §VERSIONS.md). VERSIONS.md pins SHA `9d52d04a` "2026-04-22 → resolved as v0.5.0", but the real `v0.5.0` tag is dated **2026-04-07** (the 04-22 SHA post-dates the tag). Also **v0.6.0 is now released** — the load-bearing SDK is one minor behind. | Reconcile VERSIONS.md: record the actual `v0.5.0` tag SHA/date, or bump to the SHA that truly corresponds. Evaluate **v0.6.0** upgrade per the VERSIONS.md "upgrading a dep" process (re-verify per-protocol audit docs against the new SHA). |
| **Low** | `github.com/aws/aws-sdk-go` v1.49.6 (**indirect**) | 2 vulns **with no fix available** (GO-2022-0646, GO-2022-0635 — SSRF / IMDS-token issues). Not reachable per govulncheck. This is the **v1 (legacy)** SDK pulled in transitively; the repo's direct AWS use is **aws-sdk-go-v2**. | Track only. Identify which module drags in legacy `aws-sdk-go` v1 (likely `bigquery`/transitive); no action needed while unreachable. Cannot be patched (Fixed-in: N/A). |
| **Low** | AWS SDK v2 cluster (`config`, `credentials`, `service/s3`) | Several minor/patch versions behind (s3 v1.97.3 → v1.103.3; config v1.29.17 → v1.32.25; credentials v1.17.70 → v1.19.24). No CVEs. | Routine bump on next maintenance pass; AWS v2 modules release very frequently, no urgency. |
| **Low** | `google.golang.org/api` v0.278.0 | 6 minor versions behind (→ v0.284.0). No CVEs. | Routine bump. |
| **Info** | `gopkg.in/yaml.v3` v3.0.1 | Tagged dual **MIT + Apache** (NOTICE present). Permissive; no concern. | None. |
| **Info** | `github.com/coder/websocket` v1.8.14 | License is **ISC** (permissive, OSI-approved; "permission to use, copy, modify, and distribute … with or without fee"). Compatible with Apache-2.0. | None. |
| **Info** | `github.com/lib/pq` v1.12.3 | Version *looks* unusual (canonical lib/pq historically ≤ v1.10.9), but the module **origin is the real `github.com/lib/pq`** repo (not a fork) — it advanced past 1.10. MIT. | None — verified genuine. |
| **Info** | go.mod hygiene | No `replace` directives; no `toolchain` directive (relies on the `go 1.25.10` line + installed toolchain); `// indirect` markers consistent; `go mod verify` clean. | Consider adding an explicit `toolchain go1.25.11` line once bumped, so CI/builders pin the patched stdlib. |

---

## Outdated direct dependencies

`go list -u -m all` — direct deps only (indirect omitted unless CVE-bearing).

| Dependency | Current | Latest available | Behind | Notes |
| ---------- | ------- | ---------------- | ------ | ----- |
| github.com/stellar/go-stellar-sdk | v0.5.0 | **v0.6.0** | 1 minor | Load-bearing (XDR/SCVal). VERSIONS.md pinned. stellar/go monorepo archived 2025-12-16 — this is the successor SDK. |
| golang.org/x/crypto | v0.51.0 | **v0.53.0** | 2 minor | **Has CVEs** (see Findings; fixed ≥ v0.52.0). |
| github.com/aws/aws-sdk-go-v2/service/s3 | v1.97.3 | v1.103.3 | ~6 minor | No CVE. |
| github.com/aws/aws-sdk-go-v2/config | v1.29.17 | v1.32.25 | ~3 minor | No CVE. |
| github.com/aws/aws-sdk-go-v2/credentials | v1.17.70 | v1.19.24 | ~2 minor | No CVE. |
| github.com/aws/aws-sdk-go-v2 | v1.41.5 | v1.42.0 | 1 minor | No CVE. |
| google.golang.org/api | v0.278.0 | v0.284.0 | 6 minor | No CVE. |
| github.com/redis/go-redis/v9 | v9.19.0 | v9.20.1 | 1 minor | No CVE. |
| github.com/alicebob/miniredis/v2 | v2.37.0 | v2.38.0 | 1 minor | Test-only. |
| golang.org/x/sync | v0.20.0 | v0.21.0 | 1 minor | No CVE. |
| github.com/coder/websocket | v1.8.14 | v1.8.15 | 1 patch | No CVE. |

**Up to date (no update offered):** `BurntSushi/toml` v1.6.0, `golang-migrate/migrate/v4` v4.19.1,
`lib/pq` v1.12.3, `prometheus/client_golang` v1.23.2, `prometheus/client_model` v0.6.2,
`testcontainers-go` (+ postgres module) v0.42.0, `cloud.google.com/go/bigquery` v1.77.0,
`ClickHouse/clickhouse-go/v2` v2.46.0, `coreos/go-systemd/v22` v22.7.0, `google/uuid` v1.6.0,
`gopkg.in/yaml.v3` v3.0.1.

**Major-version-behind / abandoned:** none among direct deps. (`stellar/go-stellar-sdk` is pre-1.0
by upstream design; not a major-behind situation.)

---

## govulncheck output summary

`govulncheck v1.1.4` was not installed; installed `@latest` for the run (see Tools).
Exit code **3** (vulnerabilities found).

**Reachable (your code calls these) — 2, both Go standard library:**

| ID | Component | Issue | Found / Fixed | Reach trace |
| -- | --------- | ----- | ------------- | ----------- |
| GO-2026-5039 | `net/textproto` | Arbitrary inputs in errors without escaping | go1.25.10 / **go1.25.11** | `internal/api/v1/dashboardwebhooks/handlers.go:282 → io.ReadAll → textproto.Reader.ReadMIMEHeader` |
| GO-2026-5037 | `crypto/x509` | Inefficient candidate hostname parsing (DoS) | go1.25.10 / **go1.25.11** | `internal/hashdb/hashdb.go:131 → x509.Certificate.Verify/VerifyHostname`; `internal/ledgerstream/tiered.go:112 → x509.HostnameError.Error` |

**Imported but not called (1, stdlib):** GO-2026-5038 `mime` — fixed in go1.25.11.

**In required modules, not called (15):**
- `golang.org/x/crypto@v0.51.0` — **14** vulns (GO-2026-5005, -5006, -5013, -5014, -5015, -5016, -5017, -5018, -5019, -5020, -5021, -5023, -5033 + one more in series), **all fixed in v0.52.0**.
- `github.com/aws/aws-sdk-go@v1.49.6` (legacy v1, indirect) — **2** vulns (GO-2022-0646, GO-2022-0635), **Fixed-in: N/A** (no patched release).

**Net:** every reachable finding is cleared by `go1.25.11`. No reachable third-party-dependency
vulnerability. The x/crypto cluster is unreachable today but worth bumping since it's a direct dep
with a clean fix.

---

## License compatibility (direct dependencies)

Repo license: **Apache-2.0**. All direct deps are permissive and Apache-compatible.
Licenses read from the module cache `LICENSE`/`COPYING` files (`go mod download` already complete).

| Dependency | License | Apache-2.0 compatible? |
| ---------- | ------- | ---------------------- |
| github.com/BurntSushi/toml | MIT | yes |
| github.com/alicebob/miniredis/v2 | MIT | yes |
| github.com/golang-migrate/migrate/v4 | MIT | yes |
| github.com/lib/pq | MIT | yes |
| github.com/prometheus/client_golang | Apache-2.0 | yes |
| github.com/prometheus/client_model | Apache-2.0 | yes |
| github.com/redis/go-redis/v9 | BSD-2-Clause | yes |
| github.com/testcontainers/testcontainers-go (+ /modules/postgres) | MIT | yes |
| golang.org/x/sync | BSD-3-Clause | yes |
| golang.org/x/crypto | BSD-3-Clause | yes |
| github.com/stellar/go-stellar-sdk | Apache-2.0 | yes |
| cloud.google.com/go/bigquery | Apache-2.0 | yes |
| github.com/ClickHouse/clickhouse-go/v2 | Apache-2.0 | yes |
| github.com/aws/aws-sdk-go-v2 (+ config, credentials, service/s3) | Apache-2.0 | yes |
| github.com/coder/websocket | **ISC** | yes (OSI permissive) |
| github.com/coreos/go-systemd/v22 | Apache-2.0 | yes |
| github.com/google/uuid | BSD-3-Clause | yes |
| google.golang.org/api | BSD-3-Clause | yes |
| gopkg.in/yaml.v3 | MIT + Apache-2.0 (dual) | yes |

**Flagged GPL / AGPL / SSPL / non-OSI / unknown:** **none.**
No copyleft, no SSPL, no proprietary or unknown-licensed direct dependency.
(Indirect deps were not exhaustively enumerated; the well-known transitive tree —
golang.org/x/*, google.golang.org/*, grpc/protobuf, otel, moby/docker, containerd —
is uniformly BSD/MIT/Apache.)

---

## VERSIONS.md drift check

VERSIONS.md (captured 2026-04-22) pins SHAs of upstream repos. Most rows are **runtime
binaries / reference-only repos / on-chain contracts**, NOT Go-module deps, so they have
no go.mod counterpart to drift against. The one row that is a Go-module dep:

| VERSIONS.md row | VERSIONS.md says | go.mod / go.sum has | Drift? |
| --------------- | ---------------- | ------------------- | ------ |
| `stellar/go-stellar-sdk` | SHA `9d52d04a911d…cd42`, 2026-04-22, "v0.5.0 resolved by go mod tidy" | `v0.5.0` (real tag, **dated 2026-04-07**) | **YES (minor)** — the pinned SHA `9d52d04a` (2026-04-22) **post-dates** the actual `v0.5.0` tag (2026-04-07), so VERSIONS.md's "this SHA = v0.5.0" claim is internally inconsistent. go.mod is on the genuine tag; VERSIONS.md's SHA→tag mapping is stale/wrong. Additionally **v0.6.0** now exists. |

Install-time tooling pins in VERSIONS.md — **all match the Makefile exactly, no drift:**

| Tool | VERSIONS.md | Makefile | Match? |
| ---- | ----------- | -------- | ------ |
| mvdan.cc/gofumpt | v0.8.0 | `GOFUMPT_VERSION := v0.8.0` | yes |
| goimports | v0.42.0 | `GOIMPORTS_VERSION := v0.42.0` | yes |
| golangci-lint/v2 | v2.11.4 | `GOLANGCI_LINT_VERSION := v2.11.4` | yes |
| govulncheck | v1.1.4 | `GOVULNCHECK_VERSION := v1.1.4` | yes |

Runtime/reference rows (galexie v26.0.0, rs-stellar-archivist, stellar-rpc, the various
contract repos, etc.) are not Go deps — out of scope for go.mod drift; not re-verified here.

**Net:** drift = **YES**, isolated to the go-stellar-sdk SHA↔tag mapping in VERSIONS.md
(go.mod itself is correct). Tooling pins clean.

---

## Tools run

| Command | Result |
| ------- | ------ |
| `go version` | go1.25.10 darwin/arm64 — **success** |
| `go mod verify` | `all modules verified` — **success** |
| `grep "replace\|toolchain" go.mod` | none present — **success** |
| `go list -u -m all` | 521 modules, exit 0 — **success** |
| `go install golang.org/x/vuln/cmd/govulncheck@latest` | installed to `$GOPATH/bin` (repo-pinned v1.1.4 was not present) — **success** |
| `govulncheck ./...` | exit **3** (2 reachable stdlib vulns) — **success (vulns found)** |
| `govulncheck -show verbose ./...` | exit **3**, full 18-vuln module breakdown — **success** |
| `go list -m -versions <dep>` (lib/pq, go-stellar-sdk, clickhouse, migrate, bigquery, prometheus) | version histories — **success** |
| `go list -m -json github.com/lib/pq@v1.12.3` | confirmed origin = canonical `github.com/lib/pq` (not a fork) — **success** |
| `go list -m -json github.com/stellar/go-stellar-sdk@v0.5.0` | tag time 2026-04-07 — **success** |
| License detection (`find … LICENSE*` + grep over module cache) | 19 direct deps classified — **success** |
| `grep` Makefile / scripts for tooling version pins | matched VERSIONS.md — **success** |
