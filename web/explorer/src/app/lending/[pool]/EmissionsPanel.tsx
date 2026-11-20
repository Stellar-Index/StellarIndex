'use client';

import { useQuery } from '@tanstack/react-query';

import { Panel } from '@/components/reveal';
import { Button, TBody, TR, Table, Td, Th, THead } from '@/components/ui';
import { HBarList, type HBarItem } from '@/components/charts/Bars';
import { apiGet, asExample, type Envelope } from '@/api/client';
import type { paths } from '@/api/types';
import { formatBaseUnits, formatCompactTokenUnits } from '@/lib/format';
import { shortAssetText } from '@/lib/asset-label';
import { useAssetText } from '../lendingUsd';

export type Emissions =
  paths['/lending/pools/{pool}/emissions']['get']['responses'][200]['content']['application/json']['data'];

const DAY = 86_400n;
const TIER_TEXT = {
  FirstLoss: 'first loss',
  SecondLoss: 'second loss',
  ThirdLoss: 'third loss',
} as const;
const UNAVAILABLE_TEXT: Record<string, string> = {
  blend_v1_emission_storage_unverified:
    'Blend v1 emission storage is not verified, so none is shown',
  pool_instance_unavailable: 'the pool’s storage is not in the lake',
  emission_storage_reader_unavailable:
    'this deployment cannot read emission storage',
};

/** Emission-token base units per day from a raw eps, exact integer division. */
export function perDayBase(eps: string, epsDecimals: number): string | null {
  if (!/^\d+$/.test(eps)) return null;
  return ((BigInt(eps) * DAY) / 10n ** BigInt(epsDecimals)).toString();
}

export type EmissionRow = {
  id: string;
  label: string;
  perDay: string | null;
  live: boolean;
  archived: boolean;
};

/** One row per stream (reserve side, backstop tier) in the served order. */
export function emissionRows(
  d: Emissions,
  assetText: (a: string) => string = shortAssetText,
): EmissionRow[] {
  const res = d.reserves.map((r) => ({
    id: `r${r.token_id}`,
    label: `${r.asset ? assetText(r.asset) :`reserve ${Math.floor(r.token_id / 2)}`} ${r.side === 'supply' ? 'suppliers' : 'borrowers'}`,
    perDay: perDayBase(r.eps, d.eps_decimals),
    live: r.live,
    archived: r.archived,
  }));
  const bs = d.backstop.map((b, i) => ({
    id: `b${b.tier ?? i}`,
    label: b.tier ? `Backstop, ${TIER_TEXT[b.tier]}` : 'Backstop',
    perDay: perDayBase(b.eps, d.eps_decimals),
    live: b.live,
    archived: b.archived,
  }));
  return [...res, ...bs];
}

/** Live streams with a known scale as bars; the table lists every stream. */
export function emissionBars(
  rows: EmissionRow[],
  decimals: number | null,
): HBarItem[] {
  if (decimals == null) return [];
  return rows
    .filter((r) => r.live && r.perDay != null && r.perDay !== '0')
    .map((r) => ({
      id: r.id,
      label: r.label,
      value: Number(r.perDay),
      display: formatCompactTokenUnits(r.perDay, decimals),
      title: `${formatBaseUnits(r.perDay, decimals, 7)} per day`,
    }))
    .sort((a, b) => b.value - a.value);
}

export function EmissionsPanel({ pool }: { pool: string }) {
  const q = useQuery({
    queryKey: ['/v1/lending/pools/{pool}/emissions', pool],
    retry: false,
    staleTime: 300_000,
    queryFn: async () =>
      (
        await apiGet<Envelope<Emissions>>(
          `/v1/lending/pools/${encodeURIComponent(pool)}/emissions`,
          {},
        )
      ).data,
  });
  const assetText = useAssetText();
  const d = q.data;
  const token = d?.emission_token ? assetText(d.emission_token) : 'tokens';
  const dec = d?.emission_token_decimals ?? null;
  const rows = d ? emissionRows(d, assetText) : [];
  const bars = emissionBars(rows, dec);
  return (
    <Panel
      headingLevel={2}
      title="Emissions"
      hint={`${token} emitted per day to each side of each reserve and to the backstop, from contract storage`}
      source={asExample(`/v1/lending/pools/${pool}/emissions`, {})}
      bodyClassName="space-y-3"
    >
      {q.isLoading && (
        <p className="text-ink-muted text-sm">Loading emissions…</p>
      )}
      {q.isError && (
        <p role="alert" className="text-ink-muted text-sm">
          Emissions are unavailable right now.
          <Button
            variant="secondary"
            size="sm"
            className="ml-3"
            onClick={() => void q.refetch()}
          >
            Retry
          </Button>
        </p>
      )}
      {d && !d.storage_available && (
        <p className="text-ink-muted text-sm">
          Emissions cannot be shown:{' '}
          {UNAVAILABLE_TEXT[d.storage_unavailable_reason ?? ''] ??
            'emission storage is unavailable'}
          .
        </p>
      )}
      {d && d.storage_available && rows.length === 0 && (
        <p className="text-ink-muted text-sm">
          This pool has no emission streams.
        </p>
      )}
      {bars.length > 0 && (
        <HBarList
          items={bars}
          ariaLabel={`${token} per day by stream: ${bars.map((b) => `${b.label} ${b.display}`).join(', ')}`}
        />
      )}
      {rows.length > 0 && (
        <div className="overflow-x-auto">
          <Table>
            <THead>
              <tr>
                <Th>Stream</Th>
                <Th align="right">{token} per day</Th>
                <Th align="right">Status</Th>
              </tr>
            </THead>
            <TBody>
              {rows.map((r) => (
                <TR key={r.id}>
                  <Td>{r.label}</Td>
                  <Td
                    align="right"
                    className="font-mono tabular-nums"
                    title={
                      r.perDay != null && dec != null
                        ? formatBaseUnits(r.perDay, dec, 7)
                        : undefined
                    }
                  >
                    {r.perDay == null
                      ? '—'
                      : dec == null
                        ? `${r.perDay} base units`
                        : formatCompactTokenUnits(r.perDay, dec)}
                  </Td>
                  <Td align="right" className="text-ink-muted text-xs">
                    {r.live ? 'live' : 'ended'}
                    {r.archived ? ', archived' : ''}
                  </Td>
                </TR>
              ))}
            </TBody>
          </Table>
        </div>
      )}
      {d && rows.length > 0 && bars.length < rows.length && (
        <p className="text-ink-muted text-xs">
          The chart shows live streams
          {dec == null ? ' once the token’s decimals are known' : ''}; ended
          streams are listed in the table only.
        </p>
      )}
    </Panel>
  );
}
