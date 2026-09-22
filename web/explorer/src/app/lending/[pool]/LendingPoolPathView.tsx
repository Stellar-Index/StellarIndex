'use client';

import Link from 'next/link';

import { Breadcrumbs } from '@/components/ui';
import { useLastPathSegment } from '@/lib/useLastPathSegment';

import { PoolReserves } from './PoolReserves';

/**
 * LendingPoolPathView — the runtime fallback for /lending/[pool] outside
 * the build-time pre-render (T278, same S1b shell pattern already used by
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
      <Breadcrumbs
        items={[
          { label: 'Home', href: '/' },
          { label: 'Lending', href: '/lending' },
          { label: pool ? `${pool.slice(0, 8)}…${pool.slice(-8)}` : 'Pool' },
        ]}
      />
      <header className="space-y-2">
        <h1 className="font-mono text-2xl tracking-tight break-all">
          {pool || 'Loading…'}
        </h1>
        <p className="text-ink-muted text-xs">
          Rendered live from the API (outside the build-time pre-render).
        </p>
        <Link
          href="/protocols/blend"
          className="text-brand-600 inline-block text-xs hover:underline"
        >
          Blend protocol →
        </Link>
      </header>
      {pool && <PoolReserves pool={pool} />}
    </div>
  );
}
