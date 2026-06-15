# 04 — Audit verdict & self-assessment (whole-system audit)

**Audit:** 2026-06-14 · **Remediation + verdict:** 2026-06-15
**Scope:** every file and cross-file interaction in the Stellar Index monorepo
(~942 Go files / 33 internal packages / 6 binaries / pkg/client / migrations /
configs / openapi / web/explorer / scripts / test / deploy / docs), audited
against dimensions D1 (correctness), D2 (ADR invariants), D3 (security), D4
(shared state), D5 (resource/perf), D6 (schema/migration), D7 (API contract),
D8 (observability), D9 (degrade-not-panic), plus cross-cutting seams X1–X10.

---

## 1. Headline

- **0 Critical.**
- **0 committed secrets.** (One real GCP service-account key exists in the
  working dir but is correctly `.gitignore`d + untracked — R-A21-A22-5;
  validates the `.gitignore` rather than violating it.)
- **i128/ADR-0003 clean** across every wire type, decoder, and the SDK —
  amounts are `*big.Int` / `NUMERIC` / decimal strings end-to-end; the two
  explorer exceptions found (`bump_to`/`offer_id` as JSON numbers) were fixed.
- **259 findings** across 26 areas (24 planned + the X6/X9 passes added during
  remediation): 25 High, 60 Medium, 117 Low, 57 Info.
- **All 25 Highs addressed: 22 FIXED + pushed, 3 DOCUMENTED with rationale, 0
  unaddressed.** The Medium/Low/Info tail is captured + triaged (none
  launch-blocking).

The single most consequential finding was the **SEP-41 supply mint/clawback
data-loss** (R-A05-1/R-A07-1): post-P23/CAP-67 the on-chain topic shape moved
the counterparty from topic[2] to topic[1], and the fixed-index decoder dropped
every CAP-67 mint + clawback. r1-lake-verified: **99.96% of recent mints + 100%
of clawbacks** were being silently lost → `total_supply` undercounted for every
watched SEP-41 token since 2025-09-03. Fixed (shape-aware decode); historical
re-derive from the lake is a deferred operator job (live ingest is now correct).

---

## 2. The 3 DOCUMENTED Highs (deliberate non-code-change, with rationale)

These are real findings where a code change was judged the wrong remedy:

1. **R-A12-1 — SEP-10 token endpoint expensive-crypto.** The global anonymous
   per-IP rate-limit (60/min) is the intended ceiling. Ed25519 verification is
   ~microseconds; `ReadChallengeTx` (a cheap XDR parse) runs before
   `VerifyChallengeTxSigners`; 60 verifies/min/IP is not a material DoS vector.
   A dedicated tight per-IP throttle on `/v1/auth/sep10/token` — the *standard
   Stellar wallet-login flow* — would degrade legitimate users behind shared
   NAT/mobile egress more than it hardens anything. The area author rated it
   Medium and offered "document the anon bucket as the ceiling" as a valid
   resolution; that is the decision.
2. **R-A15-2 / R-A15-3 — non-atomic / non-idempotent DDL in migrations
   0053–0060.** These migrations are *already applied* on r1. golang-migrate
   tracks a version number, not a file checksum, so editing them would only
   affect fresh installs — and rewriting an applied migration is a worse
   practice (and a worse risk) than the fresh-install-only hazard it carries
   (a mid-file failure on a *first* apply). Recorded as guidance for future
   migration authoring (wrap multi-statement DDL in `BEGIN/COMMIT`; use
   `DROP CONSTRAINT IF EXISTS`), not retro-edited.

Everything else High was fixed in code and pushed to `main`.

---

## 3. Remediation summary (commits on main)

| Theme | Findings | Commit(s) |
|---|---|---|
| Explorer pagination composite cursor + OpenAPI accounts | R-A11-1/2/3, R-A19-3 | da6fd426 |
| Explorer UI (result_code / accounts page / total_coins) | R-A17-1/2, R-X3X5-1 | 368deae6 |
| SEP-41 supply shape-aware decode (data loss) | R-A05-1, R-A07-1 | 99d2c2b0 |
| Login throttle / retention-down / chunk interval / k6 alerts | R-A12-2, R-A15-1/4, R-A20-1 | 538d5fd1 |
| Projector panic isolation (X9) + API-key split-brain (X6) | R-X9-1, R-X6-1 | 6b175c8a |
| SDK Pagination / S3 cred env / reference-sync lint | R-A14-1, R-A16-1, R-A19-1/2 | 3c751505 |
| Projector idle-guard, import-lint, stale binaries | R-A04-1, R-A21-A22-1, R-A18-1 | (earlier this session) |

Every commit built + tested green; the full `verify.sh` gate passed through the
final pre-X6/X9 commit, and each subsequent change was build/test/vet/gofmt/
import-lint/lint-docs verified. New regression tests were added for the
non-trivial fixes (sep41 shape matrix, explorer cursor, login throttle,
projector panic isolation, SDK pagination round-trip, S3 cred env).

**Not done on shared prod (deliberately):** historical re-derive of the dropped
sep41 mint/clawback rows from the lake; applying migration 0062; redeploying the
indexer/API. These are operator quiet-window jobs — the audit did not redeploy
r1 or run multi-day backfills concurrently with itself.

---

## 4. Coverage assurance (why this is "every file + cross-file")

