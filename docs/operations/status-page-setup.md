---
title: Public status page at `status.ratesengine.net`
last_verified: 2026-05-12
status: operator runbook
---

# Public status page setup

The Rates Engine public status page lives at
`https://status.ratesengine.net`. The source lives in this repo at
[`web/status/`](../../web/status/) — a static Next.js export
deployed to Cloudflare Pages on every push to `main`.

F-1211 (codex audit-2026-05-12): this doc previously described an
**Upptime-on-GitHub-Pages** pipeline (and SEV playbook entries
referred to a **cstate** scaffold). Both predecessors were
retired in favour of the in-tree Next.js implementation. The
Upptime fork and the `deploy/status-page/cstate/` directory no
longer exist.

## Architecture (current)

```
web/status/                    ← source (Next.js static export)
  src/
    app/                       ← /, /history, /incidents/[slug]
    data/
      components.json          ← canonical component list
      incidents/               ← one JSON per posted incident
    lib/                       ← uptime computation + probe wiring
  next.config.mjs              ← output: 'export'
  public/                      ← static assets, favicon, OG image

deploy:                        ← Cloudflare Pages auto-deploy on push
  trigger:                       push to main
  build:                         `pnpm install && pnpm build`
  output:                        web/status/out
  domain:                        status.ratesengine.net (apex CNAME)
```

The page is **independent of the API by construction** — Cloudflare
Pages is a different provider stack from our Hetzner/AWS/Vultr
origins, so an origin-side outage cannot take the status page down.
The page does not call into the API at runtime; it renders from the
static JSON committed in the repo plus a build-time uptime
calculation from `src/lib/uptime.ts`.

## Posting an incident

1. Create a new file in `web/status/src/data/incidents/` with the
   schema from the closest recent example. Slug is the URL-visible
   identifier; updates live in the same file as a chronological
   array.
2. Update affected component statuses in `components.json` if the
   incident is severe enough to change them.
3. Commit + push to `main`. Cloudflare Pages will deploy the new
   page within ~2 minutes.

The binding runbook for SEV-1 / SEV-2 updates is
[`runbooks/sev-status-page-update.md`](runbooks/sev-status-page-update.md).

## Component status states

Modelled after Atlassian Statuspage:

- **operational** — green; no active incident.
- **degraded_performance** — partial latency / error-rate impact.
- **partial_outage** — major subsystem down but some surface still
  works.
- **major_outage** — API unavailable.
- **under_maintenance** — scheduled; not an incident.

## Programmatic incident sources

The status page does not poll Prometheus directly — that's the
authoritative source for engineers, not the public-facing signal.
SEV declarations are surfaced via the SEV playbook to the on-call,
who then mirrors the relevant status onto the public page. This
keeps the public page editorial (no false-positive flapping) and
the Prometheus dashboards authoritative (no editorial gate).

## CI / deploy

CI runs `pnpm typecheck`, `pnpm lint`, `pnpm build`, and
`pnpm audit --audit-level high` on every push touching
`web/status/`. The Cloudflare Pages project is configured to
deploy from `main` on push; preview deploys fire automatically
for every PR.
