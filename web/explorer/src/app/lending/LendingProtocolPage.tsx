import type { Metadata } from 'next';
import type { ReactNode } from 'react';
import Link from 'next/link';
import { ExternalRef } from '@/components/ExternalRef';
import { NoteBadge } from '@/components/NoteBadge';
import { Panel } from '@/components/reveal';
import { Badge, Container, PageHeader } from '@/components/ui';
import { protocolMeta } from '@/app/protocols/registry';

import { LendingPoolsTable } from './LendingPoolsTable';
import { LendingProtocolRevenue } from './LendingProtocolRevenue';
import { LendingProtocolStats } from './LendingProtocolStats';

export type LendingProtocolId = 'blend' | 'blnt';

interface LendingProtocolPageSpec {
  id: LendingProtocolId;
  name: string;
  href: string;
  hint: string;
  title: string;
  description: string;
  about: ReactNode;
  /** Off-site references, rendered only in the page footer. */
  refs: readonly string[];
}

const blendAbout = (
  <>
    <div className="flex flex-wrap items-center gap-2">
      <Badge tone="ok" dot>
        Live
      </Badge>
      <Badge>Isolated pools</Badge>
      <Badge title="Each pool reads the SEP-40 Reflector oracle for collateral valuation.">
        Reflector-priced
      </Badge>
      <Badge title="Liquidations execute against a Comet-style auction backstop.">
        Comet backstop
      </Badge>
      <Link
        href="/protocols/blend"
        className="text-brand-600 ml-auto text-xs hover:underline"
      >
        Protocol page
      </Link>
    </div>
    <details className="text-ink-muted text-xs">
      <summary className="cursor-pointer">About Blend</summary>
      <ul className="mt-2 space-y-1">
        <li>
          Each pool is isolated (Aave-V3 style); collateral and borrow assets
          are chosen per pool by the operator.
        </li>
        <li>
          Reflector divergence from our VWAP shifts the liquidation threshold;
          it surfaces on coin pages via{' '}
          <code className="font-mono">flags.divergence_warning</code> (see{' '}
          <Link href="/oracles" className="underline decoration-dotted">
            /oracles
          </Link>
          ).
        </li>
        <li>
          The backstop is a Comet pool, the same code path indexed on{' '}
          <Link href="/dexes" className="underline decoration-dotted">
            /dexes
          </Link>
          .
        </li>
        <li>
          Liquidations can sandwich oracle updates; see{' '}
          <Link href="/mev" className="underline decoration-dotted">
            /mev
          </Link>
          .
        </li>
      </ul>
    </details>
  </>
);

const blntAbout = (
  <div className="flex flex-wrap items-center gap-2">
    <Badge>Isolated pools</Badge>
    <Badge title="Supports real-world-asset reserves.">RWA reserves</Badge>
    <Badge title="Backstop deposits sit in first-, second- and third-loss tiers.">
      Tiered backstop
    </Badge>
    <Link
      href="/protocols/blnt"
      className="text-brand-600 ml-auto text-xs hover:underline"
    >
      Protocol page
    </Link>
  </div>
);

/** The two lending protocols with a page of their own, in landing order. */
export const LENDING_PROTOCOL_PAGES: readonly LendingProtocolPageSpec[] = [
  {
    id: 'blend',
    name: 'Blend',
    href: '/lending/blend',
    hint: 'Isolated pools, priced by Reflector, liquidated through a Comet backstop',
    title: 'Blend lending — pools, revenue and BLND on Stellar',
    description:
      'Blend lending on Stellar: total supplied and borrowed, interest and revenue by window, the BLND token, and every Blend pool.',
    about: blendAbout,
    refs: ['https://github.com/blend-capital/blend-contracts'],
  },
  {
    id: 'blnt',
    name: 'BLNT V3',
    href: '/lending/blnt',
    hint: 'Blend-derived pools with RWA support and tiered backstops',
    title: 'BLNT V3 lending — pools, revenue and BLNT on Stellar',
    description:
      'BLNT V3 lending on Stellar: total supplied and borrowed, interest and revenue by window, the BLNT token, and every BLNT pool.',
    about: blntAbout,
    refs: [protocolMeta('blnt')?.links?.website].filter(
      (h): h is string => !!h,
    ),
  },
];

function spec(id: LendingProtocolId): LendingProtocolPageSpec {
  return LENDING_PROTOCOL_PAGES.find((p) => p.id === id)!;
}

/** The protocol's lending page, or null for a protocol without one. */
export function lendingProtocolHref(name: string): string | null {
  return LENDING_PROTOCOL_PAGES.find((p) => p.id === name)?.href ?? null;
}

export function lendingProtocolMetadata(id: LendingProtocolId): Metadata {
  const p = spec(id);
  return {
    alternates: { canonical: p.href },
    title: p.title,
    description: p.description,
  };
}

export function LendingProtocolPage({
  protocol,
}: {
  protocol: LendingProtocolId;
}) {
  const p = spec(protocol);
  return (
    <Container className="space-y-6 py-8">
      <PageHeader
        breadcrumbs={[
          { label: 'Home', href: '/' },
          { label: 'Lending', href: '/lending' },
          { label: p.name },
        ]}
        title={`${p.name} lending`}
        meta={p.hint}
      />
      <LendingProtocolStats protocol={protocol} />
      <LendingProtocolRevenue protocol={protocol} />
      <Panel
        headingLevel={2}
        title={`About ${p.name}`}
        bodyClassName="space-y-3"
      >
        {p.about}
      </Panel>
      <LendingPoolsTable protocol={protocol} />
      <NoteBadge label="indexed pools only">
        Pools are listed once they have an indexed auction or a position; a pool
        with neither is not listed yet.
      </NoteBadge>
      {p.refs.length > 0 && (
        <div
          data-testid="references"
          className="border-line flex flex-wrap gap-x-4 gap-y-1 border-t pt-4 text-xs"
        >
          {p.refs.map((href) => (
            <ExternalRef key={href} href={href} />
          ))}
        </div>
      )}
    </Container>
  );
}
