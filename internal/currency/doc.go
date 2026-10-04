// Package currency loads + indexes the verified-currency catalogue:
// the hand-curated list of currencies (USDC, EURC, XLM, AQUA, …) that
// the API surfaces with a "verified" badge and whose Stellar identities
// anchor the unverified-ticker-collision warning on `/v1/assets/{id}`.
// Non-fiat entries with no Stellar issuance (USDT, BTC, ETH, …) are marked
// reference_only: they are excluded from Browseable() but still feed the
// reference-price cross-check (CoinGeckoIDs/CoinMarketCapIDs) and, keyed
// on ticker, flag every classic asset bearing that ticker as a collision.
// Fiat entries (USD, EUR, …) carry no issuance either; they are
// denominations (FiatDenomination), never collisions.
//
// The seed catalogue ships embedded in the binary (data/seed.yaml).
// Changing the catalogue is a code change + redeploy; it is never
// populated from an external aggregator.
//
// Typical wiring:
//
//	cat, err := currency.LoadEmbedded()
//	if err != nil { ... }
//	opts.VerifiedCurrencies = cat  // wired into v1.Options
//
// Handlers consult the loaded *Catalogue via the lookup methods:
//
//	LookupBySlug("usdc")            // /v1/assets/usdc routing
//	LookupByStellarAssetID(...)     // exact-match identification
//	StellarCollision(code, issuer)  // unverified-collision detection
package currency
