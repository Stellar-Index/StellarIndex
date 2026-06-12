# Cloudflare Pages — bootstrap

Provisions the three customer-facing surfaces on Cloudflare
Pages, plus DNS + the `api.stellaratlas.xyz` proxy:

| Surface | Project | Domain |
|---|---|---|
| Showcase site | `stellaratlas-showcase` | `stellaratlas.xyz` |
| Customer dashboard | `stellaratlas-dashboard` | `app.stellaratlas.xyz` |
| Status page | `stellaratlas-status` | `status.stellaratlas.xyz` |
| API (proxied) | (n/a — Caddy on r1) | `api.stellaratlas.xyz` → `136.243.90.96` |

## One-time prerequisite (already done)

Cloudflare's GitHub app must be authorised against the
`StellarAtlas` org once. Visit
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
#    Zone resources: Include → Specific zone → stellaratlas.xyz
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

1. Verifies the API token + looks up the `stellaratlas.xyz`
   zone (warns + skips DNS if the zone isn't on Cloudflare).
2. For each of the three Pages projects: creates it pointing
   at `StellarAtlas/stellar-atlas` with the right
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
dig +short stellaratlas.xyz app.stellaratlas.xyz status.stellaratlas.xyz
dig +short api.stellaratlas.xyz   # → 136.243.90.96 behind orange-cloud

# Surface health
curl -sI https://stellaratlas.xyz | head -3
curl -sI https://app.stellaratlas.xyz | head -3
curl -s  https://api.stellaratlas.xyz/v1/healthz
```

## After this lands

Two operator steps remain to make the dashboard fully live:

1. **Set the dashboard config block** in `/etc/stellaratlas.toml`
   on r1 so the auth flow mounts:

   ```toml
   [api.dashboard]
   base_url       = "https://app.stellaratlas.xyz"
   email_from     = "Stellar Atlas <hello@stellaratlas.xyz>"
   cookie_secure  = true
   cookie_domain  = ".stellaratlas.xyz"
   ```

2. **Add the Resend API key** to `/etc/default/stellaratlas`:

   ```sh
   STELLARATLAS_RESEND_API_KEY=re_...
   ```

   Then `systemctl restart stellaratlas-api`.

## Fallback paths

- **Per-project Wrangler CLI deploy** — when CF git integration
  is paused (e.g. mid-rotation of the GitHub-app token):
  ```sh
  cd web/explorer && pnpm build && \
    wrangler pages deploy out --project-name stellaratlas-showcase
  ```
- **GitHub Actions workflow** — `showcase-deploy.yml` exists for
  the same case + hotfix-of-arbitrary-commit needs. Trigger via
  `gh workflow run showcase-deploy.yml --ref main -f environment=production`.
  Mirror workflows for dashboard + status are not yet wired (the
  CF git integration removes the need; add when the operator
  pattern actually demands them).

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

## Known discrepancy: CF project still named `stellaratlas-showcase`

The repo + every code path now refers to "explorer", but the
Cloudflare Pages project itself is still `stellaratlas-showcase`.
CF doesn't support project rename. The cutover (create new
`stellaratlas-explorer` project → reassign `stellaratlas.xyz` +
`www.stellaratlas.xyz` custom domains → delete old project) is
tracked separately. User-facing UX is unaffected; the CF-side
label is the only mismatch.
