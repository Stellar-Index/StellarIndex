'use client';

import Link from 'next/link';
import { hrefFor } from '@/lib/hrefFor';

import { Panel } from '@/components/reveal';
import {
  Badge,
  Callout,
  Skeleton,
  Table,
  THead,
  TBody,
  Th,
  Td,
  LinkRow,
} from '@/components/ui';
import { PANEL_LABEL_CLASS } from '@/components/ui/Page';
import { asExample } from '@/api/client';
import { USDC_ASSET_ID } from '@/lib/networks';
import { useHistory, type TradeRow } from '@/api/hooks';
import { useObservationsFollow } from '@/lib/live/hooks';
import { formatRelative, formatUnitsReadable } from '@/lib/format';
import { TradeScatter } from '../../markets/[pair]/TradeScatter';
import { HBarList, type HBarItem } from '@/components/charts/Bars';

const HISTORY_LIMIT = 100;

/**
 * HistoryTabPanel — backs the "History" tab on /assets/[slug].
 *
 * Fetches recent trades from /v1/history with
 * `base=<asset_id>&quote=native` (XLM) — the most active on-chain
 * pair for any classic asset on Stellar; XLM itself is quoted in USDC. The asset_id comes from
 * the parent server component's /v1/coins/{slug} lookup.
 *
 * Fiat-quoted trades (e.g. against `fiat:USD`) don't surface here:
 * /v1/history serves raw on-chain trades only; aggregator-derived
 * pairs ship via /v1/vwap and /v1/twap.
 */
export function HistoryTabPanel({
  assetID,
  decimals = 7,
}: {
  assetID: string;
  decimals?: number;
}) {
  const quote = historyQuote(assetID);
  const history = useHistory(assetID, quote, HISTORY_LIMIT);
  // Live tape (RT-2): refresh the instant this pair trades, event-driven off
  // the observations stream (one connection — single pair, cap-safe).
  useObservationsFollow(assetID, quote, ['/v1/history', assetID, quote]);

  if (history.isError) {
    return (
      <Panel
        headingLevel={2}
        title="Recent trades"
        source={asExample('/v1/history', {
          base: assetID,
          quote,
          limit: HISTORY_LIMIT,
        })}
      >
        <Callout tone="bad">Failed to load history.</Callout>
      </Panel>
    );
  }

  if (history.isLoading) {
    return (
      <Panel
        headingLevel={2}
        title="Recent trades"
        source={asExample('/v1/history', {
          base: assetID,
          quote,
          limit: HISTORY_LIMIT,
        })}
      >
        <div className="space-y-3" aria-busy="true">
          <span className="sr-only">Loading trades</span>
          <Skeleton className="h-4 w-3/5" aria-hidden />
          <Skeleton className="h-4 w-full" aria-hidden />
          <Skeleton className="h-4 w-4/5" aria-hidden />
        </div>
      </Panel>
    );
  }

  const rows = history.data ?? [];

  if (rows.length === 0) {
    return (
      <Panel
        headingLevel={2}
        title="Recent trades"
        source={asExample('/v1/history', {
          base: assetID,
          quote,
          limit: HISTORY_LIMIT,
        })}
        bodyClassName="text-sm text-ink-muted"
      >
        No trades against {quote === 'native' ? 'XLM' : 'USDC'} in the recent
        window.
      </Panel>
    );
  }

  return (
    <Panel
      headingLevel={2}
      title={`Recent trades — last ${rows.length}`}
      source={asExample('/v1/history', {
        base: assetID,
        quote,
        limit: HISTORY_LIMIT,
      })}
      bodyClassName="-mx-4"
    >
      <div className="px-4">
        <TradeScatter
          trades={rows.map((r) => ({
            ...r,
            base_decimals: r.base_decimals ?? decimals,
          }))}
        />
        <TradeVenueBars rows={rows} />
      </div>
      <div className="overflow-x-auto">
        <Table className="min-w-[640px]">
          <THead>
            <tr>
              <Th>When</Th>
              <Th>Source</Th>
              <Th>Ledger</Th>
              <Th align="right">Base amount</Th>
              <Th align="right">Quote amount</Th>
              <Th align="right">Price</Th>
            </tr>
          </THead>
          <TBody>
            {rows.map((r) => (
              <LinkRow
                href={`/transactions/${r.tx_hash}/`}
                key={`${r.tx_hash}-${r.op_index}`}
              >
                <Td className="font-mono text-xs whitespace-nowrap">
                  {r.tx_hash ? (
                    <Link
                      href={`/transactions/${r.tx_hash}/`}
                      className="hover:text-brand-600 hover:underline"
                      title={`View transaction ${r.tx_hash}`}
                    >
                      {formatRelative(r.ts)}
                    </Link>
                  ) : (
                    formatRelative(r.ts)
                  )}
                </Td>
                <Td>
                  <Link
                    href={hrefFor.source(r.source)}
                    className="hover:underline"
                  >
                    <Badge tone="neutral" className="font-mono">
                      {r.source}
                    </Badge>
                  </Link>
                </Td>
                <Td className="text-ink-muted font-mono text-xs">{r.ledger}</Td>
                <Td align="right" className="font-mono text-xs">
                  {formatUnitsReadable(
                    r.base_amount,
                    r.base_decimals ?? decimals,
                  )}
                </Td>
                <Td align="right" className="font-mono text-xs">
                  {/* quote leg is XLM or classic USDC, both fixed 7 decimals —
                    never the base asset's own `decimals` prop. */}
                  {formatUnitsReadable(r.quote_amount, r.quote_decimals ?? 7)}
                </Td>
                <Td align="right" className="font-mono text-xs">
                  {r.price ?? '—'}
                </Td>
              </LinkRow>
            ))}
          </TBody>
        </Table>
      </div>
    </Panel>
  );
}

/** XLM against itself is not a market, so XLM's tape is its USDC pair. */
export function historyQuote(assetID: string): string {
  return assetID === 'native' ? USDC_ASSET_ID : 'native';
}

/** Trade count per venue over the rows shown, ranked. */
export function tradeVenueRows(rows: Pick<TradeRow, 'source'>[]): HBarItem[] {
  const counts = new Map<string, number>();
  for (const r of rows) counts.set(r.source, (counts.get(r.source) ?? 0) + 1);
  return [...counts]
    .map(([source, n]) => ({
      label: source,
      value: n,
      display: `${n} trade${n === 1 ? '' : 's'}`,
      annotation: `${Math.round((n / rows.length) * 100)}%`,
    }))
    .sort((a, b) => b.value - a.value);
}

function TradeVenueBars({ rows }: { rows: TradeRow[] }) {
  const items = tradeVenueRows(rows);
  // One venue compares nothing; the Source column already names it.
  if (items.length < 2) return null;
  return (
    <div className="pb-3">
      <h3 className={`${PANEL_LABEL_CLASS} mb-2`}>Trades by venue</h3>
      <HBarList items={items} ariaLabel="Trade count by venue" />
    </div>
  );
}

export type { TradeRow };
