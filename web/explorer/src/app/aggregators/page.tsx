import type { Metadata } from 'next';
import Link from 'next/link';
import { hrefFor } from '@/lib/hrefFor';
import { ExternalLink } from 'lucide-react';

import { ReferencePriceAggregators } from './ReferencePriceAggregators';
import { RoutedVolumePanel } from './RoutedVolumePanel';

import { Badge, Breadcrumbs, Container } from '@/components/ui';
export const metadata: Metadata = {
  title: 'Aggregators — routers and yield wrappers on Stellar',
  description:
    'Soroswap router, DeFindex yield vaults — protocols that route into the underlying DEXes and lending pools. Excluded from VWAP to avoid double-counting upstream markets.',
  alternates: { canonical: '/aggregators' },
};

type ContractRef = {
  label: string;
  cstrkey: string;
};

type Entry = {
  name: string;
  type: 'router' | 'yield';
  blurb: string;
  notes: string[];
  contractsRepo?: string;
  contractRefs?: ContractRef[];
  homepage?: string;
  /** Internal protocol-page slug (/protocols/<slug>) when one exists. */
  protocolSlug?: string;
};

const ENTRIES: Entry[] = [
  {
    name: 'Soroswap Router',
    type: 'router',
    blurb:
      'Multi-hop router over Soroswap pairs; routed trades resolve to the pair-level swap events we already index.',
    notes: [
      'No router-specific decoder needed for trades — the underlying SoroswapPair swap events fire regardless. The router contract is tracked separately for routed-via attribution (what % of pair volume arrived via the router).',
      'Per the protocol-class contract, routers are in `/v1/sources` with class=aggregator and contribute zero VWAP weight by default. Including them would double-count the underlying pair trade.',
    ],
    contractsRepo: 'https://github.com/soroswap/core',
    protocolSlug: 'soroswap',
    contractRefs: [
      // Sourced from soroswap/core public/mainnet.contracts.json.
      {
        label: 'Router',
        cstrkey: 'CAG5LRYQ5JVEUI5TEID72EYOVX44TTUJT5BQR2J6J77FH65PCCFAJDDH',
      },
      {
        label: 'Pair factory',
        cstrkey: 'CA4HEQTL2WPEUYKYKCDOHCDNIV4QHNJ7EL4J4NQ6VADP7SYHVRYZ7AW2',
      },
    ],
    homepage: 'https://soroswap.finance',
  },
  {
    name: 'DeFindex',
    type: 'yield',
    blurb:
      'Yield vaults over Blend and other lending pools: borrow APY plus emissions, minus a management fee.',
    notes: [
      'Position changes happen on the vault contract (deposit/withdraw); the actual yield-bearing legs are at Blend. We track the vault TVL but rely on Blend for the underlying yield observation.',
      'Excluded from VWAP — yield-aggregator inflows are not price-discovery events.',
    ],
    contractsRepo: 'https://github.com/paltalabs/defindex',
    protocolSlug: 'defindex',
    contractRefs: [
      // Sourced from paltalabs/defindex public/mainnet.contracts.json.
      {
        label: 'Factory',
        cstrkey: 'CDKFHFJIET3A73A2YN4KV7NSV32S6YGQMUFH3DNJXLBWL4SKEGVRNFKI',
      },
      {
        label: 'USDC autocompound',
        cstrkey: 'CDB2WMKQQNVZMEBY7Q7GZ5C7E7IAFSNMZ7GGVD6WKTCEWK7XOIAVZSAP',
      },
      {
        label: 'EURC autocompound',
        cstrkey: 'CC5CE6MWISDXT3MLNQ7R3FVILFVFEIH3COWGH45GJKL6BD2ZHF7F7JVI',
      },
      {
        label: 'XLM autocompound',
        cstrkey: 'CDPWNUW7UMCSVO36VAJSQHQECISPJLCVPDASKHRC5SEROAAZDUQ5DG2Z',
      },
    ],
    homepage: 'https://defindex.io',
  },
];

