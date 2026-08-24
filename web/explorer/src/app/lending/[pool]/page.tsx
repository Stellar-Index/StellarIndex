import type { Metadata } from 'next';
import Link from 'next/link';
import { ExternalLink } from 'lucide-react';

import { Panel } from '@/components/reveal';
import { Breadcrumbs, Container } from '@/components/ui';
import { buildFetchData, failBuild } from '@/lib/buildFetch';
import {
  SITE_OG_IMAGES,
  SITE_TWITTER_IMAGES,
  serializeJsonLd,
} from '@/lib/seo';
import type { paths } from '@/api/types';

import { PoolReserves } from './PoolReserves';

type Params = Promise<{ pool: string }>;

// One /v1/lending/pools row, derived from the generated OpenAPI
// contract (src/api/types.ts, `make web-generate-api`).
type LendingPool = NonNullable<
  paths['/lending/pools']['get']['responses'][200]['content']['application/json']['data']
>[number];

// Curated annotations for every Blend mainnet contract we track.
// Mirrors the BLEND_POOL_META map in LendingPoolsTable; kept in
// sync so the detail route renders identical context whether the
// user arrived via the listing or a deep link.
// Sourced from docs/operations/wasm-audits/blend.md (Phase 4 walk).
const BLEND_POOL_LABELS: Record<
  string,
  { name: string; note?: string; deployedAt?: string; initiator?: string }
> = {
  CAQQR5SWBXKIGZKPBZDH3KM5GQ5GUTPKB7JAFCINLZBC5WXPJKRG3IM7: {
    name: 'Backstop V2',
    note: 'Holds the protocol-wide BLND-USDC LP shares that backstop pool insolvency. Receives auction proceeds when borrower positions liquidate at a loss.',
    deployedAt: '2025-04-14',
    initiator: 'GAX2VVWVHU5YQY5J3NJBXKHI3FFKZN54BE6GRJCWSIKSBZTQWJJNJMPC',
  },
  CDSYOAVXFY7SM5S64IZPPPYB4GVGGLMQVFREPSQQEZVIWXX5R23G4QSU: {
    name: 'Pool Factory V2',
    note: 'Spawns new isolated lending-pool contracts. Each user-facing pool (with its own reserves and risk parameters) is a child of this factory.',
    deployedAt: '2025-04-14',
    initiator: 'GAX2VVWVHU5YQY5J3NJBXKHI3FFKZN54BE6GRJCWSIKSBZTQWJJNJMPC',
  },
  CAJJZSGMMM3PD7N33TAPHGBUGTB43OC73HVIK2L2G6BNGGGYOSSYBXBD: {
    name: 'Pool #1 (genesis)',
    note: 'First pool deployed by the Pool Factory V2, ~4 minutes after the factory itself. Initiator overlaps with the protocol team — likely a reference/genesis pool.',
    deployedAt: '2025-04-14',
    initiator: 'GAX2VVWVHU5YQY5J3NJBXKHI3FFKZN54BE6GRJCWSIKSBZTQWJJNJMPC',
  },
  CBNR7PYFY775UG7W37B4OJG2OBBUKLFW6VIBHFDKKLR2HECPRMRZMDK3: {
    name: 'Pool #2',
    deployedAt: '2025-04-15',
    initiator: 'GBCAS7XIGDRZY4BMABJMGGW7J3YTITRRV5BTEMFQE5ZZSSVWHHX2ZSS4',
  },
  CCCCIQSDILITHMM7PBSLVDT5MISSY7R26MNZXCX4H7J5JQ5FPIYOGYFS: {
    name: 'Pool #3',
    deployedAt: '2025-04-17',
    initiator: 'GBCAS7XIGDRZY4BMABJMGGW7J3YTITRRV5BTEMFQE5ZZSSVWHHX2ZSS4',
  },
  CB4OFHAY2TAEYUVPOJS36S657C6NYMSIFUNCCA5AHYT46Y5XUID3O2ED: {
    name: 'Pool #4',
    deployedAt: '2025-05-01',
    initiator: 'GBIWJGAOSFC4KUPHXM573TKTWHMI7VW7D4GCHYZYH243Q6HVBV7ORBIT',
  },
  CAE7QVOMBLZ53CDRGK3UNRRHG5EZ5NQA7HHTFASEMYBWHG6MDFZTYHXC: {
    name: 'Pool #5',
    deployedAt: '2025-05-01',
    initiator: 'GBIWJGAOSFC4KUPHXM573TKTWHMI7VW7D4GCHYZYH243Q6HVBV7ORBIT',
  },
  CBYOBT7ZCCLQCBUYYIABZLSEGDPEUWXCUXQTZYOG3YBDR7U357D5ZIRF: {
    name: 'Pool #6',
    deployedAt: '2025-07-13',
    initiator: 'GCCI7K6QU6FVVIXWSLKRPTBKJCFBLEJKPTZMP27A2KL37N4ZL3OCM3GI',
  },
  CALRF5I2OCJCU577R6MZBCY5IIXNMAAG6PNMN7GUKEYIXBJCJN2FJRVI: {
    name: 'Pool #7',
    deployedAt: '2025-11-22',
    initiator: 'GDH3FRHOOWXYXEASH43N2VOVFOPJSVJF3EQFSLBLJYFPHOUAF4N4AETH',
  },
  CADR6Q2UOCDJAGXMAB2E6SRT35STLZ2IGLZUCXJQG7TC2LNKCU5RTQVY: {
    name: 'Pool #8',
    deployedAt: '2025-11-25',
    initiator: 'GDH3FRHOOWXYXEASH43N2VOVFOPJSVJF3EQFSLBLJYFPHOUAF4N4AETH',
  },
  CDMAVJPFXPADND3YRL4BSM3AKZWCTFMX27GLLXCML3PD62HEQS5FPVAI: {
    name: 'Pool #9',
    deployedAt: '2025-11-25',
    initiator: 'GDH3FRHOOWXYXEASH43N2VOVFOPJSVJF3EQFSLBLJYFPHOUAF4N4AETH',
  },
};

