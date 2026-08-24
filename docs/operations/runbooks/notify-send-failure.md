---
title: Runbook — notify-send-failure
last_verified: 2026-08-25
status: ratified
severity: P2
---

# Runbook — `stellarindex_notify_send_failure_ratio_high`

## At a glance

| Field | Value |
| ----- | ----- |
| Alert | `stellarindex_notify_send_failure_ratio_high` |
| Severity | P2 (ticket — user-facing auth flows stop delivering, but existing sessions/keys are unaffected) |
| Detected by | Prometheus rule in `deploy/monitoring/rules/notify.yml` (counter from `internal/notify` call sites) |
| Typical MTTR | 15 min (credential/domain fix) – provider-dependent (Resend outage) |
| Impact | The named `template` stops delivering: `magic-link` → no new dashboard sign-ins; `signup-verify` → API-signup confirmations don't arrive (the key still works, but `email_verified` never flips). Existing sessions and API keys keep working. |

## Symptoms

- `stellarindex_notify_sends_total{template="…",result="failed"}` climbing while
  `result="sent"` is flat.
- The failure ratio for a template exceeds 50% for 15+ minutes.
- Users report "I never got the sign-in email" / "my confirmation link never
  arrived".

## What this metric watches

`internal/notify` is the Resend client behind two mail paths and only two:

- `magic-link` — the dashboard sign-in email (`internal/api/v1/dashboardauth`).
  The login handler deliberately returns `200` whether or not the send
  succeeds (so an attacker can't use the response to confirm an email exists),
  so **the counter is the only signal the mail failed**.
- `signup-verify` — the API-signup confirmation email
  (`cmd/stellarindex-api` `signupVerifyEmailerAdapter`).

Price alerts deliver via **webhooks**, not mail — they are unaffected by a mail
outage and are watched separately.

## Quick diagnosis (≤ 5 min)

```sh
# Which template is failing, and what's the ratio?
#   promql: sum by (template) (rate(stellarindex_notify_sends_total{result="failed"}[15m]))
#           / sum by (template) (rate(stellarindex_notify_sends_total[15m]))

# The send error is logged at the call site. Look for the mapped error class
# (ErrProviderRejected = 4xx, ErrTransient = 5xx/network, ErrInvalidMessage =
# our own validation).
ssh <api-host> 'journalctl -u stellarindex-api --since "30 min ago" --no-pager \
  | grep -iE "send magic link email|signup.?verif" | tail -30'
```

| Log / error class | Likely cause |
| ----------------- | ------------ |
| `notify: transient provider failure` (5xx / network) | Resend outage or network egress problem — check https://resend-status.com |
| `notify: provider rejected` (4xx) | API key rotated/invalid, sending domain unverified, or a bad From address |
| `notify: invalid message` | A template/rendering regression produced an empty subject/body — a code bug, not a provider issue |

## Mitigation

- [ ] **Provider outage (transient/5xx)**: confirm on Resend's status page. If
  it's them, there is no local fix — the counter recovers when they do. Note it
  in the incident channel so support can tell affected users to retry.
- [ ] **Credential / domain (4xx)**: verify `STELLARINDEX_RESEND_API_KEY` is set
  and current, and that the sending domain is still verified in the Resend
  dashboard. Rotating the key is a **separate operational action** (do not
  commit a key); redeploy the API with the corrected secret.
- [ ] **`invalid message` (our bug)**: this is a rendering/validation
  regression, not a provider problem — check recent changes to
  `internal/notify/templates.go` or the signup email body; roll back if needed.
- [ ] **Verification**: `result="sent"` resumes climbing and the ratio falls
  back below the threshold. Send yourself a magic link to confirm end-to-end.

## Known false-positive patterns

- **Very low mail volume**: the ratio is computed over a 15m window; a single
  failure in an otherwise-empty window can briefly spike the ratio. The
  `for: 15m` dwell absorbs one-off blips — a sustained firing is real.

## Related

- `internal/notify` — the Resend client and its `ErrProviderRejected` /
  `ErrTransient` / `ErrInvalidMessage` error classes.
- The dashboard-auth login flow (`internal/api/v1/dashboardauth`) and the
  API-signup verify flow (`cmd/stellarindex-api`) — the two send call sites.

## Changelog

- 2026-08-25 — initial draft alongside the task #33 / W8 recon 9c notify counter.
