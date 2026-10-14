'use client';

import Link from 'next/link';
import { useQuery } from '@tanstack/react-query';

import { Panel } from '@/components/reveal';
import {
  Button,
  Stat,
  StatCell,
  StatGrid,
  TBody,
  TR,
  Table,
  Td,
  Th,
  THead,
} from '@/components/ui';
import { apiGet, asExample } from '@/api/client';
import { useAsset, useAssetSupply, type AssetDetail } from '@/api/hooks';
import { assetSlug } from '@/components/AssetLink';
import { assetHref } from '@/lib/fiat-slugs';
import {
  formatCompact,
  formatCompactUnits,
  formatPriceSmall,
  formatUnitsReadable,
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
 * SacAssetPanel — the wrapped-asset view for a Stellar Asset Contract:
 * identity and market stats from /v1/assets, then supply flows and the
 * top holders. The asset page remains the full record.
 */
export function SacAssetPanel({ contractId }: { contractId: string }) {
  const asset = useAsset(contractId);
  const a = asset.data?.data;
  return (
    <div className="space-y-6" data-testid="sac-asset-panel">
      <Panel
        title="Wrapped asset"
        hint="Stellar Asset Contract"
        source={asExample('/v1/assets/{asset_id}', { asset_id: contractId })}
        bodyClassName="space-y-4"
      >
        {asset.isLoading ? (
          <p className="text-ink-muted text-sm">Loading asset…</p>
        ) : !a ? (
          <p className="text-ink-muted text-sm">
            This is a Stellar Asset Contract, but the asset lookup did not
            return.
            <Button
              variant="secondary"
              size="sm"
              className="ml-3"
              onClick={() => asset.refetch()}
            >
              Retry
            </Button>
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
  const name = a.name ?? (a.type === 'native' ? 'Stellar Lumens' : undefined);
  const holders = useTopHolders(a.asset_id);
  const holderCount = holders.data?.holder_count;
  const stats = (
    [
      {
        label: 'Price',
        value: a.price_usd ? `$${formatPriceSmall(a.price_usd)}` : null,
      },
      { label: 'Market cap', value: usd(a.market_cap_usd) },
      {
        label: 'Circulating',
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
        label: 'Holders',
        value: holderCount != null ? formatCompact(holderCount) : null,
      },
    ] as { label: string; value: string | null }[]
  ).filter((s): s is { label: string; value: string } => s.value != null);
  const cols = stats.length >= 5 ? 5 : stats.length === 4 ? 4 : 3;
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
            {name && name !== code && (
              <span className="text-ink-muted truncate text-sm">{name}</span>
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
      {stats.length > 0 && (
        <StatGrid cols={cols} className="grid-cols-2">
          {stats.map((s, i) => (
            <StatCell
              key={s.label}
              className={
                i === stats.length - 1 && stats.length % 2 === 1
                  ? 'col-span-2 sm:col-span-1'
                  : undefined
              }
            >
              <Stat label={s.label} value={s.value} />
            </StatCell>
          ))}
        </StatGrid>
      )}
    </>
  );
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
    <div className="grid gap-6 lg:grid-cols-2">
      <Panel
        title="Supply movement"
        hint="Minted, burned and clawed back"
        source={asExample('/v1/assets/{asset_id}/supply', {
          asset_id: contractId,
        })}
      >
        {supply.isLoading ? (
          <p className="text-ink-muted text-sm">Loading…</p>
        ) : supply.isError ? (
          <p className="text-ink-muted text-sm">
            The supply lookup did not return.
            <Button
              variant="secondary"
              size="sm"
              className="ml-3"
              onClick={() => supply.refetch()}
            >
              Retry
            </Button>
          </p>
        ) : !s || supplyDecimals == null ? (
          <p className="text-ink-muted text-sm">
            No mint or burn flows recorded for this asset.
          </p>
        ) : (
          <SupplyFlowsBar rows={buildSupplyFlowRows(s, supplyDecimals)} />
        )}
      </Panel>
      <Panel
        title="Top holders"
        hint={`Largest ${TOP_HOLDERS} balances`}
        source={asExample(`/v1/assets/${a.asset_id}/holders`, {
          limit: TOP_HOLDERS,
        })}
        bodyClassName="-mx-4 -mb-4"
      >
        {holders.isLoading ? (
          <p className="text-ink-muted px-4 pb-4 text-sm">Loading…</p>
        ) : rows.length === 0 ? (
          <p className="text-ink-muted px-4 pb-4 text-sm">
            No holder data yet.
          </p>
        ) : (
          <div className="overflow-x-auto">
            <Table>
              <THead>
                <tr>
                  <Th className="w-10">#</Th>
                  <Th>Account</Th>
                  <Th align="right">Balance</Th>
                </tr>
              </THead>
              <TBody>
                {rows.map((h, i) => (
                  <TR key={h.account_id}>
                    <Td className="text-ink-faint font-mono text-xs">
                      {i + 1}
                    </Td>
                    <Td>
                      <Link
                        href={`/accounts/${encodeURIComponent(h.account_id ?? '')}/`}
                        className="text-brand-600 font-mono text-xs hover:underline"
                      >
                        {truncateMiddle(h.account_id, 8, 6)}
                      </Link>
                    </Td>
                    <Td align="right" className="font-mono tabular-nums">
                      {formatUnitsReadable(h.balance, decimals)}
                    </Td>
                  </TR>
                ))}
              </TBody>
            </Table>
          </div>
        )}
        {slug && (
          <div className="border-line border-t px-4 py-3">
            <Link
              href={assetHref(slug)}
              className="text-brand-600 text-xs hover:underline"
            >
              View all on the asset page
            </Link>
          </div>
        )}
      </Panel>
    </div>
  );
}
