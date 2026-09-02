---
title: Agent onboarding — one curl to an API key
last_verified: 2026-08-11
status: living doc
---

# Agent onboarding

Stellar Index is free. If you are an AI agent (or a script, or a
human in a hurry) and you want an API key, this is the whole flow —
one unauthenticated POST, no email, no browser, no payment:

```sh
curl -X POST https://api.stellarindex.io/v1/register \
  -H 'Content-Type: application/json'
```

Response (the key is shown **once** — store it; if you lose it,
just register again):

```json
{
  "data": {
    "account_id": "7d9f2a54-4f0e-4c1a-9b3d-2f6c8e1a0b5c",
    "api_key": "sip_YOUR_KEY_HERE…",
    "key_id": "kid_YOUR_KEY_ID",
    "key_prefix": "sip_YOUR_KEY_HERE",
    "tier": "free",
    "limits": {
      "rate_limit_per_min": 1000,
      "monthly_quota": 1000000,
      "max_active_keys": 25,
      "max_webhooks": 10,
      "max_price_alerts": 25
    }
  },
  "as_of": "2026-08-11T14:35:42.881Z",
  "flags": { "stale": false, "reduced_redundancy": false, "triangulated": false, "divergence_warning": false }
}
```

Then use the key on every request:

```sh
curl -fsSL -H "Authorization: Bearer sip_YOUR_KEY_HERE…" \
  "https://api.stellarindex.io/v1/price?asset=native&quote=fiat:USD"
```

Optionally give the account a name and a contact email (the email is
contact-only — it is never verified and nothing is keyed on it):

```sh
curl -X POST https://api.stellarindex.io/v1/register \
  -H 'Content-Type: application/json' \
  -d '{"name": "my-trading-agent", "email": "ops@example.com"}'
```

## The tier model

| Tier | Who | Rate limit | Monthly quota |
|---|---|---|---|
| `anon` | no key at all | 6,000 req/min per IP on the hosted API (60/min is the self-hosted default) | — |
| `free` | anyone who registers (this page) | 1,000 req/min per key, stamped at mint | 1M req/month |
| `partner` | staff-set per-account limits — [contact us](mailto:hello@stellarindex.io) | up to 100,000 req/min | negotiated |

Everything is free; `partner` is not a paid plan, it's an operator
override for teams that need more headroom.

Yes, the hosted anonymous per-IP limit is currently *higher* than a
free key's per-key limit. That is deliberate: a key buys per-subject
attribution, usage reporting and the private `/v1/account/*` surfaces,
not headroom. Read `X-RateLimit-Limit` on any response rather than
hard-coding either number.

## Rules of the road

- **Registration is per-IP throttled** (shared with `/v1/signup`,
  ~5/hour). Register once and keep the key — don't mint a fresh
  account per session; you'll hit 429 and gain nothing, since one
  free key already carries the full free-tier budget.
- The plaintext key is returned exactly once and stored only as a
  hash. Losing it is not an incident: register again, or mint
  additional keys under the same account via `POST /v1/account/keys`
  (authenticated).
- Anonymous access works for everything public — you only need a key
  for the higher rate limit.

Full API reference: [docs.stellarindex.io](https://docs.stellarindex.io) ·
spec: [`openapi/stellar-index.v1.yaml`](../openapi/stellar-index.v1.yaml) ·
first steps: [getting-started.md](getting-started.md)
