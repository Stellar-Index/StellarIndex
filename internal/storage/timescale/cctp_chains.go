package timescale

import "fmt"

// ─── CCTP chain-identity tables (SDF-facing: verified, never guessed) ────
//
// Both tables were verified 2026-07-30 against Circle's PRIMARY docs:
//
//   - USDC token contracts per chain:
//     https://developers.circle.com/stablecoins/usdc-contract-addresses
//   - CCTP domain registry:
//     https://developers.circle.com/cctp/concepts/supported-chains-and-domains
//
// INBOUND source-chain attribution: a Stellar mint's transfer carries the
// burn-side USDC token in its CCTP BurnMessage body (version u32 ‖ burnToken
// bytes32 ‖ mintRecipient bytes32 ‖ …). The projected message_received row
// stores the body as raw hex, so hex chars 33..72 are the LOW 20 BYTES of
// burnToken. For EVM chains that slice IS the token address (bytes32 is the
// left-padded address); for 32-byte-address chains (Solana / Aptos /
// Starknet) it is the verified tail of the full address:
//
//	Solana   USDC mint EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v
//	         = 0xc6fa7af3…452f5d61 (base58-decoded), tail abc9…5d61
//	Aptos    USDC 0xbae207659db88bea0cbead6da0ed00aac12edcdda169e591cd41c94180b46f3b
//	Starknet USDC 0x033068f6539f8e6e6b131e6b2b814e6c34a5224bc66947c47dab9dfee93b35fb
//
// A tail that is not in the table renders as "Unverified (0x…)" — an honest
// label is always preferred over a guessed chain name.
//
// cctpBurnTokenChains maps that lowercase 40-hex-char burn-token tail to the
// chain name. Only chains that are CCTP domains are listed (a burn observed
// on Stellar can only originate from a CCTP domain).
var cctpBurnTokenChains = map[string]string{
	// EVM chains — the slice is the USDC contract address itself.
	"a0b86991c6218b36c1d19d4a2e9eb0ce3606eb48": "Ethereum",
	"b97ef9ef8734c71904d8002f8b6bc66dd9c48a6e": "Avalanche",
	"0b2c639c533813f4aa9d7837caf62653d097ff85": "OP Mainnet",
	"af88d065e77c8cc2239327c5edb3a432268e5831": "Arbitrum",
	"833589fcd6edb6e08f4c7c32d4f71b54bda02913": "Base",
	"3c499c542cef5e3811e1192ce70d8cc03d5c3359": "Polygon PoS",
	"078d782b760474a361dda0af3839290b0ef57ad6": "Unichain",
	"176211869ca2b568f2a7d4ee941e073a821ee1ff": "Linea",
	"d996633a415985dbd7d6d12f4a4343e31f5037cf": "Codex",
	"29219dd400f2bf60e5a23d13be72b486d4038894": "Sonic",
	"79a02482a880bce3f13e09da970dc34db4cd24d1": "World Chain",
	"754704bc059f8c67012fed69bc8a327a5aafb603": "Monad",
	"e15fc38f6d8c56af07bbcbe3baf5708a2bf42392": "Sei",
	"fa2958cb79b0491cc627c1557f441ef849ca8eb1": "XDC",
	"b88339cb7199b77e23db6e890353e22632ba630f": "HyperEVM",
	"2d270e6886d130d724215a266106e6832161eaed": "Ink",
	"222365ef19f7947e5484218551b56bb3965aa7af": "Plume",
	"98d2919b9a214e6fa5384ac81e6864ba686ad74c": "EDGE",
	"a00c59ff5a080d2b954d0c75e46e22a0c371235a": "Injective",
	"cfb1186f4e93d60e60a8bdd997427d1f33bc372b": "Morph",
	"c879c018db60520f4355c26ed1a6d572cdac1815": "Pharos",
	"3d7f2c478aafdb65542bcb44bceec05849999d2d": "Cronos",
	// 32-byte-address chains — the slice is the verified tail of the full
	// address (see the doc comment above for the byte-exact derivations).
	"abc97431b1bbe4c2d2f6e0e47ca60203452f5d61": "Solana",
	"a0ed00aac12edcdda169e591cd41c94180b46f3b": "Aptos",
	"2b814e6c34a5224bc66947c47dab9dfee93b35fb": "Starknet",
}

// cctpChainForBurnToken resolves a burn-token tail (lowercase 40-hex slice
// of the BurnMessage body) to a verified chain name. Unknown tails get an
// honest "Unverified (0x…)" label — SDF-facing data never guesses.
func cctpChainForBurnToken(tail string) string {
	if tail == "" {
		return "Unknown source"
	}
	if name, ok := cctpBurnTokenChains[tail]; ok {
		return name
	}
	if len(tail) >= 8 {
		return fmt.Sprintf("Unverified (0x%s…%s)", tail[:4], tail[len(tail)-4:])
	}
	return fmt.Sprintf("Unverified (0x%s)", tail)
}

// cctpDomainChains is Circle's public CCTP domain registry (verified against
// https://developers.circle.com/cctp/concepts/supported-chains-and-domains,
// 2026-07-30; Noble/Sui/Aptos are CCTP-v1-legacy domains, kept because
// deposit_for_burn rows reference them). Domains 20/23/24 are unassigned.
var cctpDomainChains = map[uint32]string{
	0:  "Ethereum",
	1:  "Avalanche",
	2:  "OP Mainnet",
	3:  "Arbitrum",
	4:  "Noble",
	5:  "Solana",
	6:  "Base",
	7:  "Polygon PoS",
	8:  "Sui",
	9:  "Aptos",
	10: "Unichain",
	11: "Linea",
	12: "Codex",
	13: "Sonic",
	14: "World Chain",
	15: "Monad",
	16: "Sei",
	17: "BNB Smart Chain",
	18: "XDC",
	19: "HyperEVM",
	21: "Ink",
	22: "Plume",
	25: "Starknet",
	26: "Arc",
	27: "Stellar",
	28: "EDGE",
	29: "Injective",
	30: "Morph",
	31: "Pharos",
	32: "Cronos",
}

// cctpChainForDomain resolves a CCTP destination domain id to its chain
// name; an id outside the verified registry renders as "Domain N".
func cctpChainForDomain(domain uint32) string {
	if name, ok := cctpDomainChains[domain]; ok {
		return name
	}
	return fmt.Sprintf("Domain %d", domain)
}
