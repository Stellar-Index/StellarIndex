# Cloudflare Pages — bootstrap

Provisions the customer-facing surfaces on Cloudflare Pages, plus
DNS + the `api.stellarindex.io` proxy:

| Surface | Project | Domain |
|---|---|---|
| Explorer (incl. in-site `/account`) | `ratesengine-showcase` | `stellarindex.io` |
| Status page | `ratesengine-status` | `status.stellarindex.io` |
| API (proxied) | (n/a — Caddy on r1) | `api.stellarindex.io` → `136.243.90.96` |

> **CF project names are `ratesengine-*`, not `stellarindex-*`.** They
> were never renamed in the 2026-06-12 rebrand — CF Pages project names
> are immutable (see "Known discrepancy" below). The deploy workflows
> (`explorer-deploy.yml`, `status-page.yml`) target the real
> `ratesengine-*` names.

> **The standalone customer dashboard was retired (2026-06-17).** Login +
> the account/keys/usage/settings/admin surfaces now live *in-site* at
> `stellarindex.io/account/*` (explorer routes). The old
> `ratesengine-dashboard` project / `app.stellarindex.io` domain is
> redundant — retire it (redirect `app.` → `stellarindex.io/account`,
> then delete the project). See "Retiring the dashboard surface" below.

## One-time prerequisite (already done)

Cloudflare's GitHub app must be authorised against the
`StellarIndex` org once. Visit
`https://dash.cloudflare.com/<account-id>/pages/new/connect` and
click through the OAuth grant. After that, every project this
script creates can wire its `git source` programmatically — no
further dashboard clicks.

## Run it

```sh
# 1. Create a scoped API token at:
#      https://dash.cloudflare.com/profile/api-tokens
#    Permissions:
#      Account → Cloudflare Pages → Edit
#      Account → Account Settings → Read
#      Zone    → Zone → Read
#      Zone    → DNS → Edit
#    Zone resources: Include → Specific zone → stellarindex.io
#    Account resources: Include → <your account>

export CLOUDFLARE_API_TOKEN=cf_pat_...
export CLOUDFLARE_ACCOUNT_ID=...

# 2. Dry-run (shows the JSON bodies, no changes):
DRY_RUN=1 bash scripts/ops/cf-pages-bootstrap.sh

# 3. For real:
bash scripts/ops/cf-pages-bootstrap.sh
```

The script is **idempotent** — re-runs just verify state and
patch any drift. Safe to run from CI on every change to
`scripts/ops/cf-pages-bootstrap.sh` itself.

## What it does

