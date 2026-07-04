---
title: ADR-0044 Stage 1 spike report — OpenNext on Cloudflare Workers
last_verified: 2026-07-04
status: current
---

# ADR-0044 Stage 1 spike report — OpenNext on Cloudflare Workers

Date: 2026-07-04
Scope: prove an OpenNext build of the CURRENT explorer app (no
route-family changes) renders under `wrangler dev`, per the Stage 1
exit criterion in [ADR-0044](0044-explorer-edge-rendering.md).
Everything here ran locally; nothing was deployed.

**Verdict: GO for Stage 2.** The build worked on the first attempt,
the full route inventory renders under `wrangler dev`, the worker
bundle is 2.5 MB gzipped (limit 15 MB paid / 10 MB free), and the
on-demand-SSR promise at the heart of the ADR is demonstrated: an
issuer that today hard-404s (beyond the top-100 prerender) rendered a
full, real page in 529 ms against the live API with zero code changes.

---

## What was built

| Piece | Value |
|---|---|
| `@opennextjs/cloudflare` | 1.20.1 (wraps `@opennextjs/aws` 4.0.2) |
| `wrangler` | 4.107.0 (workerd 1.20260701.1) |
| Next.js | 16.2.9 (Turbopack) — **supported**: adapter peer range is `>=15.5.18 <16 \|\| >=16.2.6` |
| Build command | `pnpm build:worker` → `OPEN_NEXT=1 opennextjs-cloudflare build -c wrangler.ssr.jsonc` |
| Preview command | `pnpm preview:worker` → `opennextjs-cloudflare preview -c wrangler.ssr.jsonc` |

Files added/changed (all in `web/explorer/`, diff-minimal):

- `next.config.mjs` — `output: 'export'` is now gated: omitted when
  `OPEN_NEXT=1`, unchanged default otherwise. The static CF Pages
  path is untouched.
- `open-next.config.ts` — adapter config; the only non-default is
  `incrementalCache: staticAssetsIncrementalCache` (see finding 2).
- `wrangler.ssr.jsonc` — Worker `stellarindex-explorer-ssr`, `main:
  .open-next/worker.js`, assets binding on `.open-next/assets`,
  `nodejs_compat`. Lives NEXT TO the CF Pages `wrangler.toml`; every
  command passes `-c wrangler.ssr.jsonc` explicitly to avoid config
  ambiguity.
- `package.json` — the two scripts above + devDeps + pnpm
  `onlyBuiltDependencies` (workerd/esbuild postinstalls, which pnpm 10
  blocks by default).
- `.gitignore` — `.open-next/`, `.wrangler/`.

## Build results

- **One-shot success.** No adapter errors, no unsupported-version
  bail, no patch failures. `next build` compiled in 13.3 s, TS in
  9.2 s, and prerendered all **3,832** SSG pages against the live API;
  the whole `build:worker` run was **~3.5 min wall**. The buildFetch
  memo layer clearly does its job (the old CF Pages builds took far
  longer).
- **`next/font` (Inter + JetBrains Mono, Google-hosted): no issue.**
  The fetch happens once inside `next build` exactly as on the static
  path; the woff2 files land in `_next/static/media/` and serve from
  the assets binding (verified 200).
- **React Compiler + Turbopack: no interaction with the adapter.**
- **Native modules:** `@resvg/resvg-js` / `satori` / `workers-og` are
  only imported by `functions/og/[[path]].js` (CF Pages Functions),
  which the Next build never sees — nothing native reached the worker
  bundle.

### Bundle size vs Worker limits

`wrangler deploy --dry-run` on the built worker:

```
Total Upload: 16025.09 KiB / gzip: 2513.83 KiB
```

**2.5 MB compressed — 17% of the free-plan cap (10 MB), 8× headroom
under the paid cap (15 MB).** Not a risk axis; the server bundle
scales with code, not with entity count.

### Where the prerendered pages actually go (the one real surprise)

The OpenNext build puts **zero** page HTML into `.open-next/assets`
(209 files: `_next/static`, public/ files, `_redirects`, `_headers`).
All 3,832 prerendered pages (7,316 files — HTML + RSC-data pairs, 666
MB) land in `.open-next/cache` and are only servable through an
**incremental cache** implementation. With the default (no-op) cache,
every route — including `/` — would re-render per request, and the
fs-reading pages (blog, research, changelog, status incidents — they
read `docs/*.md` from the repo at render time) would 500 inside
workerd, since those files don't exist in the deployed worker.

