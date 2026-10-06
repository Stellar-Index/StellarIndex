---
title: Launch-day operator checklist (L6.4 cutover)
last_verified: 2026-05-03
status: operator runbook
---

# Launch-day operator checklist

> **SUPERSEDED by [`v1-launch-plan.md`](v1-launch-plan.md) §2.8.** The public-flip
> steps here already happened (2026-07-03, differently) and the CalVer tag format
> is wrong (we use SemVer). The still-live content (`apikey_optional` warning,
> F-0100 counter-presence check, first-24h watch) is carried in the new plan.

Cutover runbook for **L6.4**. Do not advance until the prior step passes. Every
launch-blocking row is already done when this starts; this flips the public surface
from private staging to production.

## T-7 days — final pre-cut

- [ ] **Open PR pile drained.** `gh pr list --state open --limit 100` shows zero
      launch-blocking entries; leftovers become
      post-launch explicitly.
- [ ] **Last L6.5 docs sweep.** Every `last_verified` within 30 days, every runbook
      `status:` accurate. One PR, merged same day.
- [ ] **External security review (L5.6) findings closed**: a tracked PR or a
      recorded "won't fix, post-launch" per comment.
- [ ] **SEV-1/SEV-2 dry-run (L5.7) done**: staging drill with time-to-detect and
      time-to-mitigate captured, [SEV playbook](sev-playbook.md) updated.
- [ ] **Chaos Wave 1 retrospective (L5.5) clean**: all three scenarios passed,
      `test/chaos/reports/<launch-cut>/RETRO.md` committed, code fixes landed.
- [ ] **Showcase site staged.** `web/explorer/` Pages project connected per
      [`explorer-deployment.md`](explorer-deployment.md), preview deploy
      succeeded, `stellarindex.io` bound but DNS still on staging (cut at T-0
      step 5).

## T-3 days — final freeze

- [ ] **Merge freeze.** No PRs to `main` except `launch-blocker` fixes; announce
      in `#stellar-index` and pin the date.

Struck so the T-1 "all boxes ticked" rule can be met: the public-flip dry-run (repo
already public, no orphan-branch cut to rehearse) and the customer demo (L6.6 maps
to W6.7 announcement copy in `v1-launch-plan.md`; no customers at the 1.0 cut).

## T-1 day — go/no-go

- [ ] **Production environment green** on the SLO board:
      - `stellarindex_aggregator_ticks_total` rising on `outcome="ok"`.
      - `stellarindex_source_events_total` rising for every source with
        `source_enabled=1`.
      - `stellarindex_aggregator_vwap_writes_total` rising.
      - No fired alerts in Alertmanager.
      - **Counter-presence sanity** (F-0100): "no fired alerts" is a false green
        when a counter went stale and the rule sees no-data. Run on prod
        Prometheus; the result must be non-empty for every named family:
        ```promql
        count by (__name__) ({__name__=~"stellarindex_.*_total"})
        ```
        A missing family means silence-not-success: investigate first. Cascade-fragile
        rules carry `absent_over_time(...)` guards (F-0080, F-0085, F-0104); this
        is the manual check for a new rule landing without one.
- [ ] **SLA probe latest pass.** `cmd/stellarindex-sla-probe` against staging ran
      in the last 4 h with `verdict: pass` (or run it per T-0 step 4).
- [ ] **CDN provisioned** per [`cdn-setup.md`](cdn-setup.md): `api.stellarindex.io`
      proxied through Cloudflare, `cf-cache-status` visible on historical surfaces.
- [ ] **Status page provisioned** per [`status-page-setup.md`](status-page-setup.md):
      live at `stellarindex.io/status`; `internal/incidents/data/` has no open
      entries (`status: investigating | identified | monitoring`).
- [ ] **Customer comms approved**, ready to send post-cut.
- [ ] **Rollback rehearsed** via [`rollback.md`](rollback.md): DNS revert,
      rate-limit reset, comms templates known cold.
- [ ] **Go/No-go.** All ticked → GO; any unticked → defer and revisit tomorrow.

## T-0 — cutover

Order matters.

1. **Tag the release (`release-process.md` §Cut).**
   ```sh
   # On private repo first; the tag points at the commit that
   # contains the promoted CHANGELOG block.
   git checkout main && git pull --ff-only origin main
   git tag YYYY.MM.DD.1
   git push origin YYYY.MM.DD.1
   ```

