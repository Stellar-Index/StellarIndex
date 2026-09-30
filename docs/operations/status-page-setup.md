---
title: Public status page at `status.stellarindex.io`
last_verified: 2026-09-29
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
`/v1/incidents`, `/v1/diagnostics/ingestion`, `/v1/diagnostics/backups`,
the `/v1/ledger/stream` SSE feed, and each row's own probe — directly
from the visitor's browser. What survives an API-side outage is
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

`/v1/status` uses four states, computed by `rollupOverall` in
`internal/api/v1/status.go`:

- **ok** — every declared service heartbeat is fresh, the metrics
  backend answered, no page-severity alert is firing and the latency
  SLO holds.
- **degraded** — the API is serving but at least one signal is
  unhealthy: a service reports degraded, the metrics backend is
  unreachable, a page-severity alert is firing, the latency SLO is
  breached, or only some services could be observed.
- **down** — at least one service's heartbeat is older than its
  staleness threshold. A missing heartbeat is `unknown`, not `down`.
- **unknown** — no service could be observed at all. The page also
  shows this locally before its first successful poll.

Each entry in `services[]` is `ok`, `down` or `unknown` on the same
heartbeat rule. The incidents block carries its own `incidents_status`
(`ok`, `degraded`, `unknown`); `unknown` means the alerts query failed
and the counts must not be read as zero.

## Programmatic incident sources

The page has two incident feeds, and only one is editorial:

- **Firing alerts — automatic, no editorial gate.** `/v1/status`
  (`PrometheusStatusBackend.Incidents` in `internal/api/v1/status.go`)
  queries `ALERTS{alertstate="firing"}` (minus the dead-man's switch)
  and publishes up to 16 alerts verbatim: the raw `alertname`, its
  severity (normalised to `page|ticket|informational|unknown`) and its
  `runbook_url`. The page renders them under active incidents with a
  Runbook link. Any alert that fires reaches customers on the next poll
  (30 s, behind a 10 s edge cache) — write alert names and runbook
  URLs as public text.
- **Declared incidents — editorial.** SEV declarations go through the
  SEV playbook to the on-call, who writes the incident file
  (`internal/incidents/data/`) that `/v1/incidents` serves as the
  incident history. That feed carries the customer-facing wording.

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
