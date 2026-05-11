// Package currency loads + indexes the verified-currency catalogue:
// the hand-curated list of cross-chain currencies (USDC, USDT, BTC,
// ETH, XLM, AQUA, …) that the API surfaces with a "verified" badge
// and whose Stellar identities anchor the unverified-ticker-collision
// warning on `/v1/assets/{id}`.
//
// The seed catalogue ships embedded in the binary (data/seed.yaml)
// — see `docs/architecture/multi-network-assets-migration.md` Phase
// 1.1 for the rationale. Changing the catalogue is a code change +
// redeploy; runtime augmentation (CG / CMC) lands in Phase 1.2.
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
