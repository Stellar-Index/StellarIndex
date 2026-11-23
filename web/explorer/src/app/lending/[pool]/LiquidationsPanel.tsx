'use client';

import { useInfiniteQuery } from '@tanstack/react-query';

import { Panel } from '@/components/reveal';
import { HBarList } from '@/components/charts/Bars';
import { Button, TBody, TR, Table, Td, Th, THead } from '@/components/ui';
import { asExample } from '@/api/client';
import { formatDecimalAmount, formatRelative } from '@/lib/format';
import { amountText } from '../lendingInsights';
import {
  dailyDebtRepaid,
  fetchLiquidations,
  liquidationReasonText,
  type Liquidation,
  type LiquidationLeg,
  type LiquidationsPage,
} from '../lendingLiquidations';
import { UsdFigure, useAssetText } from '../lendingUsd';
import { AccountLink, TxLink } from './AuctionsPanel';
import { usePoolReserves } from './usePoolReserves';

const LIMIT = 25;
// Backstop tokens carry no decimals in the API; they use the 7-decimal Soroban scale.
const BACKSTOP_DECIMALS = 7;

const TOKEN_UNIT: Record<LiquidationLeg['kind'], string> = {
  b_token: ' bTokens',
  d_token: ' dTokens',
  underlying: '',
  backstop_token: ' backstop tokens',
  unknown: ' tokens',
};

function Leg({
  leg,
  decimals,
  assetText,
}: {
  leg: LiquidationLeg;
  decimals: number | null | undefined;
  assetText: (a: string) => string;
}) {
  const raw = leg.underlying ?? leg.amount;
  const unit = leg.underlying != null ? '' : TOKEN_UNIT[leg.kind];
  return (
    <div className="whitespace-nowrap">
      <span className="font-mono" title={`${leg.amount} base units`}>
        {amountText(
          raw,
          leg.kind === 'backstop_token' ? BACKSTOP_DECIMALS : decimals,
        )}
      </span>{' '}
      <span title={leg.asset}>
        {assetText(leg.asset)}
        {unit}
      </span>
      <span className="text-ink-muted"> · </span>
      <UsdFigure
        value={leg.usd}
        reason={leg.usd == null ? liquidationReasonText(leg.reason) : undefined}
      />
    </div>
  );
}

function Side({
  legs,
  total,
  lowerBound,
  decimalsOf,
  assetText,
}: {
  legs: LiquidationLeg[];
  total: string | null;
  lowerBound: boolean;
  decimalsOf: (a: string) => number | null | undefined;
  assetText: (a: string) => string;
}) {
  if (legs.length === 0) return <>—</>;
  return (
    <>
      {legs.map((l) => (
        <Leg
          key={`${l.asset}-${l.kind}`}
          leg={l}
          decimals={decimalsOf(l.asset)}
          assetText={assetText}
        />
      ))}
      {(legs.length > 1 || lowerBound) && (
        <div className="text-ink-body">
          Total{' '}
          <UsdFigure
            value={total}
            lowerBound={lowerBound}
            reason={
              lowerBound
                ? 'some of it has no USD value at the fill'
                : 'No part of it has a USD value at the fill.'
            }
          />
        </div>
      )}
    </>
  );
}

function DailyChart({ fills }: { fills: Liquidation[] }) {
  const days = dailyDebtRepaid(fills);
  if (days.length === 0) return null;
  const n = fills.length;
  return (
    <div className="space-y-2">
      <h3 className="text-ink-body text-xs font-medium">
        Debt repaid per day — the {n} fill{n === 1 ? '' : 's'} loaded
      </h3>
      <HBarList
        ariaLabel="Debt repaid in USD per day, over the fills loaded"
        items={days.map((d) => ({
          id: d.day,
          label: d.day,
          value: d.value,
          display: d.display,
          title: d.lowerBound
            ? `At least $${formatDecimalAmount(d.usd, 2)}: some debt repaid that day has no USD value`
            : `$${formatDecimalAmount(d.usd, 2)}`,
        }))}
      />
      {days.some((d) => d.lowerBound) && (
        <p className="text-ink-muted text-xs">
          ≥ marks a day where some debt repaid has no USD value at the fill.
        </p>
      )}
    </div>
  );
}

