import type { Metadata } from 'next';
import Link from 'next/link';
import { Suspense } from 'react';
import { ExternalLink } from 'lucide-react';

import { Panel } from '@/components/reveal';
import {
  Badge,
  Container,
  PageHeader,
  Stat,
  StatCell,
  StatGrid,
} from '@/components/ui';
import { buildFetchData, failBuild } from '@/lib/buildFetch';
import { SITE_OG_IMAGES, SITE_TWITTER_IMAGES, shellMetadata } from '@/lib/seo';
import type { paths } from '@/api/types';

import { LendingPoolPathView } from './LendingPoolPathView';
import { AuctionsPanel } from './AuctionsPanel';
import { BackstopPanel } from './BackstopPanel';
import { ConfigHistory } from './ConfigHistory';
import { PoolDetails } from './PoolDetails';
import { PoolReserves } from './PoolReserves';
import { CURRENT_NETWORK } from '@/lib/networks';
import { LENDING_POOL_META, poolProtocol } from '../poolMeta';
import { hrefFor } from '@/lib/hrefFor';

type Params = Promise<{ pool: string }>;

// One /v1/lending/pools row, derived from the generated OpenAPI
// contract (src/api/types.ts, `make web-generate-api`).
type LendingPool = NonNullable<
  paths['/lending/pools']['get']['responses'][200]['content']['application/json']['data']
>[number];

export async function generateStaticParams() {
  // Curated well-known factory contracts that don't emit auctions
  // and so don't show up in /v1/lending/pools — but operators and
  // users still deep-link to them. Keep these in the static-params
  // list so the routes pre-render even when the auction-stream
  // listing is empty.
  const curatedKeys = Object.keys(LENDING_POOL_META).map((pool) => ({ pool }));
  // fetchLendingPools throws on persistent transport failure
  // (fail-hard, src/lib/buildFetch.ts); null = CI stub. An empty
  // listing is tolerable here — the curated keys carry the route.
  const pools = await fetchLendingPools();
  const fromAPI = (pools ?? [])
    .map((p) => ({ pool: p.pool ?? '' }))
    .filter((p) => p.pool);
  const seen = new Set<string>();
  const merged = [...fromAPI, ...curatedKeys, { pool: 'shell' }].filter((p) => {
    if (seen.has(p.pool)) return false;
    seen.add(p.pool);
    return true;
  });
  return merged;
}

export async function generateMetadata({
  params,
}: {
  params: Params;
}): Promise<Metadata> {
  const { pool } = await params;
  // The runtime-fallback shell's baked HTML serves for arbitrary pools
  // outside the build-time pre-render (see LendingPoolPathView).
  if (pool.toLowerCase() === 'shell') {
    return shellMetadata(
      'Lending pool',
      'Lending-pool detail, rendered live from the Stellar Index API.',
    );
  }
  const label =
    LENDING_POOL_META[pool]?.name ?? `${pool.slice(0, 6)}…${pool.slice(-6)}`;
  const { row } = await fetchPool(pool);
  const protocol = poolProtocol(pool, row?.protocol)?.name;
  const canonical = `${CURRENT_NETWORK.explorerUrl}/lending/${pool}`;
  const title = `${label} — ${protocol ? `${protocol} lending pool` : 'Lending pool'}`;
  const description = `Auction activity, user count, and contract metadata for the ${protocol ? `${protocol} pool` : 'lending pool'} at ${pool}.`;
  return {
    title,
    description,
    alternates: { canonical },
    openGraph: {
      title,
      description,
      url: canonical,
      type: 'website',
      images: SITE_OG_IMAGES,
    },
    twitter: {
      card: 'summary_large_image',
      title,
      description,
      images: SITE_TWITTER_IMAGES,
    },
  };
}

// Memoised via buildFetch: generateStaticParams and every
// /lending/[pool] page render share ONE /v1/lending/pools call.
function fetchLendingPools(): Promise<LendingPool[] | null> {
  return buildFetchData<LendingPool[]>('/v1/lending/pools');
}

/**
 * fetchPool resolves one pool's row against the listing, keeping the
 * two very different "no row" cases apart:
 *
 * - `listed: false` — the listing itself carried no rows at all: it was
 *   null (CI stub / authoritative 4xx) or empty, which is exactly what
 *   `/v1/lending/pools` serves when no LendingReader is wired
 *   (internal/api/v1/lending.go, "200 + empty array"). We know NOTHING
 *   about this pool's auctions; every stat must render '—'.
 * - `listed: true, row: null` — the reader answered with other pools but
 *   not this one (the curated factory/backstop contracts emit no
 *   auctions). That is a real, derived zero and renders as 0.
 *
 * Collapsing both into `null` would bake "Auctions (total): 0" — a false
 * empirical claim frozen into the static export until the next build.
 */
async function fetchPool(
  pool: string,
): Promise<{ listed: boolean; row: LendingPool | null }> {
  const pools = await fetchLendingPools();
  if (!pools || pools.length === 0) return { listed: false, row: null };
  return { listed: true, row: pools.find((p) => p.pool === pool) ?? null };
}

