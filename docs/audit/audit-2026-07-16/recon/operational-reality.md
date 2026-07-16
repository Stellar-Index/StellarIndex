# Recon: operational reality — deployed vs committed vs running vs CI (2026-07-16)

The audit reads committed HEAD, but exposure/severity depend on what's ACTUALLY deployed + running + gating. This note grounds that. (Requested by operator: "look at uncommitted / undeployed / what's running + check CI.")

## Deployed vs main (undeployed delta)
- **HEAD = `4d034432`; latest tag = `v0.16.3`. Main is 12 commits ahead of v0.16.3** (all undeployed behind the ADR-0047 Phase-0 freeze).
- **R1 actually runs v0.16.0 binaries** (indexer/api/aggregator, 2026-07-11) + a drifted oob ops binary `v0.16.2-11-g33653f64-oob` (2026-07-15). Per DEPLOY-READINESS-v0.16.3: `git diff v0.16.0..v0.16.3` is mostly the org-rename (no runtime behaviour change); migrations identical (both max at 0107; main also 0107).
- **The 3 new lake verifiers (verify-contiguity/hashchain/lake) + reconcile-balances are on main but NOT in any deployed binary** — a post-Phase-0 release (v0.16.4/v0.17.0) is needed to ship them.
- **EXPOSURE RULE for the audit:** a defect introduced by the 12 undeployed commits is **not LIVE on R1 yet** → tag PENDING-DEPLOY (goes LIVE at the next release), not LIVE. A defect in code that predates v0.16.0 and is deployed = LIVE.

## Running config on R1 (differs from code defaults — refines exposure)
Deployment docs are stale (r1-deployment-state.md last_verified 2026-05-12, snapshot v0.5.0-rc.49) so treat as INDICATIVE + verify-items, but the shape is established:
- `auth_mode = apikey_optional` (NOT the code default `none`). Anonymous public at 60/min; keys get budget.
- `min_usd_volume = 0` (stop-gap, on-chain-only era) — **the VWAP volume gate is effectively OFF on R1**. This de-risks CS-040: even if the FX decimals mismatch mis-scaled volume, a 0 floor drops nothing.
- `[external]` = 6 FREE venues enabled: binance, kraken, bitstamp, coinbase, coingecko, ecb. **polygon-forex + exchangeratesapi are NOT enabled** → the connector-path FX sources that trigger CS-040 (IncludeInVWAP:true, AmountDecimals=6) are OFF. **CS-040 = GATED, not LIVE.**
- `enable_stablecoin_fiat_proxy = true`; `usd_pegged_classic_assets = [USDC-GA5Z…]`; `[supply].watched_classic_assets = 8` (USDC/EURC/AQUA/yXLM/VELO/BLND/PHO/KALE); `sdf_reserve_accounts` empty (→ XLM circulating==total, CS-010 basis honesty).
- indexer/aggregator `/metrics` bound to `127.0.0.1:9464/9465` (loopback); nftables default-deny applied (F-1201); Caddy blocks public `/metrics` (F-1223). **→ the /metrics-exposure finding is mitigated on R1** (loopback + firewall + edge block); residual is defense-in-depth only.
- Email verification: code default flipped to require it 2026-05-13; r1 config predates that in the doc → **verify-item** (is it enforced on r1 now?).

