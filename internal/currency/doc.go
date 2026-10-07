// Package currency is the hand-curated verified-currency catalogue
// (USDC, EURC, XLM, AQUA, …) behind the API's "verified" badge and the
// unverified-ticker collision warning on `/v1/assets/{id}`. It ships
// embedded (data/seed.yaml, [LoadEmbedded]); changing it is a code
// change, never an import from an external aggregator.
//
// Entry kinds:
//   - Stellar-issued: identified exactly by their Stellar identities.
//   - reference_only (USDT, BTC, ETH, …): no Stellar issuance; excluded
//     from Browseable(), still feed the reference-price cross-check, and
//     flag every classic asset with that ticker as a collision.
//   - fiat (USD, EUR, …): denominations (FiatDenomination), never
//     collisions.
package currency
