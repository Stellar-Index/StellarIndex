'use client';

import Link from 'next/link';
import { useQuery } from '@tanstack/react-query';

import { Panel } from '@/components/reveal';
import { apiGet, asExample } from '@/api/client';
import { useAsset, useAssetSupply, type AssetDetail } from '@/api/hooks';
import { assetSlug } from '@/components/AssetLink';
import { assetHref } from '@/lib/fiat-slugs';
import {
  formatCompact,
  formatCompactUnits,
  formatBaseUnits,
  formatPrice,
  truncateMiddle,
} from '@/lib/format';
import type { paths } from '@/api/types';
import { SidebarAssetIcon } from '../assets/[slug]/SidebarAssetIcon';
import {
  SupplyFlowsBar,
  buildSupplyFlowRows,
} from '../assets/[slug]/SupplyFlowsBar';
import { onChainSupplyDecimals } from '../assets/[slug]/SupplyTabPanel';

type HoldersResp = NonNullable<
  paths['/assets/{asset_id}/holders']['get']['responses'][200]['content']['application/json']['data']
>;

const TOP_HOLDERS = 5;

function Stat({ label, value }: { label: string; value: string }) {
  return (
    <div className="border-line bg-surface rounded-lg border p-3">
      <div className="text-ink-muted text-[11px] tracking-wider uppercase">
        {label}
      </div>
      <div className="text-ink mt-1 font-mono text-lg font-semibold">
        {value}
      </div>
    </div>
  );
}

function usd(raw: string | null | undefined): string | null {
  const v = formatCompactUnits(raw, 0);
  return v === '—' ? null : `$${v}`;
}

function useTopHolders(assetID: string | undefined) {
  return useQuery<HoldersResp>({
    queryKey: ['/v1/assets/{id}/holders', assetID, TOP_HOLDERS],
    enabled: !!assetID,
    retry: false,
    staleTime: 60_000,
    queryFn: async () => {
      const env = await apiGet<{ data: HoldersResp }>(
        `/v1/assets/${encodeURIComponent(assetID ?? '')}/holders`,
        { limit: TOP_HOLDERS },
      );
      return env.data;
    },
  });
}

/**
 * SacAssetPanel — the classic asset a Stellar Asset Contract wraps, with
 * its supply, top holders and mint/burn movement. /v1/assets/{id} resolves
 * the SAC address to the asset; the three panels load independently.
 */
export function SacAssetPanel({ contractId }: { contractId: string }) {
  const asset = useAsset(contractId);
  const a = asset.data?.data;

  return (
    <div className="space-y-4" data-testid="sac-asset-panel">
      <Panel
        title="Wrapped asset"
        hint="Stellar Asset Contract"
        source={asExample('/v1/assets/{asset_id}', { asset_id: contractId })}
        bodyClassName="space-y-3"
      >
        {asset.isLoading ? (
          <p className="text-ink-muted text-sm">Loading asset…</p>
        ) : !a ? (
          <p className="text-ink-muted text-sm">
            This is a Stellar Asset Contract; the wrapped asset could not be
            resolved right now.
          </p>
        ) : (
          <AssetIdentity a={a} />
        )}
      </Panel>
      {a && <SacMovement a={a} contractId={contractId} />}
    </div>
  );
}

