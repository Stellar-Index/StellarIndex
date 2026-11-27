'use client';

import { useQuery } from '@tanstack/react-query';

import { Panel } from '@/components/reveal';
import { HBarList } from '@/components/charts/Bars';
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
import { apiGet, asExample, type Envelope } from '@/api/client';
import type { components, paths } from '@/api/types';
import { amountText } from '../lendingInsights';
import { revenueReasonText } from '../lendingRevenueText';
import { UsdFigure, useAssetText } from '../lendingUsd';
import { usePoolReserves } from './usePoolReserves';

export type BadDebt =
  paths['/lending/pools/{pool}/bad-debt']['get']['responses'][200]['content']['application/json']['data'];
type Reserve = BadDebt['reserves'][number];
type Part = components['schemas']['LendingBadDebtPart'];

export const PARTS = [
  { key: 'moved_to_backstop', label: 'Moved to backstop' },
  { key: 'covered_by_backstop', label: 'Covered by backstop' },
  { key: 'socialised_to_suppliers', label: 'Written off to suppliers' },
  { key: 'repaid_by_backstop', label: 'Repaid by backstop' },
  { key: 'outstanding', label: 'Outstanding' },
] as const;

const nonZero = (v: string | null) => v != null && !/^-?0+$/.test(v);

/** Any bad-debt event, or a known non-zero outstanding: a pool of true zeros shows nothing. */
export function hasBadDebt(d: BadDebt): boolean {
  return d.reserves.some(
    (r) =>
      PARTS.some((p) => r[p.key].events > 0) || nonZero(r.outstanding.d_tokens),
  );
}

/** Why a reserve's identity can't be checked: the part's own reason when one names it. */
export function uncheckableText(r: Reserve): string {
  if (r.reconcile_reason === 'as_of_mismatch')
    return `can't be checked yet: ${revenueReasonText('as_of_mismatch')}`;
  const cause = PARTS.map((p) => r[p.key].d_tokens_reason).find(Boolean);
  return `can't be checked: ${revenueReasonText(cause ?? r.reconcile_reason)}`;
}

function PartCell({
  part,
  decimals,
}: {
  part: Part;
  decimals: number | null | undefined;
}) {
  const raw = part.underlying ?? part.d_tokens;
  return (
    <>
      <UsdFigure
        value={part.usd}
        reason={
          part.usd == null ? revenueReasonText(part.usd_reason) : undefined
        }
        showReason={part.usd == null}
      />
      {raw != null && (
        <div
          className="text-ink-muted font-mono text-xs"
          title={`${part.d_tokens ?? '?'} dToken base units`}
        >
          {amountText(raw, decimals)}
          {part.underlying == null ? ' dTokens' : ''}
        </div>
      )}
    </>
  );
}

/** Where moved debt went, each part against the amount moved, in dTokens. */
function Proportions({ r, label }: { r: Reserve; label: string }) {
  const moved = r.moved_to_backstop.d_tokens;
  if (!moved || !nonZero(moved)) return null;
  const items = PARTS.slice(1).map((p) => {
    const part = r[p.key].d_tokens ?? '0';
    const permille = (BigInt(part) * 1000n) / BigInt(moved);
    return {
      id: p.key,
      label: p.label,
      value: Number(part),
      display: `${permille / 10n}.${permille % 10n}%`,
    };
  });
  return (
    <HBarList
      items={items}
      max={Number(moved)}
      ariaLabel={`${label}: share of the debt moved to the backstop, by where it went`}
    />
  );
}