Fix (one line, in `open-next.config.ts`): the adapter's bundled
`static-assets-incremental-cache` — read-only, no external service;
`populateCache` copies the entries into the assets upload under
`cdn-cgi/_next_cache/`, and the worker reads them via the ASSETS
binding. After population the assets dir is **7,525 files / 681 MB**.

Consequences to carry into Stage 2:

- Workers static assets cap: **20,000 files** (same number as the
  Pages cap that caused S-024, but here prerender count is a CHOICE —
  the point of ADR-0044 is to shrink the prerender set, not grow it).
  25 MiB per-file limit: largest entry is well under.
- The static-assets cache is read-only: `x-nextjs-cache: MISS` renders
  are NOT persisted (no ISR write-back). Real revalidation needs the
  R2 or KV incremental cache + (optionally) the DO queue — that is the
  Stage 2 cache-policy work, not a spike blocker.

## `wrangler dev` smoke results

Startup ≈ 6 s to Ready. All fetched with trailing slash, as canonical:

| Route | Status | Time | Notes |
|---|---|---|---|
| `/` | 200 | 209 ms first / ~10 ms after | prerender HIT |
| `/assets/` | 200 | 11 ms | HIT |
| `/assets/xlm/` | 200 | 13 ms | HIT; HTML contains live-baked prices (`$0.2078`), "Stellar Lumens" ×10, 54 `self.__next_f` RSC-payload chunks → hydration data present |
| `/markets/` | 200 | 8 ms | HIT |
| `/markets/native~USDC-GA5Z…/` | 200 | 10 ms | HIT |
| `/issuers/` | 200 | 8 ms | HIT |
| `/issuers/{top-100 G…}` | 200 | 8 ms | HIT |
| **`/issuers/{#121, never prerendered}`** | **200** | **529 ms** | **on-demand SSR, full real page** — hard-404 on today's static site. This is the ADR's core claim, proven. Repeat request ~17 ms (in-isolate route cache; no persistence, see above) |
| `/transactions/` | 200 | 7 ms | HIT |
| `/research/` | 200 | 10 ms | HIT |
| `/research/adr/0044/` | 200 | — | fs-read page served from cache |
| `/blog/2026-05-07-shipping-the-ia-restructure/` | 200 | 9 ms | HIT |
| `/status/`, `/protocols/soroswap/`, `/sitemap.xml` | 200 | — | |
| `/coins/xlm/` | 301 → `/assets/xlm/` | 3 ms | **`_redirects` honored** — Workers static assets parses and applies the same file (wrangler logged the parsed rules) |
| `/contracts/CCW67…SJMI75/` | **404** | 8 ms | expected — see friction 1 |
| `/transactions/{64-hex}/` | **404** | — | same class |
| `/issuers/{bogus-but-valid G…}` | **500** | 151 ms | expected — see friction 3 |

Cold-start feel: local isn't representative of production isolates,
but the worker initializes fast (no giant top-level work; 2.5 MB gzip
is small for OpenNext) — nothing suggesting a cold-start problem.

## Frictions found (with fixes / Stage-2 owners)

1. **`dynamicParams = false` shells 404 under the Worker.** Nine
   routes (`transactions/[hash]`, `contracts/[id]`, `ledgers/[seq]`,
   `accounts/[g]`, `protocols/[name]`, `status/incident/[slug]`, the
   three `research/*` details) pin the param set; today the CF Pages
   Functions catch the misses and serve client shells. Those Functions
   do not run under the Worker, so e.g. any real `/contracts/{id}` is
   a 404 (the styled app 404 page, not a blank). **Fix (Stage 2):**
   flip `dynamicParams` to true on the entity routes and let them
   render on demand — which is the entire point of ADR-0044 — keeping
   `false` only for the closed sets (research/status, which prerender
   fully). The `{ id: 'shell' }` sentinel params + shell client
   components get deleted in Stage 4.
2. **Prerendered pages need an incremental cache to be served at
   all** (detail above). One-line config for the spike; a deliberate
   R2/KV + TTL policy decision for Stage 2.
3. **The buildFetch fail-hard contract inverts under SSR.** Request-
   time renders reuse `src/lib/buildFetch.ts`, whose contract is
   "throw so the BUILD fails". At request time that throw is a naked
   `500 Internal Server Error` (21-byte body, no stack in logs) — seen
   on the bogus-issuer probe; a transient API 5xx/timeout would do the
   same for a long-tail entity page. **Fix (Stage 2):** split the data
   layer into build-time (fail-hard, unchanged) vs request-time
   (bounded retry → `notFound()` for authoritative 4xx → a real error
   page + short-TTL `Cache-Control` for transport failure). The ADR's
   "per-request error pages that heal on the next request" needs this
   to be true.