function AssetIdentity({ a }: { a: AssetDetail }) {
  const decimals = a.decimals ?? 7;
  const code = a.code || (a.type === 'native' ? 'XLM' : a.asset_id);
  const slug = assetSlug(a.asset_id);
  const stats: { label: string; value: string | null }[] = [
    {
      label: 'Circulating supply',
      value:
        a.circulating_supply != null
          ? formatCompactUnits(a.circulating_supply, decimals)
          : null,
    },
    {
      label: 'Total supply',
      value:
        a.total_supply != null
          ? formatCompactUnits(a.total_supply, decimals)
          : null,
    },
    {
      label: 'Price',
      value: a.price_usd ? `$${formatPrice(a.price_usd)}` : null,
    },
    { label: 'Market cap', value: usd(a.market_cap_usd) },
  ];
  return (
    <>
      <div className="flex items-center gap-3">
        <SidebarAssetIcon image={a.image} code={code} />
        <div className="min-w-0">
          <div className="flex flex-wrap items-baseline gap-2">
            {slug ? (
              <Link
                href={assetHref(slug)}
                className="text-brand-600 text-lg font-semibold hover:underline"
              >
                {code}
              </Link>
            ) : (
              <span className="text-lg font-semibold">{code}</span>
            )}
            {a.name && a.name !== code && (
              <span className="text-ink-muted truncate text-sm">{a.name}</span>
            )}
          </div>
          <div className="text-ink-muted text-xs">
            {a.issuer ? (
              <>
                Issuer{' '}
                <Link
                  href={`/accounts/${encodeURIComponent(a.issuer)}/`}
                  className="text-brand-600 font-mono hover:underline"
                  title={a.issuer}
                >
                  {truncateMiddle(a.issuer, 6, 6)}
                </Link>
              </>
            ) : (
              'Native asset'
            )}
          </div>
        </div>
      </div>
      <div className="grid grid-cols-2 gap-3 sm:grid-cols-3 lg:grid-cols-5">
        {stats
          .filter((s) => s.value != null)
          .map((s) => (
            <Stat key={s.label} label={s.label} value={s.value as string} />
          ))}
        <HoldersStat assetID={a.asset_id} />
      </div>
    </>
  );
}

function HoldersStat({ assetID }: { assetID: string }) {
  const holders = useTopHolders(assetID);
  const n = holders.data?.holder_count;
  if (n == null) return null;
  return <Stat label="Holders" value={formatCompact(n)} />;
}

function SacMovement({
  a,
  contractId,
}: {
  a: AssetDetail;
  contractId: string;
}) {
  const supply = useAssetSupply(contractId);
  const holders = useTopHolders(a.asset_id);
  const slug = assetSlug(a.asset_id);
  const s = supply.data?.data;
  const supplyDecimals = s ? onChainSupplyDecimals(s, a.decimals) : null;
  const decimals = a.decimals ?? 7;
  const rows = holders.data?.holders ?? [];

  return (
    <div className="grid gap-4 lg:grid-cols-2">
      <Panel
        title="Supply movement"
        hint="minted, burned, clawed back"
        source={asExample('/v1/assets/{asset_id}/supply', {
          asset_id: contractId,
        })}
      >
        {supply.isLoading ? (
          <p className="text-ink-muted text-sm">Loading…</p>
        ) : !s || supplyDecimals == null ? (
          <p className="text-ink-muted text-sm">
            No mint/burn flows recorded for this asset.
          </p>
        ) : (
          <SupplyFlowsBar rows={buildSupplyFlowRows(s, supplyDecimals)} />
        )}
      </Panel>
      <Panel
        title="Top holders"
        source={asExample(`/v1/assets/${a.asset_id}/holders`, {
          limit: TOP_HOLDERS,
        })}
      >
        {holders.isLoading ? (
          <p className="text-ink-muted text-sm">Loading…</p>
        ) : rows.length === 0 ? (
          <p className="text-ink-muted text-sm">No holder data yet.</p>
        ) : (
          <ol className="divide-line-subtle divide-y text-sm">
            {rows.map((h, i) => (
              <li
                key={h.account_id}
                className="flex items-center justify-between gap-3 py-1.5"
              >
                <span className="text-ink-faint w-4 font-mono text-xs">
                  {i + 1}
                </span>
                <Link
                  href={`/accounts/${encodeURIComponent(h.account_id ?? '')}/`}
                  className="text-brand-600 flex-1 font-mono text-xs hover:underline"
                  title={h.account_id}
                >
                  {truncateMiddle(h.account_id, 8, 6)}
                </Link>
                <span className="text-ink-body font-mono tabular-nums">
                  {formatBaseUnits(h.balance, decimals)}
                </span>
              </li>
            ))}
          </ol>
        )}
        {slug && (
          <Link
            href={assetHref(slug)}
            className="text-brand-600 mt-2 inline-block text-xs hover:underline"
          >
            View all on the asset page →
          </Link>
        )}
      </Panel>
    </div>
  );
}
