---
title: Public status page at `status.stellarindex.io`
last_verified: 2026-09-29
status: operator runbook
---

# Public status page setup

The live page is `https://stellarindex.io/status`: an explorer route,
[`web/explorer/src/app/status/`](../../web/explorer/src/app/status/)
(`StatusPageClient.tsx` + `incident/[slug]`), published with the rest of
`stellarindex.io` (Cloudflare git integration on push to `main`;
`.github/workflows/explorer-deploy.yml` is the `workflow_dispatch` fallback).
`web/status/` ([README](../../web/status/README.md)) is a redirect-only stub
(`public/_redirects` 301s `status.stellarindex.io/*` to `stellarindex.io/status/*`)
kept so the subdomain and its `stellarindex-status` Pages project keep resolving.
The Upptime and cstate predecessors (`deploy/status-page/cstate/`) no longer exist.

## Architecture (current)

```
internal/incidents/data/      ← canonical incident corpus
  _template.md                  ← required frontmatter shape
  YYYY-MM-DD-<slug>.md          ← one Markdown file per incident
internal/incidents/incidents.go ← go:embed loader behind /v1/incidents
web/explorer/src/app/status/   ← the live page
  StatusPageClient.tsx           ← polls the live API at runtime
  incident/[slug]/               ← per-incident postmortem pages
  src/lib/incidents.ts           ← build-time loader of the same data/*.md
web/status/                    ← redirect-only remnant
```

The page is **NOT independent of the API.** `StatusPageClient.tsx` fetches
`API_BASE_URL` from the visitor's browser: `/v1/status` (polled),
`/v1/status/notices`, `/v1/incidents`, `/v1/diagnostics/ingestion`,
`/v1/diagnostics/backups`, the `/v1/ledger/stream` SSE feed, and each row's own
probe. During an API outage the static shell and build-time incident history
still render (Cloudflare Pages is a different provider stack from the
Hetzner/AWS/Vultr origins), but every live panel degrades to stale/unreachable.

## Posting an incident

1. Copy `internal/incidents/data/_template.md` to a new file
   `internal/incidents/data/<YYYY-MM-DD>-<slug>.md` and fill in
   the YAML frontmatter (`title`, `severity`, `status`,
   `started_at`, `affected_components`).
2. Append the customer-facing body (Identification → Impact →
   Timeline → What we did) per the template.
3. Commit + push to `main`. The explorer redeploys the new
   page within ~2 minutes; once the API binary is also redeployed, webhook
   subscribers receive `incident.sev1` / `incident.resolved` via
   `stellarindex-ops emit-incident`.

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

`ci.yml`'s `web/explorer` job runs `pnpm typecheck`, `pnpm lint`, `pnpm test` and
a trivy scan of `pnpm-lock.yaml` on every PR/push touching `web/explorer/`; the
status route is covered there. The `web/status/` stub has its own `ci.yml` job
(`pnpm typecheck`, `pnpm lint`, trivy) and manual deploy workflow
(`.github/workflows/status-page.yml`), used only to redeploy the redirect project
when the CF git integration needs a hotfix path.
