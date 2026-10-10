'use client';

import { useQuery } from '@tanstack/react-query';

import { Panel } from '@/components/reveal';
import { apiGet, asExample } from '@/api/client';
import { AssetText } from '@/components/AssetLink';
import {
  Container,
  PageHeader,
  Stat,
  StatCell,
  StatGrid,
} from '@/components/ui';
import { useLedgerFollow } from '@/lib/live/hooks';
import { CopyHash } from '../explorer-shared';
import {
  PoolDepthDetail,
  type LiquidityPoolRow,
  assetLabel,
  displayUnits,
  midPriceLabel,
} from './PoolDepthDetail';

const POOL_ID_RE = /^(L[A-Z2-7]{55}|[0-9a-fA-F]{64})$/;

/** One native (CAP-38) liquidity pool: the /v1/liquidity-pools?pool= row. */
export function PoolView({ id }: { id: string }) {
  const valid = POOL_ID_RE.test(id);
  useLedgerFollow(['/v1/liquidity-pools']);
  const q = useQuery<LiquidityPoolRow | null>({
    queryKey: ['/v1/liquidity-pools', id],
    queryFn: async () => {
      const env = await apiGet<{ data: LiquidityPoolRow[] }>(
        `/v1/liquidity-pools?pool=${encodeURIComponent(id)}`,
      );
      return env.data?.[0] ?? null;
    },
    enabled: valid,
    staleTime: 30_000,
    retry: false,
  });
  const source = asExample('/v1/liquidity-pools', { pool: id });
  const row = q.data;

  let body: React.ReactNode;
  if (!valid) {
    body = (
      <Panel
        headingLevel={2}
        title="Liquidity pool"
        bodyClassName="text-sm text-ink-body"
      >
        Not a native liquidity-pool id: expected an L… strkey or 64-char hex.
      </Panel>
    );
  } else if (q.isLoading) {
    body = (
      <Panel
        headingLevel={2}
        title="Liquidity pool"
        source={source}
        bodyClassName="text-sm text-ink-muted"
      >
        Loading…
      </Panel>
    );
  } else if (!row) {
    body = (
      <Panel
        headingLevel={2}
        title="Liquidity pool"
        source={source}
        bodyClassName="text-sm text-ink-body"
      >
        {q.isError
          ? `Lookup failed: ${q.error instanceof Error ? q.error.message : 'unknown error'}.`
          : 'No native liquidity pool with that id in the lake.'}
      </Panel>
    );
  } else {
    const a = assetLabel(row.reserve_a.asset);
    const b = assetLabel(row.reserve_b.asset);
    body = (
      <>
        <Panel
          headingLevel={2}
          title="Pool"
          source={source}
          bodyClassName="space-y-3"
        >
          <div className="text-lg font-semibold">
            <AssetText canonical={row.reserve_a.asset} /> /{' '}
            <AssetText canonical={row.reserve_b.asset} />
          </div>
          <CopyHash value={row.pool} head={16} tail={16} />
          <StatGrid cols={4}>
            <StatCell>
              <Stat
                label={`Reserve ${a}`}
                value={displayUnits(
                  row.reserve_a.reserve,
                  row.reserve_a.decimals,
                )}
              />
            </StatCell>
            <StatCell>
              <Stat
                label={`Reserve ${b}`}
                value={displayUnits(
                  row.reserve_b.reserve,
                  row.reserve_b.decimals,
                )}
              />
            </StatCell>
            <StatCell>
              <Stat
                label="Mid price"
                value={midPriceLabel(row.mid_price_a_in_b)}
                sub={`${b} per ${a}`}
              />
            </StatCell>
            <StatCell>
              <Stat
                label="Liquidity providers"
                value={row.trustlines.toLocaleString('en-US')}
                sub="pool-share trustlines"
              />
            </StatCell>
            <StatCell>
              <Stat label="Fee" value={`${row.fee_bps / 100}%`} />
            </StatCell>
            <StatCell>
              <Stat
                label="Total shares"
                value={displayUnits(row.total_shares, 7)}
              />
            </StatCell>
            <StatCell>
              <Stat label="Model" value={row.model.replace(/_/g, ' ')} />
            </StatCell>
            <StatCell>
              <Stat
                label="As of ledger"
                value={row.as_of_ledger.toLocaleString('en-US')}
              />
            </StatCell>
          </StatGrid>
        </Panel>
        <Panel
          headingLevel={2}
          title="Depth"
          hint="Constant-product estimate from current reserves, not an order book"
          source={source}
        >
          <PoolDepthDetail row={row} />
        </Panel>
      </>
    );
  }

  return (
    <Container className="space-y-6 py-8">
      <PageHeader
        breadcrumbs={[
          { label: 'Home', href: '/' },
          { label: 'Liquidity pools', href: '/liquidity-pools' },
          { label: id ? `${id.slice(0, 6)}…${id.slice(-4)}` : 'pool' },
        ]}
        title="Liquidity pool"
        description="A Stellar native liquidity pool's reserves and depth."
      />
      {body}
    </Container>
  );
}