2. **Public-flip (`public-flip.md` §Cut-over mechanics).** Follow the 6 steps; the
   orphan-branch diff at step 4 MUST be zero, else stop and investigate.

3. **DNS flip — `api.stellarindex.io`.** The proxied record is already in place
   from the CDN setup; the "flip" is **enabling the public rate-limit tier**: change
   `[api].auth_mode` from `none` (private staging) to the production value and
   restart the API on each region:
   ```sh
   ansible-playbook -i inventory/r1.yml deploy/ansible/roles/api/restart.yml \
     --extra-vars 'auth_mode=apikey_optional'
   # Repeat for r2, r3.
   ```

   > **The production value is `apikey_optional`, NOT `apikey`.**
   > - `apikey_optional`: no key means ANONYMOUS at the anon rate-limit tier; a
   >   supplied key must be valid and grants its tier. This is what a public read
   >   API for wallets, widgets and SDKs needs, and what r1 has always run.
   > - `apikey`: every request needs a credential. It would take the public API
   >   PRIVATE, and because the auth middleware wraps the whole mux it would also
   >   401 `/v1/healthz`, `/v1/readyz` and `/metrics`, failing load-balancer probes
   >   and Prometheus at once (audit SEC-01).

4. **Smoke test the public surface.**
   ```sh
   STELLARINDEX_PROBE_API_KEY=sip_… \
   stellarindex-sla-probe \
     -base-url https://api.stellarindex.io/v1 \
     -duration 30s \
     -concurrency 4 \
     -report-format text
   ```
   A key is required: without one the probe trips the anonymous-tier limit (60
   req/min) and fails availability for unrelated reasons. Mint a load-test key from
   the operator vault. Pass: `verdict: pass`; any `failed_reasons` halts the cut
   and triggers rollback.

5. **Showcase site goes live (`stellarindex.io`).** Rebuild now that the API is in
   production auth-mode so build-time `generateStaticParams` picks up the live coin
   directory:
   ```sh
   # CF Pages: dashboard → Workers & Pages → stellarindex-explorer
   #          → Deployments → Retry deployment
   #
   # Or push an empty commit:
   git commit --allow-empty -m "chore(showcase): rebuild for launch"
   git push origin main
   ```
   Pass: `curl -I https://stellarindex.io | head -3` is 200 and
   `curl -I https://stellarindex.io/coins/USDC/` is 200 (not 404).

6. **Status page goes live.** If a launch-cut maintenance entry was opened, set
   `status: resolved` in `internal/incidents/data/<DATE>-launch-cut.md` and merge to
   `main`. Verify `stellarindex.io/status` shows no active incidents.

7. **Send customer comms** from
   [`deploy/comms/launch-announcement.md`](../../deploy/comms/launch-announcement.md)
   (email + Discord, project handle if applicable).

8. **Open the L6.7 24-h watch.** On-call clock starts; SLO dashboards stay open
   for 24 h; any alert in the first 24 h is SEV-2 minimum regardless of impact
   (per [release-process post-flight](release-process.md#post-flight)).

## Pass condition for the whole runbook

- Release tag on `main`, on the public repo, and as a GitHub Release.
- `https://api.stellarindex.io/v1/healthz` returns 200 from any external network.
- `https://stellarindex.io` returns 200 with live data in the home Network panel.
- `https://stellarindex.io/status` shows "all systems operational".
- Customer comms delivered; at least one passing SLA probe run against the public
  URL post-cut.

## If anything fails mid-cut

Stop; follow the matching section of [`rollback.md`](rollback.md). Cutover is
reversible up to step 6 (DNS revert is one line); after customer comms (step 7) the
rollback also needs a "we're rolling back" message.

## Cross-references

[`release-process.md`](release-process.md), [`public-flip.md`](public-flip.md),
[`cdn-setup.md`](cdn-setup.md), [`explorer-deployment.md`](explorer-deployment.md),
[`status-page-setup.md`](status-page-setup.md),
[`chaos-wave1-runbook.md`](chaos-wave1-runbook.md), [`rollback.md`](rollback.md),
[`sev-playbook.md`](sev-playbook.md), [`sla-probe.md`](sla-probe.md);
[`v1-launch-plan.md`](v1-launch-plan.md).
