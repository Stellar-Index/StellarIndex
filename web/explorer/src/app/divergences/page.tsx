import type { Metadata } from 'next';
import Link from 'next/link';

import { DivergenceFeed } from './DivergenceFeed';

import { NetworkUnavailable } from '@/components/NetworkUnavailable';
import { routeAvailable } from '@/lib/network-routes';
import { Breadcrumbs, Container } from '@/components/ui';
export const metadata: Metadata = {
  alternates: { canonical: '/divergences' },
  title: 'Divergences — cross-reference monitor',
  description:
    'Continuously cross-checks the canonical Stellar Index VWAP against external references (CoinGecko + on-chain Reflector/Redstone/Band active; Chainlink HTTP configured). Persistent gaps flip flags.divergence_warning.',
};

type FeedRef = { pair: string; address: string };
// status reflects what the divergence WORKER actually cross-checks
// today — not just feeds we ingest elsewhere:
//   active     — producing divergence_observations rows now
//                (Reflector/Redstone/Band went live 2026-07 as
//                on-chain references: the worker compares the
//                latest ingested oracle_updates value against our
//                VWAP for pairs both sides cover)
//   configured — implemented + operator-configured, may be between
//                refreshes / awaiting upstream data
//   planned    — described, but not yet wired as a divergence check
type RefStatus = 'active' | 'configured' | 'planned';
type Reference = {
  name: string;
  type: string;
  blurb: string;
  status: RefStatus;
  feeds?: FeedRef[];
};
const REFERENCES: Reference[] = [
  {
    name: 'CoinGecko',
    type: 'HTTP price index',
    status: 'active',
    blurb:
      "Aggregator-of-aggregators. Useful as a sanity reference because it's not on-chain and pulls from a different upstream set.",
  },
  {
    name: 'Chainlink HTTP',
    type: 'HTTP feed (off-chain Chainlink)',
    status: 'configured',
    blurb:
      'Independent price index via mainnet AggregatorV3 contracts on Ethereum. Queried over public RPC (eth.llamarpc.com). Drives the divergence worker\'s "are we wildly off" alerting threshold.',
    feeds: [
      // Operator-config from configs/ansible/.../stellarindex.toml.j2
      // [divergence.chainlink.feeds].
      {
        pair: 'EUR/USD',
        address: '0xb49f677943BC038e9857d61E7d053CaA2C1734C1',
      },
      {
        pair: 'GBP/USD',
        address: '0x5c0Ab2d9b5a7ed9f470386e82BB36A3613cDd4b5',
      },
      {
        pair: 'JPY/USD',
        address: '0xBcE206caE7f0ec07b545EddE332A47C2F75bbeb3',
      },
    ],
  },
  {
    name: 'Reflector (DEX/CEX/FX)',
    type: 'On-chain SEP-40 oracle',
    status: 'active',
    blurb:
      'Stellar-native oracle trio, compared as three references (reflector-dex/cex/fx) from our own ingested oracle_updates rows. Reflector divergence often signals an oracle update lag rather than a real price move — important to distinguish for downstream consumers like Blend.',
  },
  {
    name: 'Redstone',
    type: 'On-chain adapter contract',
    status: 'active',
    blurb:
      'Pull-style oracle on Stellar, compared from our ingested oracle_updates rows. Divergence here is rare but high-signal — Redstone batches many feeds in one transaction so divergence on one feed often precedes a wider reading update.',
  },
  {
    name: 'Band',
    type: 'On-chain Soroban contract (no events)',
    status: 'active',
    blurb:
      'Operation-args ingest (Band emits zero events). The divergence check reads the same relayed value the on-chain consumer would see, straight from our ingested rows.',
  },
];

function RefStatusBadge({ status }: { status: RefStatus }) {
  const cfg = {
    active: { label: 'Active', cls: 'bg-up-subtle text-up-strong' },
    configured: { label: 'Configured', cls: 'bg-warn-50 text-warn-700' },
    planned: { label: 'Planned', cls: 'bg-surface-subtle text-ink-muted' },
  }[status];
  return (
    <span
      className={`rounded-sm px-1.5 py-0.5 text-[10px] tracking-wider uppercase ${cfg.cls}`}
    >
      {cfg.label}
    </span>
  );
}

