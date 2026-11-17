# Stellar Index — monitoring rule files

Prometheus alerting rules that correspond 1:1 to the rows in
[docs/operations/alerts-catalog.md](../../docs/operations/alerts-catalog.md).
Loaded by AlertManager; routed per
[sev-playbook.md §3](../../docs/operations/sev-playbook.md#3-detection-channels).
**Reality check:** on the live single-host
R1 config ([`configs/alertmanager/alertmanager.r1.yml`](../../configs/alertmanager/alertmanager.r1.yml))
the page tier fans out to Discord ONLY — `chat-page` has no
`pagerduty_configs`. The multi-host template
([`alertmanager.yml.j2`](../../configs/ansible/roles/prometheus/templates/alertmanager.yml.j2))
supports PagerDuty conditionally via `alertmanager_pagerduty_key`,
but no tracked inventory sets that var, so PagerDuty is not wired
anywhere in this repo today. Until a PagerDuty integration key is
provisioned and wired, "page" severity means "post to Discord
#stellarindex-pages" — it does not itself wake anyone the way a
phone-call/SMS PagerDuty escalation would.

## Layout

```
deploy/monitoring/
├── README.md       (this file)
├── rules/          one file per alert family (api.yml, api-security.yml, infra.yml, ...)
└── rule-tests/     promtool unit tests, one file per rule family
```

The set of rule files changes often; list the directory for the current
files and see [alerts-catalog.md](../../docs/operations/alerts-catalog.md)
for every alert, its trigger and its runbook.

## Severity labels

Every alert carries:

- `severity: page` → SEV-1 (P1) — intended to wake oncall; on R1
  today this is Discord-only (see the reality check above), not a
  phone/SMS page, until PagerDuty is provisioned.
- `severity: ticket` → SEV-2 (P2) — business-hours page, after-hours ticket.
- `severity: informational` → SEV-3 (P3) — ticketed, weekly review.

AlertManager routes by label. The multi-host config template lives
at [`configs/ansible/roles/prometheus/templates/alertmanager.yml.j2`](../../configs/ansible/roles/prometheus/templates/alertmanager.yml.j2)
— rendered to `/etc/alertmanager/alertmanager.yml` on `mon-01..02`
by the `prometheus` ansible role; R1's single-host equivalent is
[`configs/alertmanager/alertmanager.r1.yml`](../../configs/alertmanager/alertmanager.r1.yml).
Routes split by `severity:` to Discord #pages + PagerDuty-if-configured
(page) / Discord #alerts (ticket) / informational digest (Alertmanager
UI only) — see the reality check above for R1's actual current wiring.

## Validating locally

```sh
# Install promtool (bundled with prometheus binary distribution):
brew install prometheus
# or from the GitHub release.

# Validate every rule file parses + has no warnings:
make monitoring-check
# which runs:
promtool check rules deploy/monitoring/rules/*.yml

# Rule-firing unit tests (promtool test rules):
promtool test rules deploy/monitoring/rule-tests/*.yml
```

CI runs `promtool check rules` on every PR via the `monitoring-rules`
job in [`.github/workflows/ci.yml`](../../.github/workflows/ci.yml)
(installs `promtool` from the official Prometheus release, then
runs `make monitoring-check`). Rule-file syntax errors fail the
PR; a doc-only drift check on alert/runbook references runs in
parallel via `scripts/ci/lint-docs.sh`.

`promtool test rules` **is** wired: `make monitoring-check` runs it
over `deploy/monitoring/rule-tests/*.yml`, and CI runs the same
target. New behavioural cases go in that directory — **not** in a
`test/monitoring/` tree, which does not exist.

Note what those tests do and do not cover: they load
`deploy/monitoring/rules/`, so the R1 overlay at
`configs/prometheus/rules.r1/` is covered only transitively, by
`lint-rule-equivalence` proving the two trees equivalent.

Run `make monitoring-check` locally before pushing to skip a
round-trip on rule-syntax errors.

## Adding an alert

Every alert needs a runbook (see [alerts-catalog.md](../../docs/operations/alerts-catalog.md)):

1. Expose the metric in `internal/obs/*.go` (Prometheus registry).
2. Add the rule to the appropriate file under `rules/` **and** its
   twin under `configs/prometheus/rules.r1/` — `lint-rule-equivalence`
   fails the build if the two trees drift.
3. Write the runbook at `docs/operations/runbooks/<name>.md` (copy
   `_template.md`).
4. Add a row to `docs/operations/alerts-catalog.md`.
5. Add a promtool case under `deploy/monitoring/rule-tests/`, asserting
   the alert FIRES on the failure it names and stays quiet otherwise.
   Not optional and not "if/when": four alerts in the wave-D audit were
   structurally unable to fire — an empty vector where a zero was
   assumed, or a label set that never matched — and every one of them
   passed `promtool check rules`, which only checks that an expression
   parses. A firing case is the only thing that distinguishes a working
   alert from a well-formed one.

All five in one PR. The `scripts/ci/lint-docs.sh` script fails the
build if any rule's `runbook_url` points at a missing runbook
file (rule §9, "Every alert rule's runbook_url must point to an
existing file") — so a fully-wired alert with a missing runbook
won't merge.

## Labels convention

Every rule carries these labels for AlertManager routing:

| Label | Values | Purpose |
| ----- | ------ | ------- |
| `severity` | `page` / `ticket` / `informational` | routing tier |
| `team` | `stellarindex` | downstream filtering |
| `component` | `ingestion` / `storage` / `cache` / `api` / `stellar` / `infra` / `meta` / `aggregator` / `archive` / `divergence` / `supply` | dashboard grouping |
| `runbook_url` | `https://github.com/Stellar-Index/StellarIndex/blob/main/docs/operations/runbooks/<name>.md` | direct link from the page |

Annotations (not labels) carry human-readable metadata:

- `summary` — one-line headline for the page.
- `description` — 2–3 line explanation, populated with label substitutions.
