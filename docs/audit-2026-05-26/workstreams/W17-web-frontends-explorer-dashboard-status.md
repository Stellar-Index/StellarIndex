# W17 — Web frontends

## Scope

`web/explorer/`, `web/dashboard/`, `web/status/`.

## Inputs

- per-frontend src/, build config, wrangler.toml, package.json
- pnpm lockfiles
- Cloudflare Pages deploy workflows
- `docs/operations/cf-pages-setup.md`,
  `docs/operations/explorer-deployment.md`

## Checks per frontend

| Check | Result | Evidence |
| --- | --- | --- |
| 1. Pages enumerated; rendered output vs API contract | | |
| 2. API client wrapper(s) (retry, error handling, request-id propagation) | | |
| 3. Dead links to removed `/v1/coins/*`, `/v1/currencies/*` | | |
| 4. SEO (sitemap, robots, meta tags, canonical URLs) | | |
| 5. Verified-badge UI matches catalogue | | |
| 6. Sparkline / chart components match API shape | | |
| 7. Build config (next.config, wrangler.toml, headers, _redirects) | | |
| 8. Cloudflare Pages deploy: cache rules + preview env handling | | |

## Web/dashboard-specific

- Auth: dashboardauth + dashboardkeys
- Stripe surface (W33 cross-ref)
- Usage / billing surface

## Web/status-specific

- Data source (incidents endpoint? cstate?)
- Incident linkage to `docs/operations/incidents/`
- Each "system OK / degraded" indicator's backing data

## Cross-checks

| # | Check | Method |
| --- | --- | --- |
| W17.1 | pnpm-audit clean per frontend | shell |
| W17.2 | Accessibility (axe-core or equivalent) | per-frontend |
| W17.3 | Mobile-first responsive | inspection |
| W17.4 | Per-frontend deploy workflow + cache rules | yaml |
| W17.5 | NEW: explorer reads `/v1/assets/verified` for verified badges | grep + UI |
| W17.6 | NEW: explorer surfaces ATH / sparkline / top-markets (rc.43..rc.46 features) | UI |

## Closure criteria

Three frontend tables filled. Findings on dead links, broken
auth, accessibility gaps.
