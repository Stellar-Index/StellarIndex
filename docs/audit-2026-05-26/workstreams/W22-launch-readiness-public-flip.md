# W22 — Launch readiness, public-flip

## Scope

Everything that must be true before the public flip.

## Inputs

- `docs/operations/public-flip.md`
- `docs/operations/launch-day-checklist.md`
- `docs/operations/pre-launch-hardening.md`
- `docs/architecture/launch-readiness-backlog.md`
- `docs/launch-task-list.md`
- `docs/operations/customer-demo-script.md`
- `docs/operations/release-process.md`
- branding / DNS state
- legal: LICENSE, third-party redistribution licences
- billing surface readiness (W33 cross-ref)
- privacy / GDPR posture
- public-flip-strategy memory

## Checks

| # | Check | Method |
| --- | --- | --- |
| W22.1 | public-flip.md step-by-step actually executable today | doc + r1 probe |
| W22.2 | launch-day-checklist.md current + complete | doc audit |
| W22.3 | pre-launch-hardening.md items closed or marked deferred | doc audit |
| W22.4 | customer-demo-script.md: every step works today | execute |
| W22.5 | DNS: ratesengine.net + subdomains correctly point | dig + R1 probe |
| W22.6 | Third-party redistribution licences: CMC, CG, Chainlink, etc. — verified | legal doc audit |
| W22.7 | Billing surface ready (W33) | cross-ref |
| W22.8 | Customer email templates (`deploy/comms/`): per-event content | template audit |
| W22.9 | Privacy / GDPR: data retention + analytics | privacy review |
| W22.10 | Public-flip strategy memory: NEW repo at v1.0; private repo not force-pushed — playbook exists | memory + doc |
| W22.11 | SEO baseline (sitemap, robots, OpenGraph, canonicals) on ratesengine.net | crawl test |
| W22.12 | All W0 findings closed (per 07-remediation-plan.md) | tracker |

## Closure criteria

Every check terminal. No W0 findings open. Findings on any
launch-blocking gap.
