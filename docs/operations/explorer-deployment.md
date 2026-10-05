---
title: Showcase site deployment (`stellarindex.io`)
last_verified: 2026-10-05
status: operator runbook
---

# Showcase site deployment

Shipping `web/explorer` to production. It is a Next.js 16 static export
(`output: 'export'` in `next.config.mjs`): `pnpm build` writes
`web/explorer/out/`, pre-rendered HTML plus fingerprinted assets, so any static
host works. Cloudflare Pages is the production path.

The browser calls the API **client-side** at this network's `api.*` origin:
`NEXT_PUBLIC_API_BASE_URL` if set at build time, else derived from
`NEXT_PUBLIC_NETWORK` (`src/lib/networks.ts`). `next.config.mjs` deliberately
inlines no default for it — a mainnet default once made a test-net build serve
mainnet data. The site and `api.stellarindex.io` stay on separate origins so
either can serve while the other is down.

## Build locally

```sh
cd web/explorer
pnpm install
pnpm build
# → web/explorer/out/   (drop this on any static host)
pnpm exec http-server -p 8080 web/explorer/out   # preview at http://localhost:8080
```

Static-param fetches fail the build hard if the API is unreachable
(`src/lib/buildFetch.ts`); build with the API reachable.

## Recommended: Cloudflare Pages

Git integration auto-deploys every push to `main` (no Actions minutes).

**One-time setup:**
1. Workers & Pages → Create application → Pages → Connect to Git →
   `Stellar-Index/StellarIndex`.
2. Build: preset `Next.js (Static HTML Export)`; command
   `cd web/explorer && pnpm install --frozen-lockfile && pnpm build`; output
   `web/explorer/out`; root directory = repo root; Node 24 (what
   `explorer-deploy.yml` uses); production branch `main`, PR previews on.
3. Env: `NEXT_PUBLIC_API_BASE_URL` = `https://api.stellarindex.io`
   (override per preview environment if wanted).
4. Custom domain `stellarindex.io` (`www.stellarindex.io` as redirect); CF
   proxies the apex, no separate origin.
5. No compatibility flags (`nodejs_compat` not needed).

PR previews land at `<pr-hash>.stellarindex-explorer.pages.dev`. Production
deploys ~2 min after merge. `_next/static/*` is fingerprinted, so CF's 1-year
asset TTL is safe; HTML is short-TTL. Roll back with one click in the CF
dashboard (every build stays available).

## Build-time guards

`pnpm build` runs package.json's `postbuild` chain over `out/`; any failure
fails the build. They live there, not in a workflow, so every publisher (CF
Pages, `explorer-deploy.yml`, the `web-explorer` CI job, a laptop) runs them.

| Script | What it defends |
| --- | --- |
| `postbuild:pages` | Correct heading outline on every page (WCAG 1.3.1); every nav-linked route exports a real frame, not an empty shell. |
| `postbuild:prune` | Deletes Next 16 `__next.*` segment-cache prefetch files, **keeping `__next._tree.txt`**. |
| `postbuild:budget` | `scripts/ci/explorer-file-budget.sh` — fails at 18,500 files, below CF Pages' hard 20,000-file cap. |
| `postbuild:seo` | `scripts/ci/explorer-seo-lint.sh` — every indexable page has a title, meta description and canonical link. |
| `postbuild:openapi` | `scripts/ci/explorer-openapi-check.sh` — `out/openapi/stellar-index.v1.yaml` exists and matches the canonical spec. |
| `postbuild:shell-fallback` | `scripts/ci/explorer-shell-fallback-check.sh` — every `functions/**/[[path]].js` shell target exists in `out/`. |

Prune runs before the budget count (the count is of what ships). Next 16 emits
~8 prefetch files per page (~36k total, past the cap; deploys once failed
silently on it for nine days). Those files are prefetch-only — a miss falls
back to `index.txt`. `__next._tree.txt` must survive: it is the only segment
file the router prefetches, and deleting it 404s every prefetch on routes
without a shell fallback. ADR-0044 (edge SSR) is the real fix.

`test/controlwiring/explorer_build_guards_test.go` asserts the `pnpm build`
chain reaches the guards and that the prune keeps the tree files.

## Test nets (Testnet / Futurenet)

Each network has its own build (different `NEXT_PUBLIC_NETWORK` +
`NEXT_PUBLIC_API_BASE_URL`), Pages project and domain. The nav switcher reads
each network's public `/v1/ledger/tip`.

| Network | Pages project | Custom domain | API origin (grey) |
| --- | --- | --- | --- |
| Mainnet | `stellarindex-explorer` | stellarindex.io | api.stellarindex.io |
| Testnet | `stellarindex-explorer-testnet` | testnet.stellarindex.io | api.testnet.stellarindex.io |
| Futurenet | `stellarindex-explorer-futurenet` | futurenet.stellarindex.io | api.futurenet.stellarindex.io |

**One-time Cloudflare setup per test net:**
1. Create the Pages project. Git-integrate it (set `NEXT_PUBLIC_NETWORK`;
   `NEXT_PUBLIC_API_BASE_URL` is optional but, if set, must be that network's
   own `api.*` — never the mainnet one) or leave it CI-published.
2. Add the custom domain. Its DNS record is orange/proxied, so CF terminates
   TLS; no origin cert.
3. Give `CLOUDFLARE_API_TOKEN` Pages:Edit on the project.

**Publish:**

