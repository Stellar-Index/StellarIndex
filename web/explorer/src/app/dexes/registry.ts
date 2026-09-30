// Curated DEX sources with friendly names + audit links. The closed set of
// /dexes/<source> pages: generateStaticParams and sitemap.ts both read it,
// so the sitemap lists exactly the built pages.
//
// scripts/ci/lint-protocol-registry-sync.sh §2 cross-checks this map (and
// ALL_DEXES in DexesView) against the Go source registry, so a Go-registered
// DEX cannot ship without a page.
export const DEX_INFO: Record<
  string,
  {
    name: string;
    type: string;
    status: string;
    contractsUrl?: string;
    blurb: string;
  }
> = {
  soroswap: {
    name: 'Soroswap',
    type: 'Uniswap V2 clone (Soroban)',
    status: 'live',
    contractsUrl: 'https://github.com/soroswap/core',
    blurb:
      'Constant-product AMM. Each pool below is a SoroswapPair contract. Click a pool to drill into its trade history and live VWAP.',
  },
  phoenix: {
    name: 'Phoenix',
    type: 'AMM (Soroban)',
    status: 'live',
    blurb:
      'Soroban AMM with per-field event split. Each pool below is one Phoenix pair contract.',
  },
  aquarius: {
    name: 'Aquarius',
    type: 'AMM with gauges (Soroban)',
    status: 'live',
    blurb:
      'Curve-style AMM with bribe/gauge layer. Constant-product and stableswap pools render uniformly.',
  },
  sdex: {
    name: 'SDEX',
    type: 'Native order book (classic)',
    status: 'native',
    blurb:
      'Stellar-native on-chain order book. Each row below is a (base, quote) classic-asset pair that traded on SDEX in the recency window.',
  },
  comet: {
    name: 'Comet',
    type: 'Balancer V1 fork (Soroban)',
    status: 'experimental',
    blurb:
      'Balancer-style multi-asset pool. Shared ("POOL", <event>) topic across every Comet pool contract.',
  },
  sushiswap_v3: {
    name: 'SushiSwap V3',
    type: 'Concentrated liquidity (Soroban)',
    status: 'live',
    blurb:
      'Concentrated-liquidity AMM, factory-gated on a single pool factory. Each pool below is one V3 pool contract. Depth sits in per-position tick ranges rather than one two-sided reserve, so this venue carries no reserve or TVL figure — see the note under the table.',
  },
};