export default function DivergencesPage() {
  // Aggregator-derived feed — empty by construction on a network
  // with no aggregator. Nav/search/sitemap no longer offer it there, but
  // a direct URL or an old bookmark still lands here, and an empty table
  // with no explanation reads as an outage.
  // The title stays on this branch: the document carries its <h1> on
  // every network (lib/nav-shell.built.test.ts), and without it the
  // empty state's own heading was the top of the outline.
  if (!routeAvailable('/divergences')) {
    return (
      <Container className="space-y-6 py-8">
        <h1 className="text-3xl font-semibold tracking-tight">Divergences</h1>
        <NetworkUnavailable href="/divergences" />
      </Container>
    );
  }

  return (
    <Container className="space-y-6 py-8">
      <header className="space-y-2">
        <Breadcrumbs
          items={[{ label: 'Home', href: '/' }, { label: 'Divergences' }]}
        />
        <h1 className="text-3xl font-semibold tracking-tight">Divergences</h1>
        <p className="text-ink-body max-w-3xl text-sm">
          Continuously cross-checks the canonical Stellar Index VWAP against
          external references. A persistent gap flips{' '}
          <code className="font-mono text-xs">flags.divergence_warning</code> on
          the canonical{' '}
          <Link href="/assets" className="underline decoration-dotted">
            coin pages
          </Link>{' '}
          and writes a row to the{' '}
          <code className="font-mono text-xs">divergence_observations</code>{' '}
          hypertable for the historical trail.
        </p>
      </header>

      <DivergenceFeed />

      <div className="grid grid-cols-1 gap-4 lg:grid-cols-2">
        {REFERENCES.map((r) => (
          <div
            key={r.name}
            className="border-line bg-surface rounded-xl border p-5 shadow-sm"
          >
            <div className="flex items-center gap-2">
              <h2
                className="text-lg font-semibold tracking-tight"
                title={r.blurb}
              >
                {r.name}
              </h2>
              <RefStatusBadge status={r.status} />
            </div>
            <p className="text-ink-muted mt-1 text-xs tracking-wider uppercase">
              {r.type}
            </p>
            {r.feeds && r.feeds.length > 0 && (
              <div className="border-line mt-4 border-t pt-3">
                <div className="text-ink-muted text-[10px] font-medium tracking-wider uppercase">
                  Wired feeds
                </div>
                <ul className="mt-1.5 space-y-1 text-xs">
                  {r.feeds.map((f) => (
                    <li
                      key={f.address}
                      className="flex items-baseline justify-between gap-3"
                    >
                      <span className="text-ink-body font-mono">{f.pair}</span>
                      <a
                        href={`https://etherscan.io/address/${f.address}`}
                        target="_blank"
                        rel="noreferrer noopener"
                        className="text-brand-600 font-mono text-[11px] hover:underline"
                        title={f.address}
                      >
                        {f.address.slice(0, 8)}…{f.address.slice(-4)}
                      </a>
                    </li>
                  ))}
                </ul>
              </div>
            )}
          </div>
        ))}
      </div>

      <details className="text-ink-muted text-xs">
        <summary className="cursor-pointer">
          Methodology and reading the board
        </summary>
        <div className="mt-2 max-w-3xl space-y-2">
          <p>
            External references never enter the canonical VWAP (they would
            import their methodology and double-count upstream markets). The
            worker instead compares our VWAP to each (pair, reference) every
            refresh tick, persists the row, and feeds the confidence score that
            gates the freeze decision (
            <Link
              href="/research/adr/0019"
              className="underline decoration-dotted"
            >
              ADR-0019
            </Link>
            ).
          </p>
          <p>
            The board shows the latest comparison per (pair, reference) over the
            trailing 7 days, widest gap first. <strong>firing</strong> means the
            last observation breached its threshold, which feeds{' '}
            <code className="font-mono">flags.divergence_warning</code>. Δ% is{' '}
            <code className="font-mono">
              (our − reference) / reference × 100
            </code>
            ; negative means our VWAP is below the reference. Click a row to see
            its history, with the alert threshold as the dashed band.
          </p>
        </div>
      </details>
    </Container>
  );
}