```sh
gh workflow run explorer-deploy.yml --ref main -f network=testnet   -f environment=production
gh workflow run explorer-deploy.yml --ref main -f network=futurenet -f environment=production
```

Cross-origin requirements:
- CSP `connect-src` in `web/explorer/public/_headers` lists all three `api.*`
  origins (one file serves every build, and the switcher probes every tip).
  Missing one blocks fetch + EventSource: dead odometer, console warnings.
- The API's CORS `allowed_origins` includes `stellarindex.io`, `testnet.` and
  `futurenet.` (role template; re-render + restart the API after widening).
- Pricing is mainnet-only: test nets run no aggregator, so
  `/v1/price/tip/stream` 404s; the explorer gates it on
  `CURRENT_NETWORK.pricing` and pricing widgets show their empty state.

## Security headers + CSP

`web/explorer/public/_headers` (Cloudflare Pages / Netlify format) is copied
into the build. CF Pages applies it to **static-asset responses only** (see
below), and every matching rule applies.

`/*` sends:
- `X-Content-Type-Options: nosniff`
- `X-Frame-Options: DENY` — the framing control for non-embed pages. The `/*`
  CSP deliberately has **no** `frame-ancestors`: `/embed/*` matches both rules,
  browsers intersect the two CSPs, and a `/*` `frame-ancestors` blocked every
  customer iframe and the `/widgets` previews. Do not add one.
- `Referrer-Policy: strict-origin-when-cross-origin`
- `Permissions-Policy` denying accelerometer / camera / geolocation /
  microphone / payment / USB
- `Strict-Transport-Security: max-age=31536000; includeSubDomains` (no `preload`)
- `Cache-Control: public, max-age=0, must-revalidate` on HTML
- `Content-Security-Policy`: `connect-src 'self'` plus
  `https://api.stellarindex.io`, `https://api.testnet.stellarindex.io`,
  `https://api.futurenet.stellarindex.io`; `img-src 'self' data:` (issuer
  icons go through the same-origin `/icon` Function, `functions/icon.js`);
  `'unsafe-inline'` on `script-src` / `style-src` for Next's inline bootstrap
  and Tailwind.

`/_next/static/*` adds 1-year `Cache-Control: immutable` (CF default; explicit
for Netlify). `/embed/*` sends `X-Frame-Options: ALLOWALL` and a CSP with
`frame-ancestors *`, which overrides the inherited `DENY`.

**No CSP reporting**, by decision: a hosted collector would be the first
runtime third party, and `/client-errors` logs only the explorer's own beacon
fields, so a `csp-report` body would log empty. After any `_headers` or
API-origin change, load one page per network with the console open.

### What `_headers` does not cover

CF Pages does not apply `_headers` to Pages Function responses
(`web/explorer/functions/`), even on a matching path:
- `/og/*` builds its own `Response` with none of the headers above.
- `/client-errors` returns bare status codes.
- Shell-fallback routes (`/accounts/*`, `/assets/*`, `/contracts/*`,
  `/embed/{asset,currency,pair}/*`, `/external/assets/*`,
  `/insights/{creators,sponsors}/*`, `/issuers/*`, `/ledgers/*`, `/lending/*`,
  `/markets/*`, `/sources/*`, `/transactions/*`) carry the headers of the
  static shell they fetch via `functions/_shared/shellFallback.js`. Tracked in #916.

A new Function must set its own security headers or route through
`shellFallback`.

## Break-glass: `explorer-deploy.yml` (Wrangler)

For hotfixes or when the CF git integration is paused (e.g. mid-rotation of
the CF GitHub app token). `workflow_dispatch` only; `environment=production`
only from `main` (`scripts/ci/resolve-pages-branch.sh`), any other ref
publishes a preview.

```sh
gh workflow run explorer-deploy.yml --ref main -f environment=production
gh workflow run explorer-deploy.yml --ref my-branch \
    -f environment=preview \
    -f api_base_url=https://api.staging.stellarindex.io
```

Secrets: `CLOUDFLARE_API_TOKEN` (Pages:Edit on all three explorer projects)
and `CLOUDFLARE_ACCOUNT_ID`.

## Alternatives

- **Vercel / Netlify:** same build command, output and env. Netlify reads
  `_headers`; Vercel needs it translated into a `vercel.json` headers block.
- **rsync to the API host:** couples site and API availability; air-gapped
  demos only.

```sh
cd web/explorer && pnpm build
rsync -av --delete out/ root@r1.stellarindex.io:/var/www/showcase/
```

`/etc/nginx/sites-available/showcase`:

```nginx
server {
    listen 443 ssl http2;
    server_name stellarindex.io;
    root /var/www/showcase;
    index index.html;
    location / {
        try_files $uri $uri/ $uri.html =404;
    }
    location /_next/static/ {
        expires 1y;
        add_header Cache-Control "public, immutable";
    }
}
```

## Verification after deploy

```sh
curl -sI https://stellarindex.io | head -3
curl -sI https://stellarindex.io/sitemap.xml | head -3
curl -sI https://stellarindex.io/assets/XLM/ | head -3   # pre-rendered asset page
curl -s https://stellarindex.io/robots.txt
```

`scripts/ci/site-crawl-check.sh` also fails when the deployed build is behind
`main` (stuck auto-deploy).

## Touchpoints

- API CDN: [cdn-setup.md](cdn-setup.md)
- Status page: [`deploy/status-page/README.md`](../../deploy/status-page/README.md)
- [public-flip.md](public-flip.md), [launch-day-checklist.md](launch-day-checklist.md)