1. Verifies the API token + looks up the `stellarindex.io`
   zone (warns + skips DNS if the zone isn't on Cloudflare).
2. For each of the three Pages projects: creates it pointing
   at `StellarIndex/stellar-index` with the right
   `root_dir` / `build_command` / `output_dir` / env vars,
   or PATCHes the existing project to converge on those
   values.
3. Attaches the production custom domain (and `www.` for the
   showcase).
4. Upserts the four CNAME / A records (proxied) at the zone.

After the first successful run, every push to `main` triggers
an automatic deploy on each Pages project (the git integration
handles it; no GitHub Actions minutes consumed).

## Verify

```sh
# Pages dashboard
open "https://dash.cloudflare.com/${CLOUDFLARE_ACCOUNT_ID}/pages"

# DNS resolution (should return Cloudflare IPs once propagated)
dig +short stellarindex.io status.stellarindex.io
dig +short api.stellarindex.io   # → 136.243.90.96 behind orange-cloud

# Surface health
curl -sI https://stellarindex.io | head -3
curl -sI https://status.stellarindex.io | head -3
curl -s  https://api.stellarindex.io/v1/healthz
```

## Enabling in-site login (the `/account` auth flow)

The magic-link handlers (`POST /v1/auth/login` + callback + logout)
mount **only** when `cfg.API.Dashboard.BaseURL` is non-empty AND Postgres
is reachable (see `internal/api/v1/server.go` `s.dashboardAuth != nil`).
On r1 today `[api.dashboard]` is absent, so `/v1/auth/login` returns 404
and `/account` shows the signed-out shell. To turn login on:

1. **Set the dashboard + cross-origin config** in `/etc/stellarindex.toml`
   on r1. Note the section split — `base_url`/`cookie_*` are
   `[api.dashboard]` keys; `allow_credentials`/`allowed_origins` are
   `[api]` keys. Putting the cookie keys under `[api]` crash-loops the API
   ("config: unknown keys"):

   ```toml
   [api]
   allowed_origins   = ["https://stellarindex.io"]
   allow_credentials = true

   [api.dashboard]
   # In-site account lives on the apex now — NOT app.stellarindex.io.
   base_url           = "https://stellarindex.io/account"
   email_from         = "Stellar Index <hello@stellarindex.io>"
   resend_api_key_env = "STELLARINDEX_RESEND_API_KEY"
   cookie_secure      = true
   # Apex-issued cookie must be readable by the api. subdomain (the
   # browser calls api.stellarindex.io with credentials), so scope it
   # to the parent zone. Pairs with SameSite=None;Secure (rc.109+).
   cookie_domain      = ".stellarindex.io"
   ```

2. **Add the Resend API key** to `/etc/default/stellarindex`:

   ```sh
   STELLARINDEX_RESEND_API_KEY=re_...
   ```

   Then `systemctl restart stellarindex-api`. Cross-origin cookies
   (apex page → `api.` XHR) need the `SameSite=None` cookie helper added
   **after the rc.109 tag** (commit 9ab579c0) — so a **rc.110+ binary is
   required**; rc.109 (currently on r1) still sets `SameSite=Lax` and the
   browser drops the cookie on the cross-subdomain call. Cut + deploy
   rc.110 before flipping login on.

## Fallback paths

- **Per-project Wrangler CLI deploy** — when CF git integration
  is paused (e.g. mid-rotation of the GitHub-app token):
  ```sh
  cd web/explorer && pnpm build && \
    wrangler pages deploy out --project-name ratesengine-showcase
  ```
- **GitHub Actions workflow** — `explorer-deploy.yml` (explorer) and
  `status-page.yml` (status) deploy via `workflow_dispatch`. Trigger via
  `gh workflow run explorer-deploy.yml --ref main`. Both target the real
  `ratesengine-*` project names.

## Troubleshooting

- **`source.config.production_branch` rejected** — you're
  hitting the legacy Pages API; the script uses the v4 endpoint
  which understands the new shape. If you've hand-edited
  `cf-pages-bootstrap.sh` to use older field names, revert.
- **Token verification 403** — token scopes are missing one of
  `Pages:Edit` / `Zone:Read` / `DNS:Edit`. Mint a new token.
- **DNS records create but the site doesn't load** — the
  custom-domain attachment can take ~30 s after project create
  for CF to issue the cert. Check
  `https://dash.cloudflare.com/<account>/pages/view/<project>/domains`.

## Retiring the dashboard surface (`app.stellarindex.io`)

The standalone dashboard was consolidated into the explorer's
`/account/*` routes (2026-06-17). `app.stellarindex.io` /
`ratesengine-dashboard` is now redundant. Retire it gracefully —
**redirect first, delete second** so any bookmark survives:

1. **Redirect `app.` → the in-site account.** A CF Bulk Redirect (or a
   one-line `_redirects` deploy on the existing `ratesengine-dashboard`
   project) sending `https://app.stellarindex.io/* →
   https://stellarindex.io/account` keeps old links working:
   ```
   /*  https://stellarindex.io/account  301
   ```
2. **Once traffic is zero**, detach the `app.stellarindex.io` custom
   domain and delete the `ratesengine-dashboard` Pages project. Project
   deletion is irreversible (re-create + re-attach domain to undo), so
   confirm before running it.

The repo no longer builds the dashboard: `web/dashboard/` was removed,
its CI job (`web-dashboard`) and Makefile targets deleted. No deploy
workflow ever targeted it (CF git integration handled the build), so
removal does not un-deploy the live copy — that's what step 2 is for.

## Known discrepancy: CF projects still named `ratesengine-*`

The repo + every code path now refers to "stellarindex" / "explorer",
but the Cloudflare Pages projects are still `ratesengine-showcase`
(stellarindex.io) and `ratesengine-status` (status.stellarindex.io) —
they were never renamed in the 2026-06-12 rebrand. **CF Pages does not
support project rename via API or dashboard.** A cutover (create new
`stellarindex-*` project → reassign the custom domains → delete old
project) would cause brief per-domain downtime for a CF-internal label
no user ever sees. Not worth it on its own; fold it into the dashboard
retirement above if a migration window opens. The deploy workflows
point at the real `ratesengine-*` names, so deploys work as-is.
