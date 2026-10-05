---
title: Cloudflare Pages — bootstrap
last_verified: 2026-09-02
status: current
---

# Cloudflare Pages — bootstrap

> **DO NOT RUN `scripts/ops/cf-pages-bootstrap.sh` AGAINST THE LIVE ZONE.**
> It is not idempotent and not CI-safe: it creates the **pre-rename** Pages
> projects and upserts the apex record `stellarindex.io` → the
> `stellarindex-showcase` project, which **detaches the live explorer from the
> domain**. It refuses to run unless `CF_BOOTSTRAP_I_UNDERSTAND=1` is set.
> Treat this page as the procedure for standing up a NEW zone.

The live status page is `https://stellarindex.io/status`. `status.stellarindex.io`
and its `stellarindex-status` project are **redirect-only**
(`web/status/public/_redirects` 301s every path to `/status`).

| Surface | Project | Domain |
|---|---|---|
| Explorer (incl. in-site `/account`) | `stellarindex-explorer` | `stellarindex.io`, `www.stellarindex.io` |
| Status page | `stellarindex-status` | `status.stellarindex.io` |
| API docs | `stellarindex-docs` | `docs.stellarindex.io` |
| Dashboard redirect (retired) | `stellarindex-app` | `app.stellarindex.io` → `301 /account` |
| API (proxied) | (n/a — Caddy on r1) | `api.stellarindex.io` → `136.243.90.96` |

- Pages project names are immutable; renaming is recreate-move-over (§Renaming).
  Deploy workflows (`explorer-deploy.yml`, `status-page.yml`) target the
  `stellarindex-*` names.
- The projects are **direct-upload, not git-connected**: deploys happen only via
  the `workflow_dispatch` workflows (wrangler `pages deploy`), never on push.
  Creating a git-connected project via the API fails on this account with
  `8000011 internal issue with your Cloudflare Pages Git installation`.
- The standalone dashboard is retired: login and account/keys/usage/settings/admin
  live at `stellarindex.io/account/*` (explorer routes). `app.stellarindex.io`
  301s to `/account` via `stellarindex-app` (single `_redirects` file,
  `/* https://stellarindex.io/account 301`); a zone redirect rule would need a
  Rulesets-scoped token the deploy token lacks.
- `*.ratesengine.net` domains were detached and return CF 522. To make them
  `NXDOMAIN`, delete that zone's DNS records (separate zone, outside the deploy
  token's scope).

## One-time prerequisite (already done)

Cloudflare's GitHub app authorised against the `StellarIndex` org
(`https://dash.cloudflare.com/<account-id>/pages/new/connect`).

## Run it

```sh
# 1. Scoped API token at https://dash.cloudflare.com/profile/api-tokens
#    Account → Cloudflare Pages → Edit
#    Account → Account Settings → Read
#    Zone    → Zone → Read
#    Zone    → DNS → Edit
#    Zone resources: Include → Specific zone → stellarindex.io
#    Account resources: Include → <your account>

export CLOUDFLARE_API_TOKEN=cf_pat_...
export CLOUDFLARE_ACCOUNT_ID=...

# 2. Dry-run (shows the JSON bodies, no changes):
DRY_RUN=1 bash scripts/ops/cf-pages-bootstrap.sh

# 3. For real — ONLY against a zone that is not live (see the banner):
CF_BOOTSTRAP_I_UNDERSTAND=1 bash scripts/ops/cf-pages-bootstrap.sh
```

Re-runs converge existing projects but are unsafe on a live zone (step 4 below
repoints the apex), and no workflow in `.github/workflows/` invokes the script.

## What it does

