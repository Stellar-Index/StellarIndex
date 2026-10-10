'use client';

import Link from 'next/link';

import { PageHeader } from '@/components/ui';
import { useLastPathSegment } from '@/lib/useLastPathSegment';

import { PoolReserves } from './PoolReserves';

/**
 * LendingPoolPathView — the runtime fallback for /lending/[pool] outside
 * the build-time pre-render (same S1b shell pattern already used by
 * /assets and /markets — see AssetPathView / PairPathView).
 *
 * generateStaticParams enumerates every pool /v1/lending/pools currently
 * lists plus the curated factory/backstop contracts, so a pool the Blend
 * factory spawns between builds hard-404'd on the static host until
 * functions/lending/[[path]].js existed to serve this shell. The real
 * pool id is read from the URL (not a build-time param) — PoolReserves is
 * already a client component and fetches its own data live.
 */
export function LendingPoolPathView() {
  const pool = useLastPathSegment();

  return (
    <div className="space-y-6">
      <PageHeader
        breadcrumbs={[
          { label: 'Home', href: '/' },
          { label: 'Lending', href: '/lending' },
          { label: pool ? `${pool.slice(0, 8)}…${pool.slice(-8)}` : 'Pool' },
        ]}
        title={
          <span className="font-mono break-all">{pool || 'Loading…'}</span>
        }
      />
      <Link
        href="/protocols/blend"
        className="text-brand-600 inline-block text-xs hover:underline"
      >
        Blend protocol →
      </Link>
      {pool && <PoolReserves pool={pool} />}
    </div>
  );
}
