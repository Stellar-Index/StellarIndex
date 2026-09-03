# Governance

How decisions get made in Stellar Index, and who makes them. This document
describes the process; **[CODEOWNERS](CODEOWNERS)** names the people currently
holding each role.

The project is pre-v1 and small. This is written to be honest about that
rather than to imply a larger structure than exists — and to say what happens
as it grows.

## What the project optimises for

Stellar Index publishes financial data. Someone will make a decision using a
number it served. Every process rule below follows from that:

- **Correct beats fast, and honest beats either.** A number the system cannot
  stand behind is withheld or labelled, never guessed. "Faster by being less
  true" is not a trade this project makes.
- **A claim needs evidence.** Performance, coverage and correctness claims
  carry measurements. This applies to a commit message as much as to the API.
- **Silence is a defect.** A check that cannot fail, an alert that reaches
  nobody, a link that 404s mid-incident — each is treated as a bug in its own
  right, not as cosmetic.

## Roles

**Contributors** open issues and pull requests. No prior involvement is
needed. Everything required to make a change is in the repository:
[CONTRIBUTING.md](CONTRIBUTING.md) for the workflow,
[AGENTS.md](AGENTS.md) for the rules, and
[docs/contributing/procedures/](docs/contributing/procedures/) for the gated
checklists.

**Maintainers** review and merge, and are listed in
[CODEOWNERS](CODEOWNERS) per area. A maintainer is responsible for the
correctness of what they approve, not merely for its style.

**The project lead** ratifies architectural decisions, sets release timing,
and owns anything that binds the project externally — the published SLA,
security disclosure, and licensing.

## How decisions are made

**Day-to-day changes** are decided in the pull request by the area's
CODEOWNER. Two approvals are not required; one careful one is.

**Architectural decisions** are made through an
[ADR](docs/adr/). Open one with status `Proposed`, discuss it in the PR, and
merge it as `Accepted` or `Rejected`. ADRs are immutable once accepted: a
later decision supersedes an earlier one by reference rather than by editing
it, so the reasoning at the time survives.

An ADR is required when a change alters a public wire shape, a data-integrity
invariant, the ingest topology, or an operational commitment. If you are
unsure whether one is needed, it probably is.

**Disagreement** is resolved by evidence first — a measurement, a
reproduction, a counter-example. Where evidence cannot settle it, the area's
CODEOWNER decides; where the decision binds the project externally, the
project lead does. A decision that overrides a raised objection is recorded
with the objection, not without it.

## Changing this document

By pull request, like anything else.
