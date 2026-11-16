'use client';

import Link from 'next/link';

import { PageHeader } from '@/components/ui';
import { useLastPathSegment } from '@/lib/useLastPathSegment';

import { poolProtocol } from '../poolMeta';
import { PoolReserves } from './PoolReserves';
import { hrefFor } from '@/lib/hrefFor';

/**
 * LendingPoolPathView — the runtime fallback for /lending/[pool] outside
 * the build-time pre-render (same S1b shell pattern already used by
 * /assets and /markets — see AssetPathView / PairPathView).
 *
 * generateStaticParams enumerates every pool /v1/lending/pools currently
 * lists plus the curated factory/backstop contracts, so a pool a lending
 * factory spawns between builds hard-404'd on the static host until
 * functions/lending/[[path]].js existed to serve this shell. The real
 * pool id is read from the URL (not a build-time param) — PoolReserves is
 * already a client component and fetches its own data live.
 */
export function LendingPoolPathView() {
  const pool = useLastPathSegment();
  // Only curated pools resolve a protocol here; the shell has no listing row.
  const protocol = pool ? poolProtocol(pool) : null;

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
      {protocol && (
        <Link
          href={hrefFor.protocol(protocol.slug)}
          className="text-brand-600 inline-block text-xs hover:underline"
        >
          {protocol.name} protocol
        </Link>
      )}
      {pool && <PoolReserves pool={pool} />}
    </div>
  );
}