export default async function LendingPoolPage({ params }: { params: Params }) {
  const { pool } = await params;
  // functions/lending/[[path]].js serves this same built HTML for
  // any /lending/* pool id outside the pre-render set above — a pool
  // deployed by a lending factory between builds otherwise hard-404'd on
  // the static host. Case-insensitive to match the sentinel every other
  // long-tail shell accepts (assets/markets emit case variants of every
  // real slug; lending pool ids are uppercase-only, so this only ever
  // matches the literal sentinel).
  if (pool.toLowerCase() === 'shell') {
    return (
      <Container className="space-y-6 py-8">
        <Suspense fallback={null}>
          <LendingPoolPathView />
        </Suspense>
      </Container>
    );
  }
  const { listed, row: data } = await fetchPool(pool);
  const label = LENDING_POOL_META[pool];
  const protocol = poolProtocol(pool, data?.protocol);
  if (!data && !label) {
    // Real build: a param that is neither curated nor in the listing
    // can only mean generateStaticParams' source data vanished
    // mid-build — fail rather than bake an all-zero page for a real
    // pool (fail-hard contract, src/lib/buildFetch.ts).
    failBuild(
      `/lending/${pool}: promised by generateStaticParams but /v1/lending/pools no longer lists it`,
    );
  }

  const poolName = label?.name || `${pool.slice(0, 8)}…${pool.slice(-8)}`;

  return (
    <Container className="space-y-6 py-8">
      {/* BreadcrumbList JSON-LD derives from this Crumb[] inside Breadcrumbs. */}
      <PageHeader
        breadcrumbs={[
          { label: 'Home', href: '/' },
          { label: 'Lending', href: '/lending' },
          { label: poolName },
        ]}
        title={
          <span className="font-mono break-all">
            {pool.slice(0, 8)}…{pool.slice(-8)}
          </span>
        }
        meta={
          <span className="flex flex-wrap items-center gap-2">
            {protocol && <Badge>{protocol.name}</Badge>}
            {label && <Badge tone="brand">{label.name}</Badge>}
            {label?.deployedAt && <span>deployed {label.deployedAt}</span>}
          </span>
        }
      />
      <div className="text-ink-muted space-y-2 text-xs">
        <p className="font-mono break-all">{pool}</p>
        {label?.initiator && (
          <p>
            Deployed by{' '}
            <Link
              href={`/accounts/${label.initiator}/`}
              className="text-brand-600 font-mono hover:underline"
              title={label.initiator}
            >
              {label.initiator.slice(0, 6)}…{label.initiator.slice(-4)}
            </Link>
          </p>
        )}
        <div className="flex flex-wrap gap-3">
          {protocol && (
            <Link
              href={hrefFor.protocol(protocol.slug)}
              className="text-brand-600 hover:underline"
            >
              {protocol.name} protocol
            </Link>
          )}
          <Link
            href={`/contracts/${encodeURIComponent(pool)}/`}
            className="text-brand-600 hover:underline"
          >
            Contract events
          </Link>
          {protocol?.website && (
            <a
              href={protocol.website}
              target="_blank"
              rel="noreferrer noopener"
              className="inline-flex items-center gap-1 hover:underline"
            >
              {new URL(protocol.website).host.replace(/^www\./, '')}
              <ExternalLink className="h-3 w-3" aria-hidden="true" />
            </a>
          )}
        </div>
      </div>
      {label?.note && (
        <Panel headingLevel={2} title="About this contract">
          <p className="text-ink-body text-sm leading-relaxed">{label.note}</p>
        </Panel>
      )}

      <StatGrid cols={4} className="grid-cols-2">
        <StatCell>
          <Stat
            label="Auctions (24h)"
            value={count(listed ? (data?.auctions_24h ?? 0) : null)}
          />
        </StatCell>
        <StatCell>
          <Stat
            label="Auctions (total)"
            value={count(listed ? (data?.auctions_total ?? 0) : null)}
          />
        </StatCell>
        <StatCell>
          <Stat
            label="Unique users (30d)"
            value={count(listed ? (data?.unique_users_30d ?? 0) : null)}
          />
        </StatCell>
        <StatCell>
          <Stat
            label="Last auction"
            value={
              <span className="text-base">
                {data?.last_seen
                  ? `${data.last_seen.replace('T', ' ').slice(0, 16)} UTC`
                  : '—'}
              </span>
            }
          />
        </StatCell>
      </StatGrid>
      {!listed && (
        <p role="status" className="text-ink-muted text-xs">
          Auction statistics are unavailable for this build.
        </p>
      )}
      <PoolDetails pool={pool} />
      <PoolReserves pool={pool} />
      <BackstopPanel pool={pool} />
      <AuctionsPanel pool={pool} />
      <ConfigHistory pool={pool} />
    </Container>
  );
}

// null means UNKNOWN (the listing didn't answer) and renders a dash; a real
// 0 still renders "0".
function count(value: number | null) {
  return value == null ? (
    <span className="text-ink-faint">—</span>
  ) : (
    value.toLocaleString('en-US')
  );
}