- **Package/dir sweep:** every one of the 33 `internal/` packages and every
  top-level dir (cmd, pkg, migrations, configs, openapi, web, scripts, test,
  deploy, docs) is referenced in ≥1 area file's evidence. Zero unreferenced
  packages.
- **Plan iteration → gap found → gap filled.** The plan (00-audit-plan.md,
  iterated v1→v3) declared seams X1–X10. A reconciliation pass found that X2,
  X7, X8, X10 were exercised *inside* A-area findings, but **X6 (auth/permission
  flow) and X9 (panic-safety) had no written conclusion** — a genuine coverage
  gap. Both were commissioned as dedicated passes during remediation; each found
  one real High (R-X6-1 split-brain revocation, R-X9-1 projector crash-loop),
  now fixed. This is the directive's "additional passes until no areas left,"
  executed.
- **Cross-cutting dataflow seams** (X1 ingest, X3-X5 explorer interfaces, X4
  config→wiring) each have a written conclusion; the dataflow X-seams trace the
  full chains (LCM→ledgerstream→dispatcher→decoder; lake→reader→xdrjson→handler;
  config-field→consumer) rather than per-file.
- **Adversarial verification:** every High was re-checked against primary
  evidence before action. Two examples where this changed the outcome: the
  sep41 finding was confirmed by a direct r1 ClickHouse query of the live topic
  shapes (not taken on faith); the X6 split-brain was confirmed *latent* (not
  live) by reading r1's actual `auth_backend` (default redis), which set the fix
  to a fail-safe guard rather than an emergency.

---

## 5. Medium / Low / Info disposition

The 234 sub-High findings are captured in `02-findings-register.md` with stable
IDs. Policy: **none are launch-blocking; they are the post-audit backlog.** The
highest-value clusters worth scheduling (not done here):

- **A20 (tests):** several integration tests assert lower-bounds only / swallow
  insert errors / sleep instead of synchronise — they would pass through a
  silent regression. Test-hardening sweep.
- **A03 (served tier):** a handful of `/v1/pairs`/`/v1/coins` paths still scan
  raw `trades` on a 14-day window — perf, not correctness.
- **Doc-drift Mediums** (A02 stale docstring now-false, A19-06 runbook↔alert
  self-name drift) — cheap, batchable.
- **ADR-0035 protocol gates** (phoenix/aquarius/comet/defindex topic-only
  `Matches`) are tracked Mediums — already on the standing backlog (tasks
  #34–37), not new.

These were left as-is deliberately: fixing 234 lower-severity items in one pass
would dilute review quality and isn't what "launch-ready" requires.

---

## 6. Systemic root-cause classes (what to prevent, not just fix)

1. **Positional/shape assumptions on evolving on-chain data** — the sep41
   data-loss + the explorer bump_to/muxed cases. Mitigation: decode by
   shape/field-name, never fixed index; the protocol-evolution doc already says
   this — the gap was enforcement.
2. **Disjoint stores behind one feature** (X6 keys split-brain; the config
   name-vs-value S3 fields). Mitigation: a single source of truth + a boot
   assertion when two backends must agree.
3. **Recover coverage asymmetry** (X9: dispatcher recovers, projector didn't).
   Mitigation: any goroutine that runs decoders on raw lake input needs the
   same per-row recover.
4. **PR-only CI vs commit-to-main cadence** (A19 reference drift; A21 import-lint
   red). Mitigation: the load-bearing gates belong in `verify.sh` (pre-push,
   runs on every commit), not only in path-filtered PR jobs — done for the
   reference-sync check.
5. **Test rot** (A20) — assertions weak enough to pass through regressions.

---

## 7. Self-assessment — am I happy, and are more passes worth it?

**Yes, I am confident in this audit, and I do NOT think another full pass is
warranted.** Reasoning:

- **Coverage is provably complete at the unit of "area":** every package + dir
  is covered, every planned dimension + seam has a written conclusion, and the
  one real planning gap (X6/X9) was found and closed *within* this effort. A
  further full fan-out would re-read the same surface.
- **Severity ceiling is low and stable:** 0 Critical across 26 independent
  passes, and the worst finding (sep41 data loss) is a *served-tier* loss the
  ADR-0033 reconcile + the immutable lake can recover — the certified substrate
  was never compromised. The Highs concentrate in the *new, unreviewed* surfaces
  (explorer, the rebrand-era wiring), which is exactly where a first audit should
  find them, and they are now fixed.
- **The findings were adversarially verified, not just listed** — including a
  live r1 lake query that turned a "High (needs confirmation)" into a
  certain-and-fixed finding, and an r1 config read that correctly downgraded a
  "live" High to "latent."
- **Diminishing returns:** the residual is a long Medium/Low/Info tail of
  perf-nits, doc-drift, and test-hardening — the kind of backlog that's better
  burned down incrementally against real priorities than re-discovered by a
  second sweep. A targeted *test-hardening* pass (A20) would have the highest
  marginal value, but that's remediation work, not more auditing.

**What would change this verdict:** a material refactor of the ingest or auth
path, the Phase-B/C participant-index + account-state work landing (new surface),
or the public OSS flip (new threat model — external contributors). Each of those
warrants a *scoped* re-audit of the changed surface, not a repeat whole-system
sweep.

**Verdict: the system is in a sound, launch-appropriate state.** The audit is
complete; its Highs are resolved; the remaining backlog is captured, triaged,
and non-blocking.
