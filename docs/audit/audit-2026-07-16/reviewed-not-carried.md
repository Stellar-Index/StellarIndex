# Reviewed and not carried forward — audit 2026-07-16

Recorded to fight false-positive drift and to show the work: candidates that were refuted, and prior-audit findings re-derived as fixed/sound. (Per-chunk refuted lists are in the workflow results; this file captures the load-bearing re-verifications. Finalized after chunk 4.)

## Prior-audit (docs/audit-2026-06-30, CS-###) re-verification — fixes that HELD
Re-derived cold against current code (`4d034432`); these prior findings did NOT resurface as confirmed, indicating the remediation held. Fast-shipped remediation is the highest-risk surface, so these are re-verifications, not inherited verdicts.

- **CS-012 — SSE `Hub.Publish` send-on-closed-channel crash** — not surfaced; the publisher path appears guarded. (Contrast: CS-013 below did resurface.)
- **CS-124 — dashboard CSRF (SameSite=None, no token)** — not surfaced; session/CSRF handling appears fixed.
- **CS-100 — `org_verified` computed but not enforced (issuer impersonation)** — not surfaced; enforcement appears wired.
- **CS-009 — CF OG edge-function SSRF** — the double-decode/unescaped-path SSRF appears MITIGATED (chunk-4 found no injection); however the OG endpoint still has **no rate limit / input-cardinality bound** (carried as chunk-4 C4-12, a DoS not an SSRF).
- **8 IDOR candidates** — not surfaced; ownership-before-mutation + empty-subject-fails-closed appear intact (chunk-3 api-auth found no cross-tenant read/write).
- **SQL / ClickHouse injection** — not surfaced; every user value is bind-parameterized (`sqlQuoteList` latent-only, trusted-pipeline-reachable).
- **API-key constant-time compare, SEP-10 replay guard, XFF spoofing** — not surfaced as defects.
- **CS-110/111/112 — DR (restore never drilled / backups co-located / CH lake no backup)** — pgBackRest now configured + drilled per the deployment docs (operational, not a code defect; [OP] to re-confirm the CH-lake-backup gap).

## Prior-audit findings that DID resurface (carried into findings.md, not here)
- **CS-013 — SSE FD-exhaustion / no conn cap** → chunk-3 C3-8 (still present).
- **CS-021 — `ledger_entries_current` versioned by ledger_seq only (resurrect deleted / before-image)** → chunk-2 C2-4 (broadened across readers).
- **CS-040 — per-source USD decimals** → chunk-1 G1 (regressed; dead `windowUSDVolume`).
- **CS-028 — cursor advances on enqueue not durable** → chunk-1 D2 / chunk-2 C2-14.
- **CS-083/084 — watermark overwrite / reconcile-nets-to-0** → chunk-2 C2-3/C2-16 (retentionStart + oracle netting).
- **CS-087/088 — divergence passes when refs down** → chunk-1 M9 (CoinGecko no staleness gate) + chunk-3 C3-4 (SDK drops `divergence_checked`).
- **CS-010 — XLM circulating==total, +58% market cap** → the `sdf_reserve_accounts=[]` basis is honest in code (CS-038-style clamp present); the *served value* is still wrong on R1 because the config leaves reserves empty ([OP]/data, not a code defect) — supply overlay + basis-label handling audited (chunk-1 M14 is a related but distinct issuer-TOML poison).
- **CS-118/119/120/121/122 (infra hardening: root services, missing user, SSH pw-auth invert, Discord webhook 0644, Patroni unauth)** → [pending chunk-4 infra-cicd confirmation].

## Candidates raised then refuted this run (false-positive log)
[Populated from per-chunk `reviewed_not_carried` after chunk 4 — the skeptic-refuted set, e.g. red-herrings like the Kraken timestamp float and the `Amount.Scan` int64 arm from chunk 1's money sweep.]

## Notable GOOD (recorded to prevent re-litigation)
- Stripe webhook dedup (AppendStripeEvent + MarkStripeEventProcessed) + signature verify + empty-secret→503 — correct (chunk-3).
- SEP-1 outbound dialer hardening (Proxy:nil, redirect cap, anti-rebind, 1 MiB cap) — correct, background-only (chunk-1/3).
- i128/NUMERIC/string discipline on the on-chain decode path — TestI128TruncationGuard + lint hold; violations are float round-trips of off-chain JSON, not truncation (chunk-1).
- ClickHouse contiguity + hash-chain-to-genesis is actually checked, not assumed (chunk-2).
- Auth-backend=postgres self-service split-brain guard (503) — correct (chunk-3).
