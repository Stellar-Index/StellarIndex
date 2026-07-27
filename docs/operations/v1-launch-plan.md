---
title: v1 launch plan — THE single source of truth
last_verified: 2026-07-27
status: active
severity: P1
---

# Stellar Index — v1 launch plan (single source of truth)

> **⭐ THIS IS THE ONE PLAN.** Consolidated 2026-07-27 from every prior
> launch/production document, with every carried item **re-verified against
> live r1 + the repo on 2026-07-27** (not copied on trust). If you are
> resuming: read §0 (verified state), then execute §2 in order.
>
> Superseded by this doc (banners added; keep for history/recipes only):
> `production-readiness-master-plan-2026-07-18.md` (the campaign log),
> `production-readiness-remaining.md`, `docs/audit/audit-2026-07-16/go-live-master-plan.md`,
> `launch-todo.md`, `launch-day-checklist.md`, `public-flip.md`,
> `public-flip-runbook.md`, `notes/ROADMAP.md`, `notes/BACKLOG.md`.
> Still ACTIVE as companions: `production-confidence-campaign-2026-07-23.md`
> (the adversarial proof harness — its E-gate is §2.6 here) and the
> gitignored `production-remediation-ledger-2026-07-23.md` (finding-status
> authority). Runbooks under `runbooks/` remain the execution recipes.

## OPERATOR INBOX (questions parked by the launch loop — answer inline, loop picks up next iteration)

> Loop contract: no mid-loop questions to Ash. Everything needing an operator
> lands here with context + recommendation + what was done meanwhile.

- **[OP-standing]** The §3 register items all stand (paging wiring 2b is the
  most launch-critical). Nothing new requires a decision yet.
- **[DECIDE-new] Supply-guard dormancy calibration** (replaces most of old
  D4): the `supply-refresh` stalled-observer guard's dormancy horizon
  (17,280 ledgers ≈ 1 day) false-positives on structurally-dormant
  components — SDF reserve accounts (change every days-weeks) and
  slow classic assets (BLND at gap 17,922). My recommendation: raise
  `WithMaxDormantComponentLedgers` for the account-observation component
  (Algorithm 1 XLM) to ~2 weeks, and to ~3 days for classic trustline
  components, keeping the tight default for SEP-41 (where a frozen
  component DID mean a dead writer for 14 days — the guard was RIGHT
  there). Loosening a data-trust guard needs your sign-off; parked. No
  interim action — the alerts are honest until calibrated.
  `SolvBTC_FUNDAMENTAL` / `SolvBTC.BBN_FUNDAMENTAL` are registered quote
  `fiat:USD` but demonstrably publish a NAV **ratio vs BTC** (~1.003 live +
  on-chain; BBN stores exactly 1.0). Correct quote is arguably `crypto:BTC`,
  but changing it rewrites an existing stored series. Recommendation: fix
  quote + one-time series annotation post-v1; NOT launch-blocking (redstone
  is IncludeInVWAP=false). Parked — no action taken beyond documenting the
  distinction in the registry comments.
- **DECIDED (auto, revertible) 2026-07-27:** moved the untracked old-vault
  backup `configs/ansible/inventory/r1.secrets.yml.lost-password-2026-07-27`
  → `~/.config/stellarindex/` (guard-rail script requires a clean tree;
  committing dead ciphertext to the public repo has no value; file preserved).
- **DECIDED (auto) 2026-07-27:** cut **v0.21.1** (sep41 ops-verify
  statement_timeout fix + CI/ansible guard entries; CHANGELOG entries for
  06ff3b5e and the six ops/CI commits added at promote time). This consumes
  the session's one-release budget — the redstone registry fix (below) lands
  on main and ships as v0.21.2 in a LATER session.

## Loop log (newest first)