1. Verifies the token and looks up the `stellarindex.io` zone (warns + skips DNS
   if the zone isn't on Cloudflare).
2. For each of the three Pages projects: creates it against
   `Stellar-Index/StellarIndex` with `root_dir` / `build_command` / `output_dir` /
   env vars, or PATCHes the existing one to match.
3. Attaches the production custom domain (and `www.` for the showcase).
4. Upserts the four CNAME / A records (proxied).

Afterwards deploys come from the zone's configured publish path. For **this**
zone that is Cloudflare's dashboard git integration, with
`.github/workflows/explorer-deploy.yml` as the `workflow_dispatch`-only
direct-upload fallback. Projects carry `build_command` / `output_dir`, so a zone
whose git integration is not connected deploys nothing until it is.

## Verify

```sh
open "https://dash.cloudflare.com/${CLOUDFLARE_ACCOUNT_ID}/pages"
dig +short stellarindex.io status.stellarindex.io   # Cloudflare IPs once propagated
dig +short api.stellarindex.io   # → 136.243.90.96 behind orange-cloud
curl -sI https://stellarindex.io | head -3
curl -sI https://status.stellarindex.io | head -3
curl -s  https://api.stellarindex.io/v1/healthz
```

## Enabling in-site login (the `/account` auth flow)

The magic-link handlers (`POST /v1/auth/login` + callback + logout) mount only
when `cfg.API.Dashboard.BaseURL` is non-empty AND Postgres is reachable
(`internal/api/v1/server.go`, `s.dashboardAuth != nil`). On r1 `[api.dashboard]`
is absent, so `/v1/auth/login` returns 404 and `/account` shows the signed-out
shell.

1. Set config in `/etc/stellarindex.toml` on r1. `base_url`/`cookie_*` are
   `[api.dashboard]` keys; `allow_credentials`/`allowed_origins` are `[api]` keys.
   Cookie keys under `[api]` crash-loop the API ("config: unknown keys"):

   ```toml
   [api]
   allowed_origins   = ["https://stellarindex.io"]
   allow_credentials = true

   [api.dashboard]
   base_url           = "https://stellarindex.io/account"   # apex, NOT app.
   email_from         = "Stellar Index <hello@stellarindex.io>"
   resend_api_key_env = "STELLARINDEX_RESEND_API_KEY"
   cookie_secure      = true
   # api. must read the apex-issued cookie; pairs with SameSite=None;Secure (rc.109+)
   cookie_domain      = ".stellarindex.io"
   ```

2. Add the Resend key to `/etc/default/stellarindex`, then
   `systemctl restart stellarindex-api`:

   ```sh
   STELLARINDEX_RESEND_API_KEY=re_...
   ```

   Cross-origin cookies need the `SameSite=None` helper added after the rc.109
   tag (commit 9ab579c0), so a **rc.110+ binary is required**; rc.109 sets
   `SameSite=Lax` and the browser drops the cookie on the cross-subdomain call.

## Fallback paths

- **Wrangler CLI deploy** (primary path, projects are direct-upload):
  ```sh
  cd web/explorer && pnpm build && \
    wrangler pages deploy out --project-name stellarindex-explorer
  # docs is a static dir, no build:
  wrangler pages deploy docs/reference/api --project-name stellarindex-docs
  ```
- **GitHub Actions** — `gh workflow run explorer-deploy.yml --ref main`
  (explorer) or `status-page.yml` (status); both `workflow_dispatch`.

## Troubleshooting

- **`source.config.production_branch` rejected** — legacy Pages API; the script
  uses the v4 endpoint. Revert any hand-edit to older field names.
- **Token verification 403** — token lacks `Pages:Edit` / `Zone:Read` / `DNS:Edit`.
- **DNS records create but the site doesn't load** — cert issuance takes ~30 s
  after project create. Check
  `https://dash.cloudflare.com/<account>/pages/view/<project>/domains`.
- **Explorer down or stale build** — see
  [`runbooks/explorer-cf-pages-down.md`](runbooks/explorer-cf-pages-down.md)
  (full outage, edge-function failure, stale deploy past the 20,000-file ceiling).
- **Route missing security headers (CSP, HSTS, X-Frame-Options)** — Pages applies
  `web/explorer/public/_headers` to static assets only, never to a Pages Function
  response. Shell-fallback routes (`accounts`, `assets`, `contracts`, `issuers`,
  `ledgers`, `markets`, `transactions`, `insights/*`, `embed/*`, …) set them
  explicitly: `shellFallback()` mirrors both `_headers` blocks and
  `shell-fallback.test.js` fails on drift (GH-916). `/og/*` and `/client-errors`
  rely on `_headers` alone and remain uncovered (#893). See
  [explorer-deployment.md § What `_headers` does not cover](explorer-deployment.md#what-_headers-does-not-cover).

## Renaming a Pages project (recreate-move-over)

`old-X` → `stellarindex-X`:

1. **Pre-create** the direct-upload project:
   `POST /accounts/{acct}/pages/projects {"name":"stellarindex-X","production_branch":"main"}`
   (omit `source`; git-connected create fails with `8000011`).
2. **Deploy** content (`wrangler pages deploy <dir> --project-name stellarindex-X`
   or the `workflow_dispatch` workflow); verify `stellarindex-X.pages.dev` serves.
3. **Move each custom domain** (a hostname lives on one project):
   `DELETE …/projects/old-X/domains/<host>` →
   `POST …/projects/stellarindex-X/domains {"name":"<host>"}` →
   `PATCH /zones/{zid}/dns_records/{id} {"content":"stellarindex-X.pages.dev"}`.
   Activation + cert take ~30–60 s (transient `522`). Lowest-traffic host first.
4. **Delete the old project.** `8000076 too many deployments` blocks it until you
   purge them (git-connected projects accrue ~1 per push): loop
   `DELETE …/projects/X/deployments/{id}?force=true` over every page at ~4 req/s,
   then delete the project.

`docs.stellarindex.io` is served by `stellarindex-docs` (`docs/reference/api/`);
the API's RFC-9457 404 body points users there.
