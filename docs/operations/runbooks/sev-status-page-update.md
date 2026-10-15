---
title: SEV status-page update
last_verified: 2026-10-12
status: living doc
related:
  - docs/operations/sev-playbook.md
  - internal/incidents/data/_template.md
  - web/explorer/src/app/status/
  - scripts/ci/status-pages-redirects.sh
---

# SEV status-page update

Every SEV that meets the threshold below MUST be posted; this is the binding how-to. Two renderers read the same corpus, `internal/incidents/data/<YYYY-MM-DD>-<slug>.md`, at build time:

- `status.stellarindex.io`: the explorer's own build and chrome on a separate Cloudflare Pages project, `stellarindex-status`, deployed by `.github/workflows/status-page.yml`, with `/` serving the explorer's status page. It builds with `EXPLORER_BUILD_OFFLINE=1`, so it makes no build-time API call and still publishes during an outage. Post here first.
- `stellarindex.io/status`: the explorer (`web/explorer/src/app/status/`), with live component health. It shares one `next build` with every explorer page, so an API outage can block it (see step 4).

## When to post and cadence

Post for an incident that a customer might notice from outside (5xx spike, latency SLO breach, `flags.stale` ingest lag, prolonged freshness gap) AND fired a SEV-1 or SEV-2 per [`sev-playbook.md`](../sev-playbook.md). Never post operator-internal incidents (build failures, non-production drift, internal monitoring blips).

| Severity | First update | Subsequent updates |
|---|---|---|
| SEV-1 | At detection (<= 15 min from SEV start) | Every hour |
| SEV-2 | At detection (<= 30 min from SEV start) | Every 24 h |

A SEV-2 that escalates to SEV-1 switches to hourly at the escalation timestamp.

## Steps

### 1. Open a fresh incident file

```sh
cd internal/incidents/data
SLUG=stale-prices                          # short lowercase hyphen-separated
DATE=$(date -u +%Y-%m-%d)
cp _template.md "${DATE}-${SLUG}.md"
$EDITOR "${DATE}-${SLUG}.md"
```

The slug appears in the incident URL (`stellarindex.io/status/incident/${DATE}-${SLUG}`).

### 2. Fill the YAML frontmatter

Match `_template.md`:

```yaml
title: "[SEV-1] API returning stale prices"
date: 2026-05-13
severity: SEV-1
status: investigating
started_at: 2026-05-13T18:00:00Z
resolved_at:                                 # leave empty until resolved
affected_components:
  - api
postmortem:                                  # leave empty until the postmortem is written
```

`affected_components` are badge labels (`api`, `indexer`, `aggregator`, `storage`); reuse the labels operators use. `severity` is `SEV-1`, `SEV-2` or `SEV-3`. `status` is `investigating`, `identified`, `monitoring` or `resolved`.

### 3. Write the first update body

Use the template's `## Identification`, `## Impact`, `## Timeline`. The Timeline table is append-only; never edit prior entries. Plain English from the customer's view, no internal component names. The page is public: no internal infrastructure detail, no cause speculation before "Identified", no naming upstream providers early.

> Bad: "Aggregator is throwing PgError 53300 'too many connections' on the secondary; primary failover initiated."
> Good: "We're seeing elevated error rates on the pricing API; some requests are returning 5xx. Engineers are investigating; next update in 1 hour."

### 4. Commit, push, publish

```sh
git checkout -b sev-${DATE}-${SLUG}
git add internal/incidents/data/${DATE}-${SLUG}.md
git commit -m "incident: ${DATE}-${SLUG} (SEV-${N})"
git push -u origin sev-${DATE}-${SLUG}
gh pr create --title "incident: ${DATE}-${SLUG}" --body ""
gh pr merge --squash --auto
```

After the merge, publish the standalone page (never touches the API):

```sh
gh workflow run status-page.yml --ref main
```

The explorer's Cloudflare Pages git integration rebuilds on the merge into `main` and shows the incident within a minute or two; verify it lands on the index. If the integration is paused: `gh workflow run explorer-deploy.yml --ref main -f network=mainnet -f environment=production`.

**Known risk.** A real API outage can fail the explorer build that would publish the update: `src/lib/buildFetch.ts` aborts the whole `next build` when any real entity page's build-time fetch fails persistently, even though the incident corpus (`web/explorer/src/lib/incidents.ts`) is read from disk. `status.stellarindex.io` is the isolated build that avoids this. If the explorer build fails with `BuildFetchError`, force every OTHER page onto networkless fallback content for this one build (break-glass; never for a routine deploy; those pages stay degraded until the next good build):

```sh
gh workflow run explorer-deploy.yml --ref main -f network=mainnet \
    -f environment=production -f api_base_url=https://ci-stub.invalid
```

### 5. Customer webhook fan-out (optional but expected)

After the incident file is merged AND the API binary is redeployed (the corpus is `go:embed`-baked), fan out the webhook:

```sh
ssh root@136.243.90.96 -- /usr/local/bin/stellarindex-ops emit-incident \
  -config /etc/stellarindex.toml \
  -slug ${DATE}-${SLUG} \
  -event sev1 \
  -write
```

On close, after the merge and deploy that flips `status: resolved`, run the same with `-event resolved`:

```sh
ssh root@136.243.90.96 -- /usr/local/bin/stellarindex-ops emit-incident \
  -config /etc/stellarindex.toml \
  -slug ${DATE}-${SLUG} \
  -event resolved \
  -write
```

It refuses impossible combinations (sev1 on a non-SEV-1 entry, resolved on an investigating one) before any network I/O.

## Append updates

Per the cadence: edit the file, append a Timeline row, flip `status:` if needed, then:

```sh
$EDITOR internal/incidents/data/${DATE}-${SLUG}.md
git add ... && git commit -m "incident: ${DATE}-${SLUG} update HH:MM" && \
  git push && gh pr merge --squash --auto
```

## Resolution

1. Set `status: resolved` and stamp `resolved_at:`.
2. Append the final Timeline row `**Resolved.**` and fill `## What we did`.
3. Commit and push as above; after the deploy lands, run `emit-incident -event resolved -write`.
4. When the postmortem lands at `docs/operations/postmortems/${DATE}-${SLUG}.md`, set the frontmatter `postmortem:` to it; the "Read full postmortem" link (`web/explorer/src/app/status/StatusPageClient.tsx`) stays hidden while it is empty.

## Workstation-down fallback

With R1 reachable by SSH, an incident can be drafted on the host, but it is lost on the next deploy (the corpus rebuilds from git). Commit it upstream within the same hour.

## Related

[`sev-playbook.md`](../sev-playbook.md), [`internal/incidents/data/_template.md`](../../../internal/incidents/data/_template.md), [`web/explorer/src/app/status/`](../../../web/explorer/src/app/status/), [`web/explorer/src/lib/incidents.ts`](../../../web/explorer/src/lib/incidents.ts), [`internal/incidents/incidents.go`](../../../internal/incidents/incidents.go)