- 2026-07-27 ~17:25Z (iter 3): the config-assertions unit (activated by
  the apply) fired 3 alerts — **2 were harness bugs, now fixed**
  (`52be4f98`): `galexie_writer_creds_valid` ran `[[ ]]` under dash
  (exit 127 → always FAIL; creds verified VALID by hand — the
  archivewriter fix DID land, so **rehydrate rollback is unblocked**),
  and `zfs_module_on_disk` was blocked by the unit's own
  `ProtectKernelModules=true` (dropped — it disabled a real
  pool-integrity guard and bought nothing here). r1 now 3 fails → 1.
  The survivor `compression_policies_applied` is REAL = §2.3.7,
  already queued. Also filed H1/H4 evidence (injection + error-leak
  spot-audit: PASS, RFC-7807 clean, no banners) and L1/L4 (freshness
  stamps live; `/v1/anomalies` honestly reports
  `divergence_checked:false` — post-v1 integration item).

- 2026-07-27 ~15:35Z (iter 2, cont. 4): drift rounds 3–5 — fixed the
  remaining idempotency classes: galexie/minio dataset dir_mode
  ping-pong (04820ce0) and the migrations sync stamping the
  controller's uid (6b2346e5 — owner/group sync off entirely; one-time
  chown to root on r1). Targeted re-apply now `changed=0` from the
  laptop. Drift-5 pending; expected residual = ONLY the stopped
  compute-completeness.timer (deliberate, §2.1; baseline file's own
  contract forbids parking it — drift goes green when the chain ends).
  sep41_supply rebuild confirmed healthy: 5 parallel CH streams ~9M
  rows each + active PG inserts; counters only tick per completed
  window; overnight ETA stands.
