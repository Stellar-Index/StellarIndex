// Package auth holds the primitives the v1 middleware uses to identify a
// caller and pick its rate-limit tier. Three tiers, in rising trust:
//
//   - anonymous: no credential, the lowest tier.
//   - apikey: `Authorization: Bearer <key>` or `X-API-Key: <key>`,
//     looked up by [RedisAPIKeyValidator] under `apikey:<sha256-hex>`
//     ([APIKeyRecord]; no TTL, expiry and revocation live in the record).
//   - sep10: a JWT we issued from the SEP-10 challenge/verify exchange
//     (package sep10).
//
// [config.APIConfig].AuthMode picks "none", "apikey" or "sep10". The noop
// validators are the explicit disabled state, including apikey mode with
// no validator wired. Auth must fit the 10 ms hot-path budget (ADR-0009).
package auth