function BadDebtBody({ d, pool }: { d: BadDebt; pool: string }) {
  const assetText = useAssetText();
  const reserves = usePoolReserves(pool);
  const decimals = new Map(
    (reserves.data?.reserves ?? []).map((r) => [r.asset, r.decimals]),
  );
  const t = d.totals_usd;
  if (!hasBadDebt(d)) {
    const unknown = d.reserves.find((r) => r.outstanding.d_tokens == null);
    return (
      <p className="text-ink-muted text-sm">
        No bad-debt auction recorded for this pool; whether any is outstanding{' '}
        can&apos;t be checked:{' '}
        {revenueReasonText(unknown?.outstanding.d_tokens_reason)}.
      </p>
    );
  }
  return (
    <>
      <StatGrid cols={5} className="grid-cols-2">
        {PARTS.map((p) => {
          const tot = t[p.key];
          const left = tot.excluded
            .map(
              (e) => `${assetText(e.asset)} (${revenueReasonText(e.reason)})`,
            )
            .join(', ');
          return (
            <StatCell key={p.key}>
              <Stat
                label={p.label}
                value={
                  <UsdFigure
                    value={tot.usd}
                    lowerBound={tot.lower_bound}
                    reason={
                      tot.usd == null
                        ? left || 'no reserve is valued'
                        : left
                          ? `leaves out ${left}`
                          : undefined
                    }
                    showReason
                  />
                }
                sub="USD at each event"
              />
            </StatCell>
          );
        })}
      </StatGrid>
      <p className="text-ink-muted text-xs">
        Debt moved to the backstop is covered by it, written off to suppliers,
        repaid by it, or still outstanding. ≥ marks a lower bound.
      </p>
      <div className="overflow-x-auto">
        <Table>
          <THead>
            <tr>
              <Th>Asset</Th>
              {PARTS.map((p) => (
                <Th key={p.key} align="right">
                  {p.label}
                </Th>
              ))}
              <Th>Adds up</Th>
            </tr>
          </THead>
          <TBody>
            {d.reserves.map((r) => (
              <TR key={r.asset}>
                <Td title={r.asset}>{assetText(r.asset)}</Td>
                {PARTS.map((p) => (
                  <Td
                    key={p.key}
                    align="right"
                    className="tabular-nums"
                    data-testid={`bad-debt-${p.key}-${r.asset}`}
                  >
                    <PartCell
                      part={r[p.key]}
                      decimals={decimals.get(r.asset)}
                    />
                  </Td>
                ))}
                <Td
                  className="text-ink-muted text-xs"
                  data-testid={`bad-debt-check-${r.asset}`}
                >
                  {r.reconciled === true
                    ? 'yes'
                    : r.reconciled === false
                      ? r.residual_d_tokens != null
                        ? `no: ${amountText(r.residual_d_tokens, decimals.get(r.asset))} dTokens unaccounted for`
                        : 'no'
                      : uncheckableText(r)}
                </Td>
              </TR>
            ))}
          </TBody>
        </Table>
      </div>
      {d.reserves
        .filter((r) => r.reconciled === true)
        .map((r) => (
          <div key={r.asset} className="space-y-1">
            <h3 className="text-ink-body text-xs font-medium">
              Where {assetText(r.asset)} bad debt went
            </h3>
            <Proportions r={r} label={assetText(r.asset)} />
          </div>
        ))}
      {d.zero_bid_fills + d.unrecorded_bid_fills > 0 && (
        <p className="text-ink-muted text-xs">
          {d.zero_bid_fills > 0 &&
            `${d.zero_bid_fills} bad-debt fill${d.zero_bid_fills === 1 ? '' : 's'} had an empty bid and covered nothing. `}
          {d.unrecorded_bid_fills > 0 &&
            `${d.unrecorded_bid_fills} fill${d.unrecorded_bid_fills === 1 ? '' : 's'} did not state a bid (${revenueReasonText('bid_not_recorded')}).`}
        </p>
      )}
    </>
  );
}

export function BadDebtPanel({ pool }: { pool: string }) {
  const q = useQuery({
    queryKey: ['/v1/lending/pools/{pool}/bad-debt', pool],
    retry: false,
    staleTime: 300_000,
    queryFn: async () =>
      (
        await apiGet<Envelope<BadDebt>>(
          `/v1/lending/pools/${encodeURIComponent(pool)}/bad-debt`,
          {},
        )
      ).data,
  });
  const d = q.data;
  const unknownOutstanding = d?.reserves.some(
    (r) => r.outstanding.d_tokens == null,
  );
  // Loading, a non-pool (404) and a pool of true zeros render nothing.
  if (q.isLoading) return null;
  if (q.error instanceof Error && q.error.message.startsWith('404'))
    return null;
  if (d && !hasBadDebt(d) && !unknownOutstanding) return null;
  return (
    <Panel
      headingLevel={2}
      title="Bad debt"
      hint="Debt left after a liquidation that the borrower's collateral could not cover, and where it went. Valued in USD at the time of each event."
      source={asExample(`/v1/lending/pools/${pool}/bad-debt`, {})}
      bodyClassName="space-y-3"
    >
      {q.isError && (
        <p role="alert" className="text-ink-muted text-sm">
          Bad debt is unavailable right now.
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
      {d && <BadDebtBody d={d} pool={pool} />}
    </Panel>
  );
}
