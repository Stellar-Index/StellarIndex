// Static protocol registry — the bounded name set for generateStaticParams,
// mirrored from internal/api/v1/protocols_registry.go. The Go registry is
// the source of truth for the wire data (categories, descriptions, genesis,
// factories, event kinds, completeness); this file only needs the NAME set
// (so the static export knows which slugs to pre-render) plus a friendly
// display label per protocol. Everything else is fetched at runtime from
// GET /v1/protocols/{name}.
//
// If protocols_registry.go gains or drops a protocol, add/remove the row
// here too — CI doesn't cross-check them, the static export simply won't
// pre-render a slug that isn't listed. The OG card function mirrors the name
// set as PROTOCOL_NAMES in functions/og/[[path]].js; og.test.js pins the two.

export interface ProtocolRegistryEntry {
  /** Canonical source name — the /v1/protocols/{name} path segment. */
  name: string;
  /** Friendly display label for headers + cards. */
  label: string;
  /** Category — mirrors the Go registry (amm/dex/lending/oracle/yield/bridge). */
  category: string;
  /** Short fallback description (the API serves the authoritative one). */
  description: string;
  /** Protocol's own website, docs and contract source; each URL checked live. */
  links?: { website?: string; docs?: string; source?: string };
}

export const PROTOCOLS: ProtocolRegistryEntry[] = [
  {
    name: 'sdex',
    links: {
      docs: 'https://developers.stellar.org/docs/learn/fundamentals/liquidity-on-stellar-sdex-liquidity-pools',
      source: 'https://github.com/stellar/stellar-core',
    },
    category: 'dex',
    label: 'SDEX',
    description: "Stellar's protocol-native central-limit order book.",
  },
  {
    name: 'soroswap',
    links: {
      website: 'https://soroswap.finance',
      docs: 'https://docs.soroswap.finance',
      source: 'https://github.com/soroswap/core',
    },
    category: 'amm',
    label: 'Soroswap',
    description: 'Constant-product Soroban AMM pairs.',
  },
  {
    name: 'aquarius',
    links: {
      website: 'https://aqua.network',
      docs: 'https://docs.aqua.network',
    },
    category: 'amm',
    label: 'Aquarius',
    description: 'Incentivised constant-product and stableswap pools.',
  },
  {
    name: 'phoenix',
    links: {
      website: 'https://www.phoenix-hub.io',
      source: 'https://github.com/Phoenix-Protocol-Group/phoenix-contracts',
    },
    category: 'amm',
    label: 'Phoenix',
    description: 'Soroban constant-product AMM with liquidity + stake events.',
  },
  {
    name: 'sushiswap_v3',
    links: { website: 'https://www.sushi.com', docs: 'https://docs.sushi.com' },
    category: 'amm',
    label: 'SushiSwap V3',
    description:
      'Concentrated-liquidity Soroban pools priced by tick range, not reserves.',
  },
  {
    name: 'comet',
    links: { source: 'https://github.com/CometDEX/comet-contracts-v1' },
    category: 'amm',
    label: 'Comet',
    description: 'Balancer-v1-style weighted pools on Soroban.',
  },
  {
    name: 'blend',
    links: {
      website: 'https://www.blend.capital',
      docs: 'https://docs.blend.capital',
      source: 'https://github.com/blend-capital/blend-contracts-v2',
    },
    category: 'lending',
    label: 'Blend',
    description: 'Isolated lending pools on Soroban.',
  },
  {
    name: 'sorocredit',
    category: 'lending',
    label: 'SoroCredit',
    description:
      'On-chain consumer USDC credit / CDP with scheduled settlements.',
  },
  {
    name: 'upshift',
    links: {
      website: 'https://www.upshift.finance',
      docs: 'https://docs.upshift.finance',
      source:
        'https://github.com/upshift-protocol/stellar-upshift-vault-contracts',
    },
    category: 'yield',
    label: 'Upshift',
    description:
      'Institutional tokenized vaults (earnUSDC, earnXLM) minting shares against one underlying.',
  },
  {
    name: 'defindex',
    links: {
      website: 'https://www.defindex.io',
      docs: 'https://docs.defindex.io',
      source: 'https://github.com/paltalabs/defindex',
    },
    category: 'yield',
    label: 'DeFindex',
    description: 'Yield vaults and strategies across Soroban DeFi.',
  },
  {
    name: 'cctp',
    links: {
      website: 'https://www.circle.com/cross-chain-transfer-protocol',
      docs: 'https://developers.circle.com/cctp',
      source: 'https://github.com/circlefin/stellar-cctp',
    },
    category: 'bridge',
    label: 'Circle CCTP',
    description: 'Canonical burn-and-mint USDC bridging.',
  },
  {
    name: 'rozo',
    links: {
      website: 'https://rozo.ai',
      source: 'https://github.com/RozoAI/rozo-intents-contracts',
    },
    category: 'bridge',
    label: 'Rozo',
    description: 'Intent-bridge payment settlement on Stellar.',
  },
  {
    name: 'soroswap-router',
    links: {
      website: 'https://soroswap.finance',
      docs: 'https://docs.soroswap.finance',
      source: 'https://github.com/soroswap/core',
    },
    category: 'amm',
    label: 'Soroswap Router',
    description: 'Aggregated multi-hop swap intents from router invocations.',
  },
  {
    name: 'band',
    links: {
      website: 'https://www.bandprotocol.com',
      docs: 'https://docs.bandchain.org',
      source:
        'https://github.com/bandprotocol/band-std-reference-contracts-soroban',
    },
    category: 'oracle',
    label: 'Band Protocol',
    description: 'Reference-rate oracle observed from relay() invocations.',
  },
  {
    name: 'reflector-dex',
    links: {
      website: 'https://reflector.network',
      docs: 'https://reflector.network/docs',
      source: 'https://github.com/reflector-network/reflector-contract',
    },
    category: 'oracle',
    label: 'Reflector (DEX)',
    description: 'Reflector oracle — Stellar-DEX price feed.',
  },
  {
    name: 'reflector-cex',
    links: {
      website: 'https://reflector.network',
      docs: 'https://reflector.network/docs',
      source: 'https://github.com/reflector-network/reflector-contract',
    },
    category: 'oracle',
    label: 'Reflector (CEX)',
    description: 'Reflector oracle — centralized-exchange price feed.',
  },
  {
    name: 'reflector-fx',
    links: {
      website: 'https://reflector.network',
      docs: 'https://reflector.network/docs',
      source: 'https://github.com/reflector-network/reflector-contract',
    },
    category: 'oracle',
    label: 'Reflector (FX)',
    description: 'Reflector oracle — fiat exchange-rate feed.',
  },
  {
    name: 'redstone',
    links: {
      website: 'https://www.redstone.finance',
      docs: 'https://docs.redstone.finance',
      source: 'https://github.com/redstone-finance/redstone-oracles-monorepo',
    },
    category: 'oracle',
    label: 'RedStone',
    description: 'Batched multi-feed price pushes to the RedStone adapter.',
  },
];

const BY_NAME = new Map(PROTOCOLS.map((p) => [p.name, p]));

export function protocolMeta(name: string): ProtocolRegistryEntry | undefined {
  return BY_NAME.get(name);
}

// Category → chip tone. The API serves the authoritative category string;
// the tone mapping + adaptive class strings live in the shared pill-tone
// helper (src/lib/pillTone.ts) so every category/venue/type chip stays on the
// same dark-surface-safe palette. Re-exported here to keep the call sites that
// import `categoryTone` from the registry working.
export { categoryToneClass as categoryTone } from '@/lib/pillTone';
