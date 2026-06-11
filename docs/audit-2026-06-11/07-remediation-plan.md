# Remediation Plan — 2026-06-11 audit

Prioritized by **risk × launch-proximity** (the public flip targets the week of Jun 24–30). Each tier is a set of mergeable units — one PR per logical fix per the repo's commit-merge-repeat cadence. This is a *plan*, not executed work (the audit was read-only).

## Tier 0 — fix before the public flip (data integrity + correctness customers see)

1. **F-1321 — market-cap 10^5–10^7× inflation.** Use the on-chain scale (7 for classic) for all unit math; surface `display_decimals` as a separate display-hint field. Hostile-issuer self-inflation on the *verified* surface — the most customer-visible correctness bug. (assets.go:1558, assets_f2.go) — small, high-value.
2. **F-1316 + F-1327 — sep41 projector + config default foot-gun.** Two-part: (a) give the sep41 decoders a watch-all/topic-only mode for the projector (or skip sep41 in IsProjectedEvent until they can serve); (b) make `Default()` set `PersistPerSource=true` AND add a reflective drift-test asserting every `default:` tag round-trips through `Default()` (closes the whole F-1327 class incl. cookie_secure/email-verification/TLS-probe/divergence-interval). Do (b) first — it's the safety backstop.
3. **F-1322 — Stripe never-upgrades.** AppendStripeEvent must return ErrAlreadyProcessed only when `processed_at IS NOT NULL`; rows with NULL processed_at must re-process. Add the failed-first-delivery regression test. Paid-customer-facing.
4. **F-1325 — observations suppress CEX data.** Narrow the #29 fast-path to quotes that genuinely never appear in trades (fiat:USD only), or drop it for the composite-index fix; fix the test that pins the wrong behavior.
5. **F-1326 — /v1/assets pagination dead.** One-line `opts.Limit = limit + 1` + a >limit regression test. Only first 500 of ~440K assets are reachable today.
6. **F-1319 — VWAP over oldest 10k.** Detect `len(trades) >= limit` → truncation counter + WARN; switch to DESC for large windows or aggregate in SQL. Silent stale 24h VWAP on liquid pairs.

## Tier 1 — fix before launch (durability, security, the alerts that must fire)

