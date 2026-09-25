---
title: Public status page at `status.stellarindex.io`
last_verified: 2026-09-24
status: operator runbook
---

# Public status page setup

> **Moved (2026-06):** the live status page now lives on the main site at
> `https://stellarindex.io/status`. The `status.stellarindex.io` subdomain +
> its `stellarindex-status` CF Pages project still exist but are **redirect-only**
> (`web/status/public/_redirects` 301s every path to `/status`). No DNS change.

The Stellar Index public status page lives at
`https://stellarindex.io/status`. Its source is a route inside the main
explorer app, [`web/explorer/src/app/status/`](../../web/explorer/src/app/status/)
(`StatusPageClient.tsx` + `incident/[slug]`), deployed by
`.github/workflows/explorer-deploy.yml` on every push to `main` alongside
the rest of `stellarindex.io`. `web/status/` (see its
[README](../../web/status/README.md)) is what remains of the earlier
standalone implementation: a redirect-only stub kept only so the
`status.stellarindex.io` subdomain and TLS cert keep resolving.

F-1211 (codex audit-2026-05-12): this doc previously described an
**Upptime-on-GitHub-Pages** pipeline (and SEV playbook entries
referred to a **cstate** scaffold). Both predecessors were
retired in favour of the in-tree Next.js implementation. The
Upptime fork and the `deploy/status-page/cstate/` directory no
longer exist.

## Architecture (current)

```
internal/incidents/data/      ← canonical incident corpus
  _template.md                  ← required frontmatter shape
  YYYY-MM-DD-<slug>.md          ← one Markdown file per incident

internal/incidents/incidents.go ← go:embed loader the API binary
                                  bakes the corpus into for /v1/incidents

web/explorer/src/app/status/   ← the live status page (explorer route)
  StatusPageClient.tsx           ← polls the live API at runtime (see below)
  incident/[slug]/               ← per-incident postmortem pages
  src/lib/incidents.ts           ← build-time loader that reads the
                                  same `internal/incidents/data/*.md`
                                  corpus for incident HISTORY

web/status/                    ← redirect-only remnant; NOT the live page
  public/_redirects              ← 301s status.stellarindex.io/* to
                                  stellarindex.io/status/*

deploy:                        ← Cloudflare Pages, explorer-deploy.yml
  trigger:                       push to main
  domain:                        stellarindex.io/status (same origin as
                                  the rest of the explorer)
```

The page is **NOT independent of the API.** `StatusPageClient.tsx` fetches
`API_BASE_URL` at runtime — `/v1/status` (polled), `/v1/status/notices`,
`/v1/incidents`, `/v1/diagnostics/ingestion`, and each row's own probe —
directly from the visitor's browser. What survives an API-side outage is
narrower than "independent": the static shell (layout, incident history
baked in at build time from the `internal/incidents/data/*.md` corpus)
still renders, but every live panel — overall status, latency, ingest
freshness, the endpoint matrix — degrades to its stale/unreachable state
rather than disappearing, because Cloudflare Pages is a different
provider stack from the Hetzner/AWS/Vultr origins the page is reporting
on.

## Posting an incident

1. Copy `internal/incidents/data/_template.md` to a new file
   `internal/incidents/data/<YYYY-MM-DD>-<slug>.md` and fill in
   the YAML frontmatter (`title`, `severity`, `status`,
   `started_at`, `affected_components`).
2. Append the customer-facing body (Identification → Impact →
   Timeline → What we did) per the template.
3. Commit + push to `main`. The explorer redeploys the new
   page within ~2 minutes; after the API binary is also
   redeployed, dashboard webhook subscribers receive the
   `incident.sev1` / `incident.resolved` callbacks via
   `stellarindex-ops emit-incident` (F-1249, codex audit-2026-05-12).

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

CI (`ci.yml`'s `web/explorer` job) runs `pnpm typecheck`, `pnpm lint`,
`pnpm test`, and a trivy scan of the committed `pnpm-lock.yaml` on every
PR/push touching `web/explorer/` — the status route is covered as part
of that job, not as its own. Deploy is `explorer-deploy.yml` (build +
Cloudflare Pages publish, on push to `main`).

The `web/status/` redirect stub is covered by its own, separate CI job
(`ci.yml`'s `web/status` job: `pnpm typecheck`, `pnpm lint`, trivy) and
its own manual-trigger deploy workflow
(`.github/workflows/status-page.yml`) — kept only to redeploy the
redirect project (`status.stellarindex.io`) when the CF git integration
needs a hotfix path.
