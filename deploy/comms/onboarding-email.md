<!--
First-time customer onboarding response. Reply when a
prospective customer asks "how do I start?" or signs up via
the self-service flow.

Tone: warm but information-dense. Goal is "first successful
request within 5 minutes of receiving this".

Subject: "Re: Stellar Index API access — getting started"
-->

# Welcome to Stellar Index, {{customer_name}}

Your API key is not in this email: we never send keys. Sign in to
the dashboard and mint one (`POST /v1/dashboard/keys`); it is shown
once, at mint time, and we keep only its hash.

(Treat it as a credential — store it in a secret manager, not in
code. To replace it, mint an independent key from the dashboard
(`POST /v1/dashboard/keys`) or ask us for one, deploy the new key,
then revoke this one with `DELETE /v1/account/keys/{keyID}`. Do not
use `POST /v1/account/keys` for this: it mints a child of the calling
key, and revoking this key revokes its children too.)

## First request

```sh
curl -H "Authorization: Bearer $STELLARINDEX_API_KEY" \
  'https://api.stellarindex.io/v1/price?base=native&quote=fiat:USD' | jq .
```

Expected response (illustrative):

```json
{
  "data": {
    "asset": "native",
    "quote": "fiat:USD",
    "price": "0.176540123456",
    "observed_at": "2026-05-03T14:23:00Z",
    "confidence": 0.94,
    "confidence_factors": {
      "z_score": 0.98,
      "source_count": 0.95,
      "diversity": 0.92,
      "liquidity": 0.99,
      "cross_oracle": 0.91,
      "baseline_quality": 1.00
    }
  },
  "as_of": "2026-05-03T14:23:00Z",
  "sources": ["binance", "coinbase", "kraken", "soroswap"],
  "flags": {
    "stale": false,
    "reduced_redundancy": false,
    "triangulated": false,
    "divergence_warning": false
  }
}
```

## Surfaces you'll likely use

| URL | When |
|---|---|
| `/v1/price` | Closed-bucket VWAP (cross-region byte-identical) — for trade execution + reporting |
| `/v1/price/tip` | Rolling-window VWAP — for UI displays where freshness > consistency |
| `/v1/history/since-inception` | Historical series, granularity-aware |
| `/v1/assets/{id}` | Asset detail (supply, market cap, FDV, 24h volume) |
| `/v1/price/stream` (SSE) | Push notifications on every closed bucket |

Full API reference: <https://docs.stellarindex.io>.
Walkthrough: <https://docs.stellarindex.io/getting-started>.

## Rate limits

Your tier ({{tier}}): **{{rate_limit}} rpm**. The
`X-RateLimit-Remaining` response header tells you how much
budget is left in the current window. 429 responses include
`Retry-After`.

## Help

- Bug reports / feature requests: <https://github.com/Stellar-Index/StellarIndex/issues>
- Status: <https://stellarindex.io/status>
- Direct support: reply to this email.

Welcome aboard.

— {{your_name}}