## In-flight / uncommitted (NOT covered by the committed-HEAD audit)
### Divergent local branches (mostly STALE)
- `feat/ced-v2-rebuild` (98728bcf) — 0 commits ahead of main (stale; ced-v2 work appears merged or superseded).
- `verify-org-migration` (33d98043) — 1 orphan commit, far behind main.
- `worktree-agent-a1180d8d5b264ffe7` (f665c227) — 1 orphan commit, far behind main.
- `worktree-agent-acb2c028516fe7194` (9394d7a4) — 1 orphan commit "fix(test): chaos/k6 harness no longer masks teardown+setup failures", far behind main. Registered git worktree at .claude/worktrees/.
- **These carry ~1 unmerged commit each on a very old base. Low risk (test/infra), but the single commits should be checked for a lost fix, then the stale branches pruned.**
### 7 stashes (loose UNCOMMITTED work — the "uncommitted code" to consider)
- STALE cross-repo cruft (pre-rename ratesengine paths — safe to drop): stash@{2} `cmd/ratesengine-api/main.go`; likely stash@{1}, stash@{6} (F-#### era).
- **Touch LIVE money/ingest code, uncommitted (review → commit-or-drop, don't leave loose):**
  - stash@{0}: `internal/pipeline/dispatcher.go` + `sink.go` (86+) — labeled ohlc-multi-bar but touches the ingest sink.
  - stash@{1}: `internal/api/v1/history.go` + `internal/storage/timescale/aggregates.go` (212+) — serving path.
  - stash@{3}: "pre-security-bump" `internal/sources/defindex/events.go` + `aggregates.go` (336+) — decoder + aggregates.
  - stash@{4}: `internal/sources/external/binance/streamer.go` (94+) — CEX reconnect.
  - stash@{5}: "coingecko poller bump (other agent's work)" `coingecko/poller.go` (30+).
  - stash@{6}: `internal/sources/external/registry.go` + framework_test (197+) — **the CS-040 registry area**.
- **RISK:** loose stashes on money code are either lost fixes or confusing debt; a `git stash pop` at the wrong time changes behaviour. Recommend the operator triage: commit the real ones onto branches, drop the stale cross-repo ones.

## CI: main has been RED for 24h+ (every push ebec7e67→4d034432) — ROOT-CAUSED
Three failing jobs on the latest main run (id 29528347365 @ 4d034432); the other 10 jobs pass:
1. **prometheus rule validation** — FAILS at `install promtool`: repo secret **`PROM_TARBALL_SHA256` is not set** → promtool never installs → rules never validated. NOT a broken rule. **[OP] set the secret.** Value (from upstream sha256sums.txt): `19700bdd42ec31ee162e4079ebda4cd0a44432df4daa637141bdbea4b1cd8927`.
2. **govulncheck + gitleaks** — FAILS at `gitleaks`: repo secret **`GITLEAKS_TARBALL_SHA256` is not set** → gitleaks never installs. **[OP] set the secret.** Value: `5bc41815076e6ed6ef8fbecc9d9b75bcae31f39029ceb55da08086315316e3ba`.
3. **openapi lint** — FAILS: **Generated Postman collection is STALE** — `examples/postman/*.json` is missing `lake_complete`/`lake_complete_sources` (the two-axis commits 1d56d5b2 + 4d034432 regenerated docs-api + web types but not `make docs-postman`). **FIXABLE by me:** `make docs-postman` + commit (1 mechanical commit → clears the gate).

### Findings this yields (fold into audit CID/OBS)
- **CID (high, LIVE):** two required CI gates (prometheus rule validation, gitleaks secret scan) have been **failing at install for 24h+ across ~11 pushes** because two repo secrets were never set (added by the "F-1294 SHA-verified install" change). The gates are **decorative** — they never run, so broken PromQL or a leaked secret would ship undetected. This is the "gates that can't fail are decorative" class. Nobody gated on it → the recurring red-main the operator flagged.
- **CID (medium, LIVE):** generated-artifact drift discipline is incomplete — `make docs-postman` isn't in `make verify`/verify.sh, so Postman drift only reds CI (which nobody watches). RECIPE CI-parity item #3 predicted this exactly.
- **Implication for remediation baseline:** local `make verify` + `make test-integration` are green, but CI main is red on the 2 secret jobs + Postman drift. Remediation should (a) land the Postman regen first (clears openapi lint), (b) surface the 2 secrets as [OP] with the values above, then every remediation PR can be judged against a main whose only remaining reds are the 2 secret-gated jobs (known-pre-existing infra, safe to merge past per the repo's own admin-bypass policy).