- 2026-07-27 ~14:50Z (iter 2, cont. 3): drift round 2 exposed three
  role idempotency bugs (disable-thp oneshot without RemainAfterExit;
  /var/lib/stellarindex 0750↔0755 mode ping-pong between two tasks;
  migrations rsync stamping the CONTROLLER's uid) — all fixed
  (`660e3b69`), applied to r1 (targeted tags, changed=3), drift round 3
  triggered. Expected residual red: ONLY the stopped
  compute-completeness.timer (self-heals when §2.1 finishes).
- 2026-07-27 ~14:30Z (iter 2, cont. 2): **CI SSH lockout caught+fixed** —
  the exclusive `authorized_keys` apply deleted the post-org-migration
  deploy key (`gh-actions-deploy@stellarindex`), hand-added 2026-07-15
  but never codified → `ansible-drift` went `unreachable=1`. Key
  recovered from the apply's `--diff`, restored on r1, codified in the
  inventory, `R1_INVENTORY_B64` secret updated, drift re-triggered.
  [OP] new sub-item: confirm the old `github-actions-deploy@r1-20260506`
  key is unused and prune it from admin_ssh_keys + r1.
- 2026-07-27 ~13:50Z (iter 2, cont.): **NEAR-MISS during the ansible
  apply** — the role's galexie install task copied a STALE pre-P27
  go-install leftover (`/root/go/bin/stellar-galexie`, June-10
  pseudo-version) over the live v27.0.0 binary; the post-install sha
  assert failed the play (good guard, wrong ordering) with production
  one galexie restart away from re-running the P27 crash-loop.
  RESTORED from the running process inode (sha `045caa5f…` == pin,
  verified); stale artifact renamed `.stale-pre-p27-20260610`; role
  hardened to assert the SOURCE artifact against the pin BEFORE
  install (`9670ef29`). Postgres restart from apply #1 landed clean
  (all services active). Apply #2 running to complete the remaining
  ~49 changes. ALSO: D4 reframed — account_observations is dormant
  reserve accounts, not a stall (see §2.3.2 + OPERATOR INBOX).
  sep41_supply rebuild paused for the apply (resumable, windows
  checkpointed); resumes right after apply #2.

- 2026-07-27 ~13:15Z (iter 2): **MAJOR — sep41 completeness was NOT a
  timeout artifact.** The v0.21.1 full verify finished and exposed a
  REAL zero-writer hole: since the sole-writer deploy (~ledger
  63,419,139 / 2026-07-13) the dispatcher skips the sep41 domain
  (F-1316) while the projector NEVER registered the sep41 sources —
  `BuildRegistry` only builds from `enabled_sources`, and the sep41
  names aren't in `KnownSources`, so no config could carry them. Both
  sep41 tables frozen at 63,419,138; 249k mismatched ledgers, Σ|Δ|=22M.
  FIXED on main (`ae7a082d`: registry always attempts sep41 from the
  watched set + regression test pinning the production shape + the
  reconcile topic0Syms perf fix — full verify was ~35/37 min discarded
  firehose, measured). Catch-up: `projected-rebuild -source sep41_supply
  -from 63419138 -to 63671020 -write -allow-live-overlap` RUNNING
  (justified: live projector provably has no sep41 source);
  sep41_transfers rebuild queued next; then full re-verifies; residual
  tail after v0.21.2 deploys. Ansible 69-task apply reviewed clean in
  check mode (`changed=70` incl. my stopped timer; minio chown is
  non-recursive) — applies AFTER the sep41 rebuilds (postgres restart
  would kill them). Perf report: sep41_supply_events is 276M rows not
  9.3M; needs VACUUM/ANALYZE (42/130 chunks never vacuumed) +
  compression policy (§2.3.7) — full report in loop context, key
  numbers preserved in the commit messages. NOTE: the 39 supply alerts
  "clearing" at 12:30Z was cosmetic (aggregator restart reset counters);
  observers still stalled, D4 still required.

- 2026-07-27 ~12:10Z (iter 1 close): **v0.21.1 DEPLOYED to r1** (all 6
  binaries, edge smoke 13/13, `-ch` copy done). sep41_supply FULL
  compute-completeness RUNNING under heavy wrapper (timer stopped;
  sep41_transfers queued next). Redstone registry fix (19→30 feeds)
  committed to main (`9bfcf5da`, verify.sh green) — ships v0.21.2 next
  session, then replay. **D3 CONFIRMED still required** (r1 table still
  `ReplacingMergeTree(ledger_seq)`, no v2); runner script committed
  (`scripts/ops/d3-lecur-v2-rebuild.sh`, staged on r1). Ordinal probe:
  only partition 38 + [63.0M,63.55M) lack ordinals among trafficked
  bands → pre-D3 step added to §2.3. B1 price sweep broadened + closed
  (campaign doc; gitignored so local-only). DECIDED (auto): savUSD/USDe/
  sUSDe classed crypto per ADR-0028's tradfi-only rwa definition.
  Launch trailhead: `set -a` before sourcing /etc/default/stellarindex
  when hand-running ops binaries (EnvironmentFile vars aren't exported).
- 2026-07-27 ~09:50Z (iter 1): guard cron armed; r1 spot-verified (matches
  §0; soak 5 PASS/0 FAIL). v0.21.1 tagged, release.yml building, deploy
  next. **Redstone §2.4 ROOT-CAUSED** (see §2.4). Redstone registry fix
  being implemented on main.

## What "v1 launch" means

Public, announced availability of the Stellar Index API + explorer as a
production service fit to present to Stellar: substrate certified complete,
served money-values proven correct, a signed repeatable deploy path, honest
capacity/HA posture, paging wired to a human, and the v1.0 wire shape frozen
(ADR-0042 — **Accepted and implemented**; the `kind` discriminator is live in
the spec, so the wire-freeze prerequisite is met).

## 0. Verified current state — 2026-07-27 (all checked live today)

| Area | State |
|---|---|
| Deployed | **v0.21.0**, all 6 binaries, services active, schema **122 clean**. Main is 12 commits ahead — 11 docs + **`06ff3b5e` (sep41 ops-verify statement_timeout fix, NOT yet deployed)** |
| Lake | Dedup complete; post-dedup completeness re-audit PASSED; CH ingest at tip (lag seconds) |
| Galexie trim | Done + verified; cold reads OK. **Soak 4× PASS / 0 FAIL**; snapshot `data/minio@pre-trim-2026-07-26` held (3.2 T) |
| D-series | D1 ✅, D2 ✅ (all partitions, 2026-07-23), CAGG re-mat ✅ ("ALL CAGG REMAT DONE" 2026-07-26). **D3: no run evidence on r1** — confirm need. **D4 NOT run** |
| Supply | `account_observations` **frozen at 63,632,946** (lake tip 63,669,421); guard correctly refusing stale snapshots → **39 `supply_refresh_error_dominant` alerts**. Fix = D4 (§2.3). `seed-sep41-genesis` WAS run 2026-07-26 (overriding the 2026-07-07 "do not run" verdict — verify AQUA in §2.6) |
| Completeness | 3 sources incomplete: `sep41_supply` + `sep41_transfers` (blocked on deploying 06ff3b5e) and **`redstone` — NEW, undiagnosed**: projection blind on 866 ledgers from 63,661,715, "undecodable-but-matched", started ≈ the v0.21.0 deploy window (§2.4) |
| Alerts | Above, plus `dex_nonstandard_decimals_detected` ×5 (informational — genuine non-7dp aquarius C-tokens, working as designed) + deadmansswitch (by design) |
| GH secrets | Deploy + Cloudflare + `R1_INVENTORY_B64` ✅ present. `ANSIBLE_VAULT_PASSWORD` / `ANSIBLE_VAULT_FILE_B64` **absent** |
| **Vault password** | ✅ **REBUILT + ROTATED 2026-07-27.** The old password (clobbered 2026-07-25 by a locally-run CI syntax step) was unrecoverable, so the vault was rebuilt from live r1 rendered values (26 keys; secret-template re-render proven byte-identical), encrypted under a NEW operator-held passphrase, pass file locked (`chflags uchg`), CI clobber-path guarded (2a23698e). Fresh creds generated for not-yet-deployed components (patroni ×2, CH serving profile, pgbackrest repo2 cipher, core placeholder); repo1 cipher + webhook keys empty matching live. Old vault kept as `.lost-password-2026-07-27`. GH secrets `ANSIBLE_VAULT_PASSWORD`/`ANSIBLE_VAULT_FILE_B64` set |
| Config drift | ✅ **`ansible-drift` FUNCTIONAL again** (first complete verdict since the rotation, 2026-07-27): `ok=243 changed=69 failed=0`. Three check-mode bugs fixed en route (timer-enables on unitless hosts e5edb17a/10802588; version-probe skip 2309f4d0 — which also proved the galexie drift-guard constants ALREADY agree, closing that "open operator action"). The red verdict is now REAL drift: **69 changed tasks = the pending config apply** (grown from the "33-task" estimate; incl. archivewriter cred fix, captive-core 18-validator quorum — 24 still live, triangulation chains, z=5.0, cold-tier render, postgres conf, ownership flips, timescale-jobs-probe + CH schema-snapshot units). Apply is §2.2 step 3, [ATTENDED] — service restarts incl. galexie (~1–3 min tip pause) + postgres |
| Deploy gate | `DEPLOY_APPROVAL_RELAXED=true` still set — **re-arm at launch** (§2.7) |
| Feeds | `COINGECKO_API_KEY` **not set** (feed dead since 2026-06-19, [OP]). `min_usd_volume=10000` since 2026-07-01 (older docs claiming 0 are stale) |
| Paging | 🔴 **NOT wired** (corrected 2026-07-27 — the env files exist but every value is EMPTY: 5× `HEALTHCHECKS_URL_*`, `HEALTHCHECKS_DEADMANSSWITCH_URL`, `SLACK_WEBHOOK_URL` all blank; only the node-level `HEALTHCHECK_PING_URL` is populated). Alert pages currently route to nobody — the original [OP] item stands: create Healthchecks.io checks + chat webhooks, paste URLs into `/etc/default/stellarindex-healthchecks` + `/etc/default/alertmanager-secrets` (then codify in the vault), rerun `pre-launch-check.sh` |
| ADRs | 0040–0048 ALL Accepted (incl. **ADR-0042 v1 wire shape** — the old "biggest unsigned gate" is resolved). hashdb wired but `enabled=false` on r1 |

## 1. Go-live gate (all must be true)

- [ ] **Supply trustworthy**: `supply_refresh_error_dominant` + `supply_cross_check_divergence` clear (or per-asset justified); AQUA/SEP-41 genesis-seed values spot-checked vs issuance truth.
- [ ] **Completeness green**: all sources `complete=true` (incl. sep41 ×2 + the new redstone gap); `/v1/coverage` two-axis verdict honest.
- [ ] **Prove-it battery passed** (§2.6): reconcile-balances, verify-lake/contiguity/hash-chain, re-derive determinism, price+supply vs external truth, `verify-usd-volume` calibrated.
- [ ] **Config codified = live**: ansible drift check green in CI; the 33-task apply landed.
- [ ] **Security posture**: creds rotated (ratesengine-admin, MinIO, anything session-exposed); approval gate re-armed; accepted-risk list explicitly signed; external security review booked/closed [OP].
- [ ] **DR honest**: off-site backup decision executed or explicitly risk-accepted; restore-drill timer re-enabled; ZFS trim snapshot resolved.
- [ ] **Launch mechanics**: `auth_mode=apikey_optional` (NEVER `apikey` — it 401s healthz/metrics, audit SEC-01); status page + API docs current; SLA definition published; announcement ready; first-24h watch staffed.

## 2. Critical path (dependency-ordered)

### 2.1 The sep41 chain (REVISED 2026-07-27 — deeper than the timeout)
✅ v0.21.1 cut + deployed (all 6 binaries; smoke 13/13; `-ch` copy done).
The full verify then exposed the REAL cause (see loop log iter 2): a
sep41 zero-writer wiring hole since ~2026-07-13. Remaining chain:
1. ✅ sep41_supply projected-rebuild DONE (37m3s, 21,939,833 events
   emitted, 0 decode errors, 250k ledgers — the 14h ETA was counter
   noise; matches the expected Σ|Δ|≈22M).
2. ✅ sep41_transfers projected-rebuild DONE (22m4s, 14,183,347 events,
   0 decode errors, 252,510 ledgers).
3. ✅ sep41_supply re-verify DONE — **fix PROVEN**: mismatched ledgers
   **249,436 → 891**, Σ|Δ| 22,051,087 → 74,269, and the first residual
   is ledger 63,671,021 = exactly the rebuild's `-to` bound. The
   remainder is purely the tail accumulating since the rebuild, which
   only stops growing when v0.21.2 (`ae7a082d`) deploys and live
   projection resumes. sep41_transfers re-verify RUNNING.
   `lake_complete=true` throughout — the archive was never at risk.
4. ✅ sep41_transfers re-verify DONE — same clean shape: **652
   mismatched, first = ledger 63,671,648 = exactly the transfers
   rebuild's `-to` bound.** Both sep41 sources are now correct up to
   their rebuild boundary; the only residual is the tail accruing until
   v0.21.2 deploys. `compute-completeness.timer` RESTARTED (§2.1.4
   done) → drift's last residual item cleared.
5. v0.21.2 (next session: carries `ae7a082d` + redstone `9bfcf5da`) →
   deploy → live sep41 projection resumes → final small rebuild for the
   deploy-gap tail → redstone replay from 63624934 (§2.4).

### 2.2 Restore the vault password → drift → config apply
1. ✅ ~~Vault password~~ — rebuilt + rotated 2026-07-27 (see §0).
2. ✅ ~~GH secrets + drift run~~ — drift functional, verdict `changed=69`.
3. ✅ **Config batch APPLIED 2026-07-27** (~14:00Z, two passes: pass 1
   died on the galexie stale-artifact guard — near-miss documented in
   the loop log, binary restored, role hardened `9670ef29`; pass 2
   clean `ok=259 changed=60 failed=0`). Post-apply battery green:
   all services active, edge smoke 13/13, galexie sha == pin,
   timescale-jobs-probe firing, ch-schema-snapshot/drift armed.
   Drift rounds 2–5 then burned down every idempotency bug (key
   lockout, thp oneshot, mode ping-pong ×3 dirs, migrations
   mtime+ownership) — **drift-5 residual = ONLY the deliberately
   stopped `compute-completeness.timer`** (self-heals at §2.1.4; the
   baseline file's contract rightly refuses to park it). Gate
   effectively met; confirm the first fully-green run after §2.1.
4. ✅ ~~Pass-file protection~~ — `chflags uchg` + ci.yml guard (2a23698e).

### 2.3 Served-tier population batch (heavy; ONE at a time under `run-heavy-job.sh`)
Order matters; each gates the next check. The DO-NOTHING trap applies:
`trades`/`oracle_updates` upserts never overwrite — corrections DELETE first.
1. ✅ D3 CONFIRMED required (2026-07-27: engine still
   `ReplacingMergeTree(ledger_seq)`, no v2 table). Runner:
   `scripts/ops/d3-lecur-v2-rebuild.sh` (staged on r1 at
   /usr/local/sbin). **Pre-step first**: ordinal probe found partition 38
   + [63.0M,63.55M) un-ordinaled — run `ch-backfill` re-derive over those
   two bands (38M band needs `-bucket galexie-archive`; 63.0–63.55M is in
   the live bucket) so D3's tie-break actually bites there, THEN
   d3 setup → reproject [38M→tip] → verify → cutover. Doc:
   `deploy/clickhouse/ledger_entries_current_intra_ledger_seq.sql`.
2. **D4 REFRAMED (2026-07-27 investigation)** — `account_observations` is
   NOT stalled: it holds exactly the 16 SDF reserve accounts, whose last
   changes are legitimately sparse (dormant by design; trustline/LP/SAC
   observation siblings are all AT TIP). The 39 supply alerts decompose:
   (a) SEP-41 assets → the sep41 zero-writer bug (fix in flight, §2.1);
   (b) XLM + slow classic assets (e.g. BLND, gap 17,922 vs horizon
   17,280) → the supply-refresh dormancy horizon (~1 day) is too tight
   for structurally-dormant components. No replay needed for (b) — a
   replay would re-derive identical rows. Remaining D4 action: after the
   sep41 chain clears, re-count the surviving alerts; for those, decide
   the `WithMaxDormantComponentLedgers` calibration (see OPERATOR INBOX
   [DECIDE-new]).
3. `supply seed-sac-balances -full-history` (may alone clear PHO/KALE/BLND
   cross-check divergence).
4. `projector-replay -source redstone -from 63624934` (after the v0.21.2
   registry fix deploys — §2.4; then re-run redstone compute-completeness
   including the false-clean [63,624,934, 63,661,714] range).
5. `ch-participant-backfill -from 2 -window 500000` (~2–4 d, resumable —
   queued since 2026-07-07; incoming-ops surface is ~1-day-only until run).
6. `MATERIALIZE idx_lecur_account_id` (off-peak) + bloom index only if the
   bound-UNION fix proves insufficient (measure first).
7. TimescaleDB compression policies (`scripts/ops/add-missing-compression-policies.sql`, post-D4); CH system-log TTL at next CH restart.

### 2.4 Investigations (parallel, code-side)
- **redstone projection blind — ROOT-CAUSED 2026-07-27 (not a code
  regression)**: RedStone's relayer expanded past our 19-feed registry on
  2026-07-24 10:56Z (ledger 63,624,934), publishing 11 unknown feed_ids
  (`EUROC` bare, `SolvBTC*_FUNDAMENTAL/USD` variants, `USDe`, `sUSDe`,
  `USDY_FUNDAMENTAL/USD`, `USST_FUNDAMENTAL`, `savUSD_FUNDAMENTAL`,
  `XAUm_FUNDAMENTAL/USD`, `deJAAA/deJTRSY_FUNDAMENTAL/USD`). All-unknown
  batches → `ErrEmptyUpdates` → undecodable-but-matched. v0.21.0's C4-059
  (6c51c760) only made pre-existing blindness VISIBLE; the pre-deploy range
  [63,624,934, 63,661,714] (~4,276 ledgers) was **false-clean** and needs
  re-verify after replay. FIX: registry + canonical-asset additions with
  per-feed quote/orientation diligence (in progress on main → ships
  v0.21.2); THEN `projector-replay -source redstone -from 63624934` (added
  to §2.3 queue). Fail-closed behavior worked as designed; optional
  hardening = distinct "registry stale" signal (post-v1).
- sep41 completeness 40-min count perf (non-blocking follow-up).

### 2.5 Soak close-out (timed gate: after 2026-07-28 ~17:00)
_Status 2026-07-27 16:45Z: 5 PASS / 0 FAIL, timer active, snapshot
3.23 T held. Needs ≥8 PASS — on the current cadence that lands before
the deadline; the loop executes this gate automatically (time+evidence,
not operator)._
If `grep -c FAIL /var/log/galexie-soak.log` = 0 and ≥8 PASS:
`zfs destroy data/minio@pre-trim-2026-07-26` (reclaims 1.07 T) +
`systemctl disable --now galexie-soak-check.timer`. Any FAIL → investigate
cold tier; rehydrate needs 2.2's archivewriter fix first.

### 2.6 Prove correctness (Phase E — the go-live evidence pack)
Run the confidence-campaign E-gate end to end and FILE the artifacts:
reconcile-balances (+ N random accounts/trustlines), verify-lake /
contiguity / hash-chain to genesis, compute-completeness all-green,
re-derive determinism proof, prices top-50 vs CoinGecko/Chainlink, supply
vs external truth **including the seeded SEP-41 genesis baselines (AQUA
overstated +15.7% in the 2026-07-07 test — verify the 2026-07-26 seed
doesn't serve that)**, first `verify-usd-volume -days 30` → calibrate the
C4-055/066 alert. Also: SEV-1/2 paging drill + rollback rehearsal —
evidence files have never been produced across three generations of plans.

### 2.7 Security + launch hardening
- Rotate `ratesengine-admin` + MinIO creds (session-exposed); confirm vault
  passphrase rotation; re-enable restore-drill timer.
- `gh variable delete DEPLOY_APPROVAL_RELAXED` + r1 environment
  Required-reviewers (re-arm the deploy approval gate).
- [OP] sign the 15 accepted-risk candidates (tail-triage-2026-07-26.md);
  decide IP rotation + SSH CIDR narrowing (C6-041).
- [OP] CoinGecko Pro key; hashdb `enabled=true` first-deploy opt-in.
- [OP] External security review; off-site backup decision (§4).
- [OP] Book/verify: `security@stellarindex.io` mailbox actually exists.

### 2.8 Launch execution
Refreshed launch-day sequence (the old checklist's CalVer/public-flip steps
are obsolete — repo has been public since 2026-07-03):
1. Tag the launch release (SemVer), deploy via the re-armed gate.
2. Confirm `auth_mode=apikey_optional`; external SLA-probe smoke with a
   `sip_` key; outside-internet `make smoke` 13/13.
3. Status page + API docs + SLA/error-budget page current; F-0100
   counter-presence PromQL sanity; Grafana launch-watch board from
   `post-launch-queries.md` (refresh metric names first).
4. Announcement; open the first-24h watch (every alert = SEV-2 minimum).

### 2.9 Explicitly deferred to post-v1 (decide, don't drift)
- **HA / R2+R3 / ClickHouse HA** — the single-box SPOF ships at v1 as a
  documented accepted risk with tested restore ([DECIDE] — the standing
  recommendation: R1 + one warm standby bootstrapping from the verified
  snapshot, post-launch). R1 is NOT hardware-upgradeable — never propose drives.
- CH Phase 8 `soroban_events` decommission (#39 — destructive, LAST;
  enumerate live readers first), monthly galexie trim timer, `/v1/tx`
  10.2B tx_hash_index backfill, contract_events_daily v2 swap
  (`feat/ced-v2-rebuild` branch — land WITH the rebuild), CEX dust DELETE
  (#68), P4 tail (i128 lint tooling, strkey/SCVal stubs, ADR-0025 CF-range),
  email-verification flip-on, site-promised features (order-book depth /
  DEX TVL / per-token oracles) [DECIDE build-or-drop], residual DeFi
  decoders, team-asks (§5), the "road to top-tier" ambition set (explorer
  depth, point-lookup path, generic Soroban decoding).

## 3. [OP] register (operator-only, consolidated + deduplicated)

1. **Vault password re-entry** (blocks §2.2). In a Claude Code session run:
   `! mkdir -p ~/.ansible && read -s VP && echo -n "$VP" > ~/.ansible/r1_vault_pass && chmod 600 ~/.ansible/r1_vault_pass && unset VP`
   …then have the agent verify decrypt + set the two GH secrets.
2. CoinGecko Pro purchase → `COINGECKO_API_KEY` on r1 + indexer restart.
2b. **Wire paging** (go-live gate): Healthchecks.io checks (5 per-binary +
   deadmansswitch) + chat webhook(s); paste into the two env files on r1 AND
   the vault; rerun `pre-launch-check.sh` → 0 fails.
3. External security review engagement.
4. Accepted-risk sign-off (15 items) + IP-rotation/SSH-CIDR decision.
5. pgbackrest retention number + off-site S3 provider (+account/creds).
6. HA v1-or-fast-follow decision (§2.9).
7. Stripe: C3-081 reconcile needs SDK + `[billing]` seam — deferred unless
   paid tiers ship at launch.
8. Team-asks (never sent — forward): Aquarius pool-set authority; DeFindex
   vault registry + 9 unproven emitters; Phoenix pool→stake map; Blend V1
   backstop address/emitter schema.

## 4. Open decisions ([DECIDE])

| Decision | Recommendation |
|---|---|
| CH backup posture | ADR-0043 §2.1 schema+state snapshot + re-derive (ledger direction); do NOT resurrect `clickhouse-backup` full-lake copies. Apply the drafted §2.3 amendment to the ADR. Warm standby is the real RTO answer |
| HA at v1 | Accepted-risk + tested restore at v1; warm standby fast-follow |
| Genesis edge [2→287,404] | Accept as documented-unfillable (recover via op-replay if ever needed) |
| Served-tier retention/serve-window policy | Document current reality (projection-scoped windows per source) as the v1 contract |
| Site-promised features (#34 residue) | Build or retract before announcement copy is finalized |
| C4-012/13 third-alias thin-pool VWAP surface | Deliberate review before public traffic |

## 5. Corrections to prior plans (so nobody re-trusts stale rows)

- `min_usd_volume=10000`, ADR-0042 signing, comet gating, deploy/CF secrets,
  k6 cron, branch protection: **DONE** — older docs listing them open are
  wrong. (Healthchecks/Discord wiring is NOT done — see §0 Paging; the env
  files exist but all values are empty.)
- `seed-sep41-genesis`: the 2026-07-07 "❌ do not run" verdict was
  overridden in practice (run 2026-07-26). The honesty check moves to §2.6.
- "Deploy pipeline can't authenticate" / "capacity 94%" / "Phase 0 running":
  resolved-by-events; ignore in superseded docs.
- restore drill "never ran": refuted — PASSED 2026-07-03; the real residuals
  are the disabled timer + cadence drift (§2.7).
- `dex_nonstandard_decimals_detected` firing is informational detection
  working (aquarius C-tokens), not the master plan's "cleared" claim nor a
  regression of the AdjustPrice normalization work.

_Update this file in the same commit as any change that lands or
invalidates an item. One plan; no forks._