export default function AggregatorsPage() {
  return (
    <Container className="space-y-6 py-8">
      <header className="space-y-2">
        <Breadcrumbs
          items={[{ label: 'Home', href: '/' }, { label: 'Aggregators' }]}
        />
        <h1 className="text-3xl font-semibold tracking-tight">Aggregators</h1>
        <p className="text-ink-body flex max-w-3xl flex-wrap items-center gap-2 text-sm">
          Routers and yield wrappers over{' '}
          <Link href="/dexes" className="underline decoration-dotted">
            DEXes
          </Link>{' '}
          and{' '}
          <Link href="/lending" className="underline decoration-dotted">
            lending pools
          </Link>
          .
          <Badge title="Excluded from VWAP to avoid double-counting. A routed swap still emits the underlying pair's swap event, which is the one we VWAP; a vault deposit moves shares but sets no price, and Blend supplies the collateral revaluation.">
            VWAP weight 0
          </Badge>
        </p>
      </header>

      <div className="grid grid-cols-1 gap-4 lg:grid-cols-2">
        {ENTRIES.map((e) => (
          <Card key={e.name} entry={e} />
        ))}
      </div>

      <RoutedVolumePanel />

      <ReferencePriceAggregators />
    </Container>
  );
}

function Card({ entry }: { entry: Entry }) {
  return (
    <div className="rounded-card border-line bg-surface shadow-card border p-5">
      <div className="flex items-baseline justify-between gap-2">
        <h2 className="text-lg font-semibold tracking-tight">{entry.name}</h2>
        <span
          className={`shrink-0 rounded px-1.5 py-0.5 text-[10px] tracking-wider uppercase ${
            entry.type === 'router'
              ? 'bg-brand-100 text-brand-700'
              : 'bg-surface-subtle text-ink-body'
          }`}
        >
          {entry.type === 'router' ? 'Router' : 'Yield vault'}
        </span>
      </div>
      <p className="text-ink-body mt-3 text-sm">{entry.blurb}</p>
      <details className="text-ink-muted mt-3 text-xs">
        <summary className="cursor-pointer">
          Notes ({entry.notes.length})
        </summary>
        <ul className="mt-1 space-y-1">
          {entry.notes.map((n, i) => (
            <li key={i}>{n}</li>
          ))}
        </ul>
      </details>
      {entry.contractRefs && entry.contractRefs.length > 0 && (
        <div className="border-line mt-4 border-t pt-3">
          <div className="text-ink-muted text-[10px] font-medium tracking-wider uppercase">
            Mainnet contracts
          </div>
          <ul className="mt-1.5 space-y-1 text-xs">
            {entry.contractRefs.map((c) => (
              <li
                key={c.cstrkey}
                className="flex items-baseline justify-between gap-3"
              >
                <span className="text-ink-body">{c.label}</span>
                <Link
                  href={`/contracts/${encodeURIComponent(c.cstrkey)}/`}
                  className="text-brand-600 font-mono text-[11px] hover:underline"
                  title={`${c.cstrkey} — contract events + code`}
                >
                  {c.cstrkey.slice(0, 6)}…{c.cstrkey.slice(-4)}
                </Link>
              </li>
            ))}
          </ul>
        </div>
      )}
      <div className="mt-4 flex flex-wrap gap-3 text-xs">
        {entry.protocolSlug && (
          <Link
            href={hrefFor.protocol(entry.protocolSlug)}
            className="text-brand-600 inline-flex items-center gap-1 font-medium hover:underline"
          >
            Protocol analytics →
          </Link>
        )}
        {entry.homepage && (
          <a
            href={entry.homepage}
            className="text-brand-600 inline-flex items-center gap-1 hover:underline"
            target="_blank"
            rel="noreferrer"
          >
            Homepage
            <ExternalLink className="h-3 w-3" />
          </a>
        )}
        {entry.contractsRepo && (
          <a
            href={entry.contractsRepo}
            className="text-ink-muted inline-flex items-center gap-1 hover:underline"
            target="_blank"
            rel="noreferrer"
          >
            Contracts source
            <ExternalLink className="h-3 w-3" />
          </a>
        )}
      </div>
    </div>
  );
}