4. **`public/_headers` security headers do NOT apply to worker-
   rendered HTML.** Workers assets applies `_headers`/`_redirects`
   only to responses served from the assets layer (verified: `og.png`
   carries the full CSP/nosniff/DENY set; `/` carries none of it).
   **Fix (Stage 2):** emit the security headers from the app
   (middleware or `headers()` in `next.config`) or at the zone level;
   keep `_headers` for the static files.
5. **pnpm 10 blocks workerd's postinstall** — silent until the binary
   is missing. Fixed via `pnpm.onlyBuiltDependencies` in
   `package.json`.
6. **Two wrangler configs in one directory.** The CF Pages
   `wrangler.toml` must survive until Stage 3, so the Worker config is
   `wrangler.ssr.jsonc` and every command passes `-c` explicitly.
   Cutover cleanup (Stage 4): collapse to a single `wrangler.jsonc`.
7. **Cache-control on SSR misses is wrong for production:** on-demand
   renders currently emit `Cache-Control: s-maxage=31536000` (the SSG
   default — Next thinks these are fully-static routes). Harmless
   locally; MUST be fixed by the Stage-2 per-route-family
   `revalidate`/TTL work before anything sits behind the real edge
   cache.

## Non-issues (checked, fine)

- **`NEXT_PUBLIC_API_BASE_URL`**: inlined via the `env` block at build
  time exactly as today; server-side fetches resolve the same constant
  (`src/api/client.ts`). Runtime-configurable API base (via a Worker
  env var) is optional Stage-2 nicety, not required.
- **Trailing-slash routing**: `trailingSlash: true` kept; canonical
  URLs identical to the static site.
- **Segment-prefetch prune (S-024)**: not needed under the Worker —
  the `__next.*.txt` segment files aren't emitted into assets; the
  prune stays on the Pages deploy path only until Stage 3 removes it.
- **`productionBrowserSourceMaps`**: browser-only; does not bloat the
  worker bundle.

## CF Pages Functions → Worker mapping (documented, not ported)

| Pages Function | Role today | Under the Worker |
|---|---|---|
| `functions/{transactions,contracts,issuers,ledgers,accounts}/[[path]].js` | asset-first, then serve the `/{family}/shell/` client shell (S-022) | **Deleted** — replaced by on-demand SSR once `dynamicParams` flips (friction 1). No porting: the worker already routes these families. |
| `functions/og/[[path]].js` | 1200×630 OG PNG via `workers-og` (satori + resvg-wasm), live price fetch, 60 s edge cache | Becomes a **route handler** (`src/app/og/[...path]/route.ts`) inside the worker, or Next's native `ImageResponse`. Only Function needing a real port; wasm-in-worker size impact to measure in Stage 2 (bundle headroom is large). |

## Stage 2 task list (concrete)

1. Incremental cache for real: R2 incremental cache (+ regional cache
   wrapper) or KV; decide whether ISR write-back is wanted or whether
   short-TTL full re-render is simpler. Add the DO queue only if
   time-based `revalidate` is adopted.
2. Per-route-family cache policy: add `export const revalidate` /
   fetch-level `next.revalidate` per ADR TTL table (entities 30–300 s,
   curated pages long) — this also fixes friction 7.
3. Flip `dynamicParams` on entity families (assets, markets, issuers,
   contracts, tx, ledgers, accounts); shrink `generateStaticParams` to
   a small warm set (or empty) — the 20k assets-file ceiling is then
   governed by prerender count we choose.
4. Split `buildFetch` into build-time vs request-time contracts
   (friction 3); request-time gets `notFound()` + error-page semantics
   and short-TTL error caching.
5. Security headers from the app or zone (friction 4).
6. Port the OG Function into the app; delete the five shell Functions.
7. Worker observability: logpush/analytics → the request-level error
   alerting the ADR requires (observability block is already enabled
   in `wrangler.ssr.jsonc`).
8. Shadow deploy on a preview hostname + run the audit crawl suite
   (ADR Stage 2 gate); measure real cold starts and uncached-render
   p95 there.
9. CI: a build-and-smoke job for the `OPEN_NEXT=1` path so the two
   render paths can't drift while both are alive.

## Reproduce

```sh
cd web/explorer
pnpm install
pnpm build:worker     # ~3.5 min; prerender hits the live API
pnpm preview:worker   # wrangler dev on :8787
```