7. **F-1329 — dead alert layer (18/106).** Highest-leverage ops fix. Emit the missing metrics (a Timescale textfile exporter for cagg-staleness/compression-lag/pgbackrest, mirroring galexie-archive-tip-lag.sh) or repoint exprs at what exists (`pgbackrest_*`, `node_zfs_zpool_state`, verify-archive-mismatches, postgres_exporter `pg_up`). **Backups can silently stop with zero paging today.** Add a CI check that every metric token in a rule resolves to an emitted name.
8. **F-1317 — dispatcher Stats() race.** Add a mutex (or atomics) around the counter maps. Fatal-crash class; cheap fix.
9. **F-1318 + F-1350 — durability + shutdown.** Flush worker buffers with a fresh bounded context on shutdown (mirror drainBufferedEvents); register `defer cancel()` after store defers; gate cursor/ledger_ingest_log on a persist ACK or document+accept the window with completeness-reconcile as the explicit repair. Add the missing shutdown-sequencing tests. Off-chain CEX/FX loss is unrecoverable.
10. **F-1320 — supply dormant-asset rejection.** Re-stamp component observations at sweep cadence when the observer is live but the entry is unchanged, OR add a per-asset alert (`max by (asset_key)(timestamp(...ok))`) + runbook pointing at WithStaleComponentLedgersFor. Add `stale_component` to the outcome enumerations.
11. **F-1332 — dashboard security headers.** Add web/dashboard/public/_headers mirroring the status page (CSP, frame-ancestors 'none', HSTS, nosniff). The auth surface is the one missing them.
12. **F-1323 + chainlink liveness (G10-02).** Dedup on the full uint80 (or detect phase regression); make PollOnce return the error when all feeds fail (don't bump the staleness gauge on a dead poller). Resolves the chronic chainlink wedge.
13. **F-1335 + F-1338 — rate-limit/XFF.** Key anonymous bucket on resolved client IP only; walk XFF right-to-left to the first untrusted hop; fix example.toml's unsafe suggested CIDR. Do before apikey_optional goes live.
14. **F-1324 — coarse-PK sweep.** Add event_index (+ content/kind discriminators) to sep41_supply_events, blend_auctions, comet_liquidity, phoenix_liquidity/stake, sdex_offer_events PKs; plumb EventIndex through the decoders (sep41_transfers especially); re-derive collapsed rows from the CH lake. Extend lint-pk-discriminators to enforce the discriminator. Largest *latent* data-loss class.
15. **F-1337 — API-key URL leakage.** Move keys to headers (CoinGecko/Polygon support it) or redact url.Error.URL before wrapping.

## Tier 2 — ops-truth before launch (the docs an operator opens at 3 AM)

16. **F-1330 — incident-pressure docs.** Rewrite rollback.md around `gh workflow run deploy.yml` + `.prev-<tag>`; fix projector-lag/replay SQL (`ingestion_cursors.last_updated`); fix the archive-completeness flags; rewrite price-divergence.md; add single-host posture notes to the ~10 multi-host runbooks; fix the wrong-port PromQL (aggregator=9465).
17. **F-1333 + F-1334 — deployed spec + test rot.** `make docs-api` + commit; add the spec-copy sync check to verify.sh/lint-docs.sh. Fix the k6 scenarios (canonical params, asset_ids body, /assets); flip the integration retention assertions to assert-absent; fix the PG15 literal + the ops integration compile break; add a nightly `-tags=integration` CI job.
18. **F-1331 — binding architecture docs.** Rewrite ingest-pipeline.md to the projector/soroban_events/ClickHouse reality; strip the 90-day retention from ha-plan.md + clickhouse-phase4 (invariant-8). These are the docs the invariants point at.
19. **F-1352 — proposal corrections.** Register the multi-zone/freshness/RPC/containerization divergences in proposal-corrections.md; refresh coverage-matrix.md (re-run the probes — five ❌ cells are now favorably wrong); produce the sla-proof artifact from an external vantage.
20. **F-1357 — ansible destructive bug + rebuild gaps.** Fix the prometheus rules-source path (it deletes all host alert rules on re-apply); add the missing ratesengine user / archive-writer MinIO user / redis / clickhouse provisioning (or document the out-of-band prereqs); fix the loki 3.2 config.

## Tier 3 — post-launch hygiene (correctness refinements + the long tail)

- F-1339/1340/1341 (SEP-40 stale flag, XLM alias coverage, Reflector Observer/oracle-ts clamp), F-1342/1343/1344/1345 (sink retry, projector cursor, divergence quote-key, freeze LKG TTL), F-1346 (perf scans), F-1347 (topic-only Matches pollution), F-1348/1349 (status-page resilience, LiveSink bounds+metrics+lake_entry_changes), F-1351 (spec envelope/enum/Quote contract), F-1353/1354 (ADR markers, dead-package decisions), F-1355/1356 (CLAUDE.md recipe, rule-drift), F-1358 (CI permissions/security.yml/docs-lint/openapi-on-push).
- The ~150 low/info items (stale comments, dead sentinels, DRY, a11y, minor doc drift) in `evidence/` — batch into a small number of mechanical sweep PRs (one per area: source-decoder comments, storage retention-comment drift, web DRY/dead-code, ADR markers, runbook command/metric/port sweep).
- W0-01: bump Go toolchain to 1.25.11 + x/net to v0.55 (3 patchable advisories).

## Suggested sequencing note

Tier 0 items 1–6 are each one small PR and independent — land them first, merge-as-you-go. The two systemic backstops (F-1327 config drift-test, F-1329 rule-resolvability CI check, F-1324 PK lint) are worth doing early because they *prevent recurrence* of the largest classes, not just the instances. Doc-truth (Tier 2) is large but low-risk and parallelizable; the ansible destructive bug (F-1357) is the one Tier-2 item that's actually dangerous (silent total alert-rule deletion on the next multi-host apply) and should jump to Tier 1 if R2/R3 bring-up is imminent.
