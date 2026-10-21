---
title: Showcase site deployment (`stellarindex.io`)
last_verified: 2026-10-05
status: operator runbook
---

# Showcase site deployment

`web/explorer` is a Next.js 16 static export (`output: 'export'`): `pnpm build`
writes `web/explorer/out/`, so any static host works. Cloudflare Pages is the
production path.

The browser calls the API **client-side** at this network's `api.*` origin:
`NEXT_PUBLIC_API_BASE_URL` if set at build time, else derived from
`NEXT_PUBLIC_NETWORK` (`src/lib/networks.ts`). `next.config.mjs` inlines no
default: a mainnet default once made a test-net build serve mainnet data. Site
and API stay on separate origins so either serves while the other is down.

## Build locally

```sh
cd web/explorer
pnpm install
pnpm build          # → web/explorer/out/
pnpm exec http-server -p 8080 web/explorer/out   # preview at http://localhost:8080
```

Static-param fetches fail the build hard if the API is unreachable
(`src/lib/buildFetch.ts`); build with the API reachable.

## Recommended: Cloudflare Pages

Publishing is either the dashboard git integration (auto-deploys each push to
`main`, no Actions minutes) or the `workflow_dispatch` direct-upload workflow
([Break-glass](#break-glass-explorer-deployyml-wrangler)). Check the project's
Settings, Builds & deployments tab for which one a zone uses.

Git-integration setup:
1. Workers & Pages → Create application → Pages → Connect to Git →
   `Stellar-Index/StellarIndex`.
2. Preset `Next.js (Static HTML Export)`; command
   `cd web/explorer && pnpm install --frozen-lockfile && pnpm build`; output
   `web/explorer/out`; root directory = repo root; Node 24; production branch
   `main`, PR previews on. No compatibility flags.
3. Env `NEXT_PUBLIC_API_BASE_URL` = `https://api.stellarindex.io` (override per
   preview environment if wanted).
   Secret `STELLARINDEX_BUILD_API_KEY` (Production environment only, so PR
   previews never see it) = a key minted on that project's network
   (`stellarindex-ops mint-key`). The prerender sends it server-side only, so the
   export is held to the key tier, not the anonymous per-IP limit. Unset, the build
   runs anonymous; a rejected key fails the build (`next.config.mjs`).
4. Custom domain `stellarindex.io` (`www.` as redirect); CF proxies the apex.

PR previews land at `<pr-hash>.stellarindex-explorer.pages.dev`; production
deploys ~2 min after merge. `_next/static/*` is fingerprinted, so CF's 1-year
asset TTL is safe; HTML is short-TTL. Roll back with one click in the CF
dashboard.

## Build-time guards

`pnpm build` runs package.json's `postbuild` chain over `out/`; any failure
fails the build. It lives there so every publisher (CF Pages,
`explorer-deploy.yml`, the `web-explorer` CI job, a laptop) runs it.

- `postbuild:pages`: heading outline on every page (WCAG 1.3.1); every
  nav-linked route exports a real frame.
- `postbuild:prune`: deletes Next 16 `__next.*` prefetch files, **keeping
  `__next._tree.txt` and `__next.*__PAGE__.txt`**.
- `postbuild:budget` (`scripts/ci/explorer-file-budget.sh`): fails at 18,500
  files, below CF Pages' hard 20,000 cap.
- `postbuild:seo` (`scripts/ci/explorer-seo-lint.sh`): title, meta description
  and canonical on every indexable page.
- `postbuild:openapi` (`scripts/ci/explorer-openapi-check.sh`):
  `out/openapi/stellar-index.v1.yaml` exists and matches the canonical spec.
- `postbuild:shell-fallback` (`scripts/ci/explorer-shell-fallback-check.sh`):
  every `functions/**/[[path]].js` shell target exists in `out/`.

Prune runs before the budget count: Next 16 emits ~3 prefetch files per page
beyond the two it keeps (~13k files unpruned, ~10.6k kept, measured 2026-10-15).
The router prefetches each route's `__next._tree.txt` and its
`__next.<segments>.__PAGE__.txt`; deleting either logs a 404 on every link hover.
The rest (`_full`, `_head`, `_index`, layout segments) are never requested.
`test/controlwiring/explorer_build_guards_test.go` asserts the chain and the keep.

## Test nets (Testnet / Futurenet)

Each network has its own build (`NEXT_PUBLIC_NETWORK` + `NEXT_PUBLIC_API_BASE_URL`),
Pages project and domain. The nav switcher reads each network's public
`/v1/ledger/tip`.

| Network | Pages project | Custom domain | API origin (grey) |
| --- | --- | --- | --- |
| Mainnet | `stellarindex-explorer` | stellarindex.io | api.stellarindex.io |
| Testnet | `stellarindex-explorer-testnet` | testnet.stellarindex.io | api.testnet.stellarindex.io |
| Futurenet | `stellarindex-explorer-futurenet` | futurenet.stellarindex.io | api.futurenet.stellarindex.io |

Per test net: create the Pages project (git-integrated with `NEXT_PUBLIC_NETWORK`
set, or CI-published; `NEXT_PUBLIC_API_BASE_URL` is optional but must be that
network's own `api.*`, never mainnet's); add the custom domain (orange/proxied,
CF terminates TLS, no origin cert); give `CLOUDFLARE_API_TOKEN` Pages:Edit on it.

```sh
gh workflow run explorer-deploy.yml --ref main -f network=testnet   -f environment=production
gh workflow run explorer-deploy.yml --ref main -f network=futurenet -f environment=production
```

- CSP `connect-src` in `web/explorer/public/_headers` lists all three `api.*`
  origins (one file serves every build; the switcher probes every tip). Missing
  one blocks fetch + EventSource.
- The API's CORS `allowed_origins` includes `stellarindex.io`, `testnet.` and
  `futurenet.` (role template; re-render + restart the API after widening).
- Pricing is mainnet-only (no test-net aggregator): `/v1/price/tip/stream` 404s,
  the explorer gates it on `CURRENT_NETWORK.pricing`, widgets show empty state.

## Security headers + CSP

`web/explorer/public/_headers` is copied into the build. CF Pages applies it to
**static-asset responses only**, and every matching rule applies. `/*` sends:
- `X-Content-Type-Options: nosniff`
- `X-Frame-Options: DENY`, the framing control for non-embed pages. The `/*` CSP
  has **no** `frame-ancestors`: `/embed/*` matches both rules, browsers intersect
  the CSPs, and a `/*` `frame-ancestors` blocked every customer iframe and the
  `/widgets` previews. Do not add one.
- `Referrer-Policy: strict-origin-when-cross-origin`
- `Permissions-Policy` denying accelerometer / camera / geolocation /
  microphone / payment / USB
- `Strict-Transport-Security: max-age=31536000; includeSubDomains` (no `preload`)
- `Cache-Control: public, max-age=0, must-revalidate` on HTML
- `Content-Security-Policy`: `connect-src 'self'` plus
  `https://api.stellarindex.io`, `https://api.testnet.stellarindex.io`,
  `https://api.futurenet.stellarindex.io`; `img-src 'self' data:` (issuer icons
  use the same-origin `functions/icon.js`); `'unsafe-inline'` on `script-src` /
  `style-src` for Next's bootstrap and Tailwind.

`/_next/static/*` adds 1-year `Cache-Control: immutable`. `/embed/*` sends a CSP
with `frame-ancestors *` and no X-Frame-Options; browsers ignore the inherited
`DENY` when a CSP frame-ancestors is present.

**No CSP reporting**, by decision: a hosted collector would be the first runtime
third party, and `/client-errors` logs only the explorer's own beacon fields.
After any `_headers` or API-origin change, load one page per network with the
console open.

### What `_headers` does not cover

CF Pages does not apply `_headers` to Pages Function responses
(`web/explorer/functions/`):
- `/og/*` builds its own `Response` with none of the headers; `/client-errors`
  returns bare status codes (#893).
- Shell-fallback routes (`/accounts/*`, `/assets/*`, `/contracts/*`,
  `/embed/{asset,currency,pair}/*`, `/external/assets/*`,
  `/insights/{creators,sponsors}/*`, `/issuers/*`, `/ledgers/*`, `/lending/*`,
  `/markets/*`, `/sources/*`, `/transactions/*`) set them explicitly:
  `functions/_shared/shellFallback.js` mirrors both `_headers` blocks and
  `shell-fallback.test.js` fails on drift (GH-916).

A new Function must set its own security headers or route through `shellFallback`.

## Break-glass: `explorer-deploy.yml` (Wrangler)

For hotfixes or when the CF git integration is paused. `workflow_dispatch` only;
`environment=production` only from `main` (`scripts/ci/resolve-pages-branch.sh`),
any other ref publishes a preview. `status-page.yml` does the same for status.
Each successful r1 deploy of a stable release also dispatches the mainnet production
deploy from `deploy.yml`, after r1 serves the release, since the prerender crawls the
live API. It waits on the `explorer-production` approval like a manual one.

```sh
gh workflow run explorer-deploy.yml --ref main -f environment=production
gh workflow run explorer-deploy.yml --ref my-branch \
    -f environment=preview \
    -f api_base_url=https://api.staging.stellarindex.io
# by hand (projects are direct-upload):
cd web/explorer && pnpm build && \
  wrangler pages deploy out --project-name stellarindex-explorer
wrangler pages deploy docs/reference/api --project-name stellarindex-docs   # static dir, no build
```

Secrets: `CLOUDFLARE_API_TOKEN` (Pages:Edit on all three explorer projects) and
`CLOUDFLARE_ACCOUNT_ID`.

Alternatives: Vercel / Netlify take the same build command, output and env
(Vercel needs `_headers` translated to `vercel.json`). rsync to the API host
couples site and API availability; air-gapped demos only.

## Verification after deploy

```sh
curl -sI https://stellarindex.io | head -3
curl -sI https://stellarindex.io/sitemap.xml | head -3
curl -sI https://stellarindex.io/assets/XLM/ | head -3   # pre-rendered asset page
curl -s https://stellarindex.io/robots.txt
```

`scripts/ci/site-crawl-check.sh` also fails when the deployed build is behind
`main` (stuck auto-deploy).

See also [cdn-setup.md](cdn-setup.md), [v1-launch-plan.md](v1-launch-plan.md#1-launch-sequence-and-gate-rationale).

## Cloudflare Pages bootstrap

> **DO NOT RUN `scripts/ops/cf-pages-bootstrap.sh` AGAINST THE LIVE ZONE.**
> It is not idempotent and not CI-safe: it creates the **pre-rename** Pages
> projects and upserts the apex record `stellarindex.io` → the
> `stellarindex-showcase` project, which **detaches the live explorer from the
> domain**. It refuses to run unless `CF_BOOTSTRAP_I_UNDERSTAND=1` is set.
> Treat this page as the procedure for standing up a NEW zone.

`status.stellarindex.io` (project `stellarindex-status`) is the
explorer build with `/` rewritten to `/status/`, incident history baked from
`internal/incidents/data` (`EXPLORER_BUILD_OFFLINE=1`) so it publishes during an outage. `stellarindex.io/status` is the explorer's
live-health page; `/incident/*` on the status domain 301s to it.

| Surface | Project | Domain |
|---|---|---|
| Explorer (incl. in-site `/account`) | `stellarindex-explorer` | `stellarindex.io`, `www.stellarindex.io` |
| Status page | `stellarindex-status` | `status.stellarindex.io` |
| API docs | `stellarindex-docs` | `docs.stellarindex.io` |
| Dashboard redirect (retired) | `stellarindex-app` | `app.stellarindex.io` → `301 /account` |
| API (proxied) | (n/a — Caddy on r1) | `api.stellarindex.io` → `136.243.90.96` |

- Project names are immutable; renaming is recreate-move-over (below). Deploy
  workflows (`explorer-deploy.yml`, `status-page.yml`) target the
  `stellarindex-*` names.
- The projects here are **direct-upload, not git-connected**: deploys happen only
  via the `workflow_dispatch` workflows (wrangler `pages deploy`), never on push.
  Creating a git-connected project via the API fails on this account with
  `8000011 internal issue with your Cloudflare Pages Git installation`.
- The standalone dashboard is retired: login and account/keys/usage/settings/admin
  live at `stellarindex.io/account/*`. `app.stellarindex.io` 301s there via
  `stellarindex-app` (one `_redirects` file, `/* https://stellarindex.io/account 301`);
  a zone redirect rule would need a Rulesets-scoped token the deploy token lacks.
Prerequisite (done): Cloudflare's GitHub app authorised against the `StellarIndex`
org (`https://dash.cloudflare.com/<account-id>/pages/new/connect`).

### Run it

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

Re-runs converge existing projects but are unsafe on a live zone (step 4 repoints
the apex); no workflow in `.github/workflows/` invokes the script.

### What it does

1. Verifies the token and looks up the `stellarindex.io` zone (warns + skips DNS
   if the zone isn't on Cloudflare).
2. For each of the three Pages projects: creates it against
   `Stellar-Index/StellarIndex` with `root_dir` / `build_command` / `output_dir` /
   env vars, or PATCHes the existing one to match.
3. Attaches the production custom domain (and `www.` for the showcase).
4. Upserts the four CNAME / A records (proxied).

Projects carry `build_command` / `output_dir`, so a zone whose git integration
is not connected deploys nothing until it is.

### Verify

```sh
open "https://dash.cloudflare.com/${CLOUDFLARE_ACCOUNT_ID}/pages"
dig +short stellarindex.io status.stellarindex.io   # Cloudflare IPs once propagated
dig +short api.stellarindex.io   # → 136.243.90.96 behind orange-cloud
curl -sI https://stellarindex.io | head -3
curl -sI https://status.stellarindex.io | head -3
curl -s  https://api.stellarindex.io/v1/healthz
```

### Enabling in-site login (the `/account` auth flow)

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

### Troubleshooting

- **`source.config.production_branch` rejected** — legacy Pages API; the script
  uses the v4 endpoint. Revert any hand-edit to older field names.
- **Token verification 403** — token lacks `Pages:Edit` / `Zone:Read` / `DNS:Edit`.
- **DNS records create but the site doesn't load** — cert issuance takes ~30 s
  after project create. Check
  `https://dash.cloudflare.com/<account>/pages/view/<project>/domains`.
- **Explorer down or stale build** — see
  [`runbooks/explorer-cf-pages-down.md`](runbooks/explorer-cf-pages-down.md)
  (full outage, edge-function failure, stale deploy past the 20,000-file ceiling).
- **Route missing security headers (CSP, HSTS, X-Frame-Options)** — see
  [What `_headers` does not cover](#what-_headers-does-not-cover).

### Renaming a Pages project (recreate-move-over)

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
   purge them (~1 per push on git-connected projects): loop
   `DELETE …/projects/X/deployments/{id}?force=true` over every page at ~4 req/s,
   then delete the project.

`docs.stellarindex.io` is served by `stellarindex-docs` (`docs/reference/api/`);
the API's RFC-9457 404 body points users there.
