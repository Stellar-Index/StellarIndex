---
title: Runbook — explorer / Cloudflare Pages outage
last_verified: 2026-09-22
status: ratified
severity: P2
---

# Runbook — Explorer / Cloudflare Pages outage

## At a glance

| Field | Value |
| ----- | ----- |
| Alert | None dedicated — no Prometheus rule watches the explorer's edge (T290 tracks adding an automated stale-deploy diff; until it lands, detection is the sources below). |
| Detected by | The weekly `site-crawl` workflow (`scripts/ci/site-crawl-check.sh`, Mondays 06:20 UTC, or `gh workflow run site-crawl.yml`); a Cloudflare dashboard/email alert on the project; a customer report; or a manual `curl -sI https://stellarindex.io`. |
| Typical MTTR | 5–20 min (rollback is a dashboard click; a stuck deploy needs a re-publish). |
| Impact | `stellarindex.io` / `www.` / `testnet.` / `futurenet.` serve errors or a stale build. `api.stellarindex.io` is a **separate** Cloudflare Pages/Caddy surface (`docs/operations/cdn-setup.md`) and is unaffected — this is a presentation-layer outage, not a data outage. See `api-down.md` if the API itself is down. |

## Symptoms

- `stellarindex.io` (or a network-specific host) returns a Cloudflare
  edge error (522/523/530) or a 5xx instead of the site.
- The weekly `site-crawl` run (`.github/workflows/site-crawl.yml`)
  fails — sitemap sample, canonical, or asset-census checks in
  `scripts/ci/site-crawl-check.sh` start erroring across the board
  rather than on one page family (a full-site failure points at the
  edge, not at content).
- **Edge-function failure** (narrower than a full outage): the root
  page loads but long-tail entity routes 500 or serve the static
  shell forever without hydrating. These paths are served by the CF
  Pages Functions under `web/explorer/functions/*/[[path]].js`
  (transactions, accounts, contracts, markets, issuers, insights,
  `og/[[path]].js` for OG images) — see
  `web/explorer/functions/transactions/[[path]].js` for the shared
  shape (try the static asset via `env.ASSETS.fetch`, else serve the
  shell). A Function erroring only breaks the family it owns; static
  top-level pages keep serving.
- **Silent stale deploy** (no error at all): the site serves fine but
  is frozen on an old build. Cloudflare Pages hard-caps a deployment
  at 20,000 files (`docs/adr/0044-explorer-edge-rendering.md` —
  this happened for 9 days in production, S-024); `pnpm build`'s
  `postbuild` chain runs `scripts/ci/explorer-file-budget.sh` to catch
  this before publish, but a deploy that bypasses that chain (a
  hand-run `wrangler pages deploy` of a stale `out/`, for instance)
  will not. Check for it directly: compare the live build identifier
  against `main`:
  ```sh
  curl -s https://stellarindex.io | grep -o 'name="re-build-sha" content="[^"]*"'
  git -C <repo> rev-parse HEAD   # on main
  ```
  (`web/explorer/src/app/layout.tsx` emits `re-build-sha` /
  `re-build-time` from `NEXT_PUBLIC_BUILD_SHA` /
  `NEXT_PUBLIC_BUILD_TIME` for exactly this comparison.) A mismatch
  beyond the last intended deploy window is the stale-deploy
  incident. This check is manual today — T290 is the open finding to
  automate it.

## Quick diagnosis (≤ 10 min)

```sh
# 1. Is it the edge, or the whole zone? api.stellarindex.io is a
#    different CF setup (see cdn-setup.md) — if it's fine, the fault
#    is scoped to the explorer's Pages project.
curl -sI https://stellarindex.io/
curl -sI https://api.stellarindex.io/v1/healthz
curl -sI https://status.stellarindex.io/   # redirect-only stub, same account

# 2. Full outage vs edge-function failure: root is a static asset,
#    long-tail entity routes go through a Pages Function.
curl -sI https://stellarindex.io/transactions/<any-64-char-hash>
curl -sI https://stellarindex.io/og/<some-asset-slug>

# 3. Deployment state — open the dashboard and check the latest
#    deployment's status and commit SHA against `main`:
open "https://dash.cloudflare.com/<account-id>/pages/view/stellarindex-explorer"
```

