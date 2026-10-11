// Package auth holds the v1 middleware's caller-identity primitives: anonymous, apikey (`apikey:<sha256-hex>` records via [RedisAPIKeyValidator]; expiry and revocation live in the record) and sep10 (JWT from package sep10).
// [config.APIConfig].AuthMode picks "none", "apikey" or "sep10"; the noop validators are the explicit disabled state. Auth must fit the 10 ms hot-path budget (ADR-0009).
package auth
