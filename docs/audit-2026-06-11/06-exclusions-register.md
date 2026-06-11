# Exclusions Register — 2026-06-11 audit

Depth calibration per the plan. What was NOT line-reviewed and why, so coverage claims are honest.

| Scope | Treatment | Rationale |
|---|---|---|
| `docs/discovery/` (48 files) | Headers/structure skimmed; only the 6 CLAUDE.md-linked live-reference docs (soroswap, phoenix, comet, reflector, band, redstone) read in full + checked vs decoders | Frozen Phase-1 archive (read-only since 2026-04-22). Verified frozen status holds; the 6 live-referenced docs' claims match current decoder code. |
| Prior audit dirs `docs/audit-2026-04-29`, `-05-02`, `-05-12`, `-05-12-codex`, `-05-26` (~206 files) | Structural integrity only (README/plan/findings present, all ledger files exist); sampled 10 dispositions in the 2026-05-26 register, spot-verified 3 closed findings (F-0024/F-0029/F-0051) genuinely closed in code | Historical records; re-auditing closed findings is out of scope. Sampling found them internally consistent and genuinely closed. |
| `docs/reference/` (generated) | Read fully but audited for **generation drift**, not content authorship | Auto-generated from OpenAPI + struct tags; content bugs belong to the sources (openapi/, config.go). Exception: metrics/README.md is hand-edited (F-1256) and WAS content-audited → F-1329/D6-02. |
| `test/fixtures/**` binary/XDR/JSON (49 files) | Reference-checked (every fixture consumed by some test — no orphans), not line-reviewed | Binary content; the audit question is "is it referenced + does it encode the right WASM-version", answered structurally. Date-named dirs vs wasm-hash convention noted (G22-11). |
| `docs/operations/wasm-audits/evidence/**` (~142 .wasm/disasm/json/py) | Existence + spot-check, not line-read | Captured artifacts; the audit reviewed the audit-log conclusions (D5-07/08), not the raw bytes. |
| `ctx-proposal.md` / RFP base64 image lines | Stripped for reading (hit size caps) | 608KB inline PNGs; flagged as a tooling problem (D1-12), content unaffected. |
| Live r1 host state (running services, DB row counts, actual metric values) | Spot-checked only where a finding hinged on it (persist_per_source config, smoke, incident sweep) | This is a **repo artifact** audit. Live-state verification was today's separate incident sweep. Several findings flagged "settle by checking r1" for follow-up (FX-snap deadness, LatestTradePerSource cost, base() quote). |
| `web/*/pnpm-lock.yaml` (3 large lockfiles) | Structurally scanned (importers, overrides, lifecycle-script allowlist, non-registry sources), not line-read | Generated; the audit question is supply-chain hygiene, answered structurally (WB-08: clean). |
| `web/explorer/out/` (1016 build-output files) | Not in scope (generated static export) | Build artifacts; source is web/explorer/src (audited in full). |
| `go.sum` | Module list skimmed for archived-upstream/replace red flags | Checksums; not human-reviewable line by line. Clean (no replace, no stellar/go). |

## Not findings (verified-good, recorded to avoid re-litigation)

- **i128/u128 discipline** holds across every decoder + storage path audited (big.Int/NUMERIC/string-JSON); KALIEN regression pinned. The one documented enforcement gap is the *claimed* custom golangci analyzer that doesn't exist (D2-10) — the runtime guard (ErrI128Overflow) is real.
- **Auth primitives** are sound: 32-byte crypto/rand keys, SHA-256 lookup, constant-time JWT compare with pinned header (no alg-confusion), Lua-atomic rate limiting with documented fail-open→fail-closed, SETNX SEP-10 replay guard, GETDEL single-use tokens, HMAC webhooks with redirects disabled + post-DNS SSRF dialer. (Sub-findings F-1335/1336/1338 are edge refinements, not breaks.)
- **ADR numbering/immutability** clean (0001-0034 contiguous, retro-edits are markers/amendments not decision rewrites).
- **Secrets**: no tracked credentials anywhere (Go, configs, ansible vault encrypted, web configs); gitleaks allowlists are documented public-value false positives.
- **ClickHouse lake design** (ledgers-last commit-marker, ReplacingMergeTree idempotency) and the **flush-ordering** invariant are correct (untested though — F-1349/G12-09).
- **No unbounded DISTINCT-ledger/LAG trades scans** in any runbook (lens clean).
- **Dependency posture** healthy (next post-CVE, registry-only, no lifecycle scripts, govulncheck only flags 3 patchable stdlib/x-net advisories → W0-01).