Which Pages project depends on the affected network — see the table
in `docs/operations/cf-pages-setup.md` (`stellarindex-explorer` /
`-testnet` / `-futurenet`). Publishing happens either via Cloudflare's
dashboard git integration or via `workflow_dispatch` direct-upload
(`.github/workflows/explorer-deploy.yml`) depending on how the target
project is currently wired — check the project's **Settings → Builds
& deployments** tab in the dashboard rather than assuming; both
`docs/operations/explorer-deployment.md` and
`docs/operations/cf-pages-setup.md` describe one or the other and have
drifted against each other on which is current.

## Fix

### Full outage or bad deploy — roll back

Cloudflare Pages keeps deployment history independent of how a
deployment was published. Dashboard → project → **Deployments** →
find the last deployment that was good → **Rollback to this
deployment**. This is the fastest recovery path and does not require
a rebuild.

If the dashboard rollback is unavailable, re-publish the last known
good commit directly:

```sh
gh workflow run explorer-deploy.yml --ref <last-good-sha> \
  -f network=mainnet -f environment=production
```

This requires the production deploy-approval gate
(`explorer-deploy.yml`'s `Assert the production approval gate is
configured` step) to be satisfied by a reviewer.

### Edge-function failure (one route family down)

Check the Function's own logs in the dashboard (**project →
Functions → Real-time Logs**) for the affected route
(`web/explorer/functions/<family>/[[path]].js`). These handlers defer
to the static asset first and only run their own logic on a 404 from
`env.ASSETS.fetch` — an error here is almost always in the shell
fallback or (for `og/[[path]].js`) the `satori`/`resvg-wasm` render
path, not in the static export. A full redeploy (above) also
redeploys the Functions, so if the cause isn't obvious, roll back
first and diagnose the Function offline.

**`og/[[path]].js` returning 503 for every request**: before treating
this as a Function outage, check the `OG_DISABLED` CF Pages
environment variable on the project (dashboard → **Settings →
Environment variables**). It is a dashboard-only kill-switch (no
redeploy needed either direction) for taking OG image generation
offline under abusive load; `OG_DISABLED=1` makes every request short
-circuit to a 503 with the body `OG image generation is temporarily
disabled.` before any upstream fetch or render runs. If it's set and
the incident that required it has passed, unset it (or set it to
anything other than `1`) to restore image generation — no deploy
required.

### Silent stale deploy (file-ceiling or a bypassed publish)

1. Confirm with the `re-build-sha` comparison under Symptoms.
2. If the dashboard shows the last deployment failed or was never
   attempted (e.g. a paused git integration), trigger a fresh publish
   — `gh workflow run explorer-deploy.yml --ref main -f network=mainnet
   -f environment=production` — which runs the file-budget guard
   (`scripts/ci/explorer-file-budget.sh`) as part of `pnpm build`'s
   `postbuild` chain and will fail loudly instead of shipping a
   partial build.
3. If the guard itself fails (export exceeds 20,000 files), that is a
   capacity regression, not an outage to work around — see
   `docs/adr/0044-explorer-edge-rendering.md` for the long-term fix
   (edge SSR) and prune before forcing a publish.

## Post-incident

If this met the customer-visible threshold in
[`sev-playbook.md`](../sev-playbook.md), post a status-page update per
[`sev-status-page-update.md`](sev-status-page-update.md).

## Related

- [`docs/operations/cf-pages-setup.md`](../cf-pages-setup.md) — project/domain layout, publish paths
- [`docs/operations/explorer-deployment.md`](../explorer-deployment.md) — build + local-preview steps
- [`docs/adr/0044-explorer-edge-rendering.md`](../../adr/0044-explorer-edge-rendering.md) — the 20k-file ceiling, bake-time poisoning, staleness drivers
- [`scripts/ci/site-crawl-check.sh`](../../../scripts/ci/site-crawl-check.sh) / `.github/workflows/site-crawl.yml` — weekly detection
- T290 (open) — automated `re-build-sha`-vs-`main` staleness alerting; not yet built, this runbook's stale-deploy check is manual until it lands