function LiquidationsList({ pool }: { pool: string }) {
  const assetText = useAssetText();
  const reserves = usePoolReserves(pool);
  const decimals = new Map(
    (reserves.data?.reserves ?? []).map((r) => [r.asset, r.decimals]),
  );
  const decimalsOf = (a: string) => decimals.get(a);
  const q = useInfiniteQuery<
    LiquidationsPage,
    Error,
    { pages: LiquidationsPage[] },
    unknown[],
    string
  >({
    queryKey: ['/v1/lending/pools/{pool}/liquidations', pool],
    retry: false,
    initialPageParam: '',
    queryFn: ({ pageParam }) => fetchLiquidations(pool, LIMIT, pageParam),
    getNextPageParam: (last) => last.next,
    staleTime: 60_000,
  });
  const rows = (q.data?.pages ?? []).flatMap((p) => p.fills);

  return (
    <>
      {q.isLoading && (
        <p className="text-ink-muted text-sm">Loading liquidations…</p>
      )}
      {q.isError && (
        <p className="text-ink-muted text-sm">
          Liquidations are unavailable right now.
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
      {q.data && rows.length === 0 && (
        <p className="text-ink-muted text-sm">
          No liquidation fill recorded for this pool.
        </p>
      )}
      {rows.length > 0 && <DailyChart fills={rows} />}
      {rows.length > 0 && (
        <div className="overflow-x-auto">
          <Table>
            <THead>
              <tr>
                <Th>When</Th>
                <Th className="hidden sm:table-cell">Liquidated</Th>
                <Th className="hidden sm:table-cell">Filler</Th>
                <Th>Collateral seized</Th>
                <Th>Debt repaid</Th>
                <Th align="right">Filler profit</Th>
              </tr>
            </THead>
            <TBody>
              {rows.map((f, i) => (
                <TR key={`${f.tx_hash}-${f.user}-${f.filler}-${i}`}>
                  <Td className="whitespace-nowrap" title={f.time}>
                    {formatRelative(f.time)}
                    <div className="text-ink-muted font-mono text-xs">
                      <TxLink hash={f.tx_hash} />
                    </div>
                  </Td>
                  <Td className="hidden font-mono sm:table-cell">
                    <AccountLink id={f.user} />
                    {f.auction_type === 1 && (
                      <div className="text-ink-muted font-sans text-xs">
                        bad debt
                      </div>
                    )}
                  </Td>
                  <Td className="hidden font-mono sm:table-cell">
                    <AccountLink id={f.filler} />
                  </Td>
                  {f.amounts_recorded ? (
                    <>
                      <Td className="text-xs" data-testid={`liq-lot-${i}`}>
                        <Side
                          legs={f.lot}
                          total={f.lot_usd}
                          lowerBound={f.lot_usd_lower_bound}
                          decimalsOf={decimalsOf}
                          assetText={assetText}
                        />
                      </Td>
                      <Td className="text-xs" data-testid={`liq-bid-${i}`}>
                        <Side
                          legs={f.bid}
                          total={f.bid_usd}
                          lowerBound={f.bid_usd_lower_bound}
                          decimalsOf={decimalsOf}
                          assetText={assetText}
                        />
                      </Td>
                    </>
                  ) : (
                    <Td
                      colSpan={2}
                      className="text-ink-muted text-xs"
                      title={liquidationReasonText(f.usd_reason)}
                    >
                      Amounts not recorded
                    </Td>
                  )}
                  <Td align="right" className="whitespace-nowrap">
                    <UsdFigure
                      value={f.filler_profit_usd}
                      reason={
                        f.amounts_recorded
                          ? 'Shown only when both sides are fully valued at the fill.'
                          : liquidationReasonText(f.usd_reason)
                      }
                    />
                  </Td>
                </TR>
              ))}
            </TBody>
          </Table>
        </div>
      )}
      {q.hasNextPage && (
        <Button
          variant="secondary"
          size="sm"
          disabled={q.isFetchingNextPage}
          onClick={() => void q.fetchNextPage()}
        >
          {q.isFetchingNextPage ? 'Loading…' : 'Load more'}
        </Button>
      )}
    </>
  );
}

export function LiquidationsPanel({ pool }: { pool: string }) {
  return (
    <Panel
      headingLevel={2}
      title="Liquidations"
      hint="Each auction fill: the collateral the filler received and the debt it repaid, valued in USD at the time of the fill, not today. Newest first."
      source={asExample(`/v1/lending/pools/${pool}/liquidations`, {})}
      bodyClassName="space-y-3"
    >
      <LiquidationsList pool={pool} />
    </Panel>
  );
}