export async function generateStaticParams() {
  // Curated well-known factory contracts that don't emit auctions
  // and so don't show up in /v1/lending/pools — but operators and
  // users still deep-link to them. Keep these in the static-params
  // list so the routes pre-render even when the auction-stream
  // listing is empty.
  const curatedKeys = Object.keys(BLEND_POOL_LABELS).map((pool) => ({ pool }));
  // fetchLendingPools throws on persistent transport failure
  // (fail-hard, src/lib/buildFetch.ts); null = CI stub. An empty
  // listing is tolerable here — the curated keys carry the route.
  const pools = await fetchLendingPools();
  const fromAPI = (pools ?? [])
    .map((p) => ({ pool: p.pool ?? '' }))
    .filter((p) => p.pool);
  const seen = new Set<string>();
  const merged = [...fromAPI, ...curatedKeys].filter((p) => {
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
  const label =
    BLEND_POOL_LABELS[pool]?.name ?? `${pool.slice(0, 6)}…${pool.slice(-6)}`;
  const canonical = `https://stellarindex.io/lending/${pool}`;
  const title = `${label} — Blend lending pool`;
  const description = `Auction activity, user count, and contract metadata for the Blend pool at ${pool}.`;
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
 * Pre-fix both collapsed into `null` and the page baked "Auctions
 * (total): 0" — a false empirical claim frozen into the static export
 * until the next build.
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
  const { listed, row: data } = await fetchPool(pool);
  const label = BLEND_POOL_LABELS[pool];
  if (!data && !label) {
    // Real build: a param that is neither curated nor in the listing
    // can only mean generateStaticParams' source data vanished
    // mid-build — fail rather than bake an all-zero page for a real
    // pool (fail-hard contract, src/lib/buildFetch.ts).
    failBuild(
      `/lending/${pool}: promised by generateStaticParams but /v1/lending/pools no longer lists it`,
    );
  }

  // Schema.org BreadcrumbList — Home → Lending → <pool name or short hash>.
  const poolName = label?.name || `${pool.slice(0, 8)}…${pool.slice(-8)}`;
  const breadcrumbLD = {
    '@context': 'https://schema.org',
    '@type': 'BreadcrumbList',
    itemListElement: [
      {
        '@type': 'ListItem',
        position: 1,
        name: 'Home',
        item: 'https://stellarindex.io',
      },
      {
        '@type': 'ListItem',
        position: 2,
        name: 'Lending',
        item: 'https://stellarindex.io/lending',
      },
      {
        '@type': 'ListItem',
        position: 3,
        name: poolName,
        item: `https://stellarindex.io/lending/${pool}`,
      },
    ],
  };

  return (
    <Container className="space-y-6 py-8">
      <script
        type="application/ld+json"
        dangerouslySetInnerHTML={{ __html: serializeJsonLd(breadcrumbLD) }}
      />
      <Breadcrumbs
        items={[
          { label: 'Home', href: '/' },
          { label: 'Lending', href: '/lending' },
          { label: poolName },
        ]}
      />

      <header className="space-y-2">
        <div className="flex flex-wrap items-center gap-2">
          <span className="bg-up-subtle text-up-strong rounded-sm px-1.5 py-0.5 text-[11px] font-medium tracking-wider uppercase">
            Blend
          </span>
          {label && (
            <span className="bg-brand-100 text-brand-800 rounded-sm px-1.5 py-0.5 text-[11px] font-medium tracking-wider uppercase">
              {label.name}
            </span>
          )}
          {label?.deployedAt && (
            <span className="bg-surface-subtle text-ink-body rounded-sm px-1.5 py-0.5 font-mono text-[11px]">
              deployed {label.deployedAt}
            </span>
          )}
        </div>
        <h1 className="font-mono text-2xl tracking-tight break-all">
          {pool.slice(0, 8)}…{pool.slice(-8)}
        </h1>
        <p className="text-ink-muted font-mono text-xs break-all">{pool}</p>
        {label?.initiator && (
          <p className="text-ink-muted font-mono text-[11px]">
            Deployed by{' '}
            <a
              href={`https://stellar.expert/explorer/public/account/${label.initiator}`}
              target="_blank"
              rel="noreferrer noopener"
              className="text-brand-600 hover:underline"
              title={label.initiator}
            >
              {label.initiator.slice(0, 6)}…{label.initiator.slice(-4)}
            </a>
          </p>
        )}
        <div className="flex flex-wrap gap-3 pt-1 text-xs">
          <Link
            href="/protocols/blend"
            className="text-brand-600 hover:underline"
          >
            Blend protocol →
          </Link>
          <Link
            href={`/contracts/${encodeURIComponent(pool)}/`}
            className="text-brand-600 hover:underline"
          >
            Contract events →
          </Link>
          <a
            href={`https://stellar.expert/explorer/public/contract/${pool}`}
            target="_blank"
            rel="noreferrer noopener"
            className="text-brand-600 inline-flex items-center gap-1 hover:underline"
          >
            View on stellar.expert
            <ExternalLink className="h-3 w-3" />
          </a>
          <a
            href="https://blend.capital"
            target="_blank"
            rel="noreferrer noopener"
            className="text-ink-muted inline-flex items-center gap-1 hover:underline"
          >
            blend.capital
            <ExternalLink className="h-3 w-3" />
          </a>
        </div>
      </header>

      {label?.note && (
        <Panel title="About this contract">
          <p className="text-ink-body text-sm leading-relaxed">{label.note}</p>
        </Panel>
      )}

      <div className="grid grid-cols-1 gap-4 sm:grid-cols-3">
        <Stat
          label="Auctions (24h)"
          value={listed ? (data?.auctions_24h ?? 0) : null}
        />
        <Stat
          label="Auctions (total)"
          value={listed ? (data?.auctions_total ?? 0) : null}
        />
        <Stat
          label="Unique users (30d)"
          value={listed ? (data?.unique_users_30d ?? 0) : null}
        />
      </div>

      {!listed && (
        <p
          role="status"
          className="border-line bg-surface-subtle text-ink-muted rounded-md border px-3 py-2 text-xs"
        >
          Auction statistics are unavailable for this build — the lending
          listing returned no rows, so the figures above are unknown rather than
          zero. They refresh on the next build.
        </p>
      )}

      {data && (
        <Panel title="Last activity">
          <div className="space-y-1 text-sm">
            <div className="text-ink-body">
              Most recent auction event:{' '}
              <span className="text-ink font-mono">
                {data.last_seen ? new Date(data.last_seen).toUTCString() : '—'}
              </span>
            </div>
          </div>
        </Panel>
      )}

      <PoolReserves pool={pool} />
    </Container>
  );
}

// `value: null` means UNKNOWN (the listing didn't answer) and renders an
// em-dash; a real 0 still renders "0".
function Stat({ label, value }: { label: string; value: number | null }) {
  return (
    <div className="border-line bg-surface rounded-xl border p-4 shadow-sm">
      <div className="text-ink-muted text-[10px] tracking-wider uppercase">
        {label}
      </div>
      <div className="text-ink mt-1 font-mono text-2xl tabular-nums">
        {value == null ? (
          <span className="text-ink-faint">—</span>
        ) : (
          value.toLocaleString('en-US')
        )}
      </div>
    </div>
  );
}
