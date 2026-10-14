---
title: Runbook — explorer / Cloudflare Pages outage
last_verified: 2026-09-22
status: ratified
severity: P2
---

# Runbook — Explorer / Cloudflare Pages outage

No Prometheus rule watches the explorer's edge. Detection: the weekly `site-crawl` workflow (`scripts/ci/site-crawl-check.sh`, Mondays 06:20 UTC, or `gh workflow run site-crawl.yml`) opens a `site-crawl-drift` issue; a Cloudflare dashboard/email alert; a customer report. Impact: `stellarindex.io` / `www.` / `testnet.` / `futurenet.` serve errors or a stale build. `api.stellarindex.io` is a separate surface (`docs/operations/cdn-setup.md`) and is unaffected; see `api.md#stellarindex_api_down` if the API is down.

## At a glance

P2, no dedicated alert. MTTR 5-20 min (rollback is a dashboard click). Presentation-layer outage, not a data outage.

## Symptoms

- Cloudflare edge error (522/523/530) or 5xx instead of the site; `site-crawl` failing across the board (a full-site failure points at the edge).
- Edge-function failure: root loads but entity routes 500 or never hydrate. These are CF Pages Functions under `web/explorer/functions/*/[[path]].js` (transactions, accounts, contracts, markets, issuers, insights, `og/[[path]].js`); a Function only breaks the family it owns.
- Silent stale deploy: site serves fine on an old build. Pages hard-caps a deployment at 20,000 files (`docs/adr/0044-explorer-edge-rendering.md`); `scripts/ci/explorer-file-budget.sh` (in `pnpm build`'s `postbuild`) guards it, but a hand-run `wrangler pages deploy` of a stale `out/` bypasses it. Compare the live build to `main`:

  ```sh
  curl -s https://stellarindex.io | grep -o 'name="re-build-sha" content="[^"]*"'
  git -C <repo> rev-parse HEAD   # on main
  ```

  `site-crawl` check 9 ("deployed build freshness") automates this: it fails when the footer commit is more than `STALE_COMMIT_BUDGET` (200) commits or `STALE_HOURS_BUDGET` (72) hours behind `main`. Between weekly runs, compare by hand.

## Quick diagnosis (<= 10 min)

```sh
# 1. Edge or whole zone? If api is fine, the fault is scoped to the explorer's Pages project.
curl -sI https://stellarindex.io/
curl -sI https://api.stellarindex.io/v1/healthz
curl -sI https://status.stellarindex.io/   # standalone incident page, same account

# 2. Full outage vs edge-function failure.
curl -sI https://stellarindex.io/transactions/<any-64-char-hash>
curl -sI https://stellarindex.io/og/<some-asset-slug>

# 3. Deployment state: latest deployment status and commit SHA against `main`.
open "https://dash.cloudflare.com/<account-id>/pages/view/stellarindex-explorer"
```

Project per network (`stellarindex-explorer` / `-testnet` / `-futurenet`): `docs/operations/explorer-deployment.md`. Publishing is either dashboard git integration or `workflow_dispatch` direct-upload (`.github/workflows/explorer-deploy.yml`); check the project's Settings, Builds & deployments tab rather than assuming (the two docs disagree).

## Fix

### Full outage or bad deploy: roll back

Dashboard, project, **Deployments**, last good deployment, **Rollback to this deployment** (no rebuild). If unavailable, re-publish the last good commit (needs the production deploy-approval gate satisfied by a reviewer):

```sh
gh workflow run explorer-deploy.yml --ref <last-good-sha> \
  -f network=mainnet -f environment=production
```

### Edge-function failure (one route family down)

Read the Function logs (project, Functions, Real-time Logs) for `web/explorer/functions/<family>/[[path]].js`. Handlers try `env.ASSETS.fetch` first, so errors are in the shell fallback or, for `og/[[path]].js`, the `satori`/`resvg-wasm` render path. A full redeploy also redeploys Functions; if the cause is not obvious, roll back first and diagnose offline.

- `og/[[path]].js` 503 on every request: check the dashboard-only `OG_DISABLED` environment variable (Settings, Environment variables). `OG_DISABLED=1` short-circuits to 503 `OG image generation is temporarily disabled.`; unset it (or set anything but `1`) once the incident has passed. No deploy needed.
- `og/[[path]].js` 302 to `/og.png`: per-entity render failed and the static card was served (`no-store`). The log line `og render failed` names the cause, usually the font fetch from `fonts.googleapis.com` exceeding its 3 s bound, else a satori/resvg error.

### Silent stale deploy

1. Confirm with the `re-build-sha` comparison.
2. If the last deployment failed or was never attempted (paused git integration), publish fresh; the file-budget guard fails loudly instead of shipping a partial build:

   ```sh
   gh workflow run explorer-deploy.yml --ref main -f network=mainnet -f environment=production
   ```
3. If the guard fails (export over 20,000 files), that is a capacity regression: see `docs/adr/0044-explorer-edge-rendering.md` and prune before forcing a publish.

## Post-incident

If customer-visible per [`sev-playbook.md`](../sev-playbook.md), post a status-page update per [`sev-status-page-update.md`](sev-status-page-update.md).

## Related

- [`docs/operations/explorer-deployment.md`](../explorer-deployment.md), [`docs/adr/0044-explorer-edge-rendering.md`](../../adr/0044-explorer-edge-rendering.md)
- [`scripts/ci/site-crawl-check.sh`](../../../scripts/ci/site-crawl-check.sh) / `.github/workflows/site-crawl.yml`: weekly detection
