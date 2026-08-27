'use client';

import Link from 'next/link';

import { Panel } from '@/components/reveal';
import { asExample } from '@/api/client';
import { useHistory, type TradeRow } from '@/api/hooks';
import { useObservationsFollow } from '@/lib/live/hooks';
import { formatRelative } from '@/lib/format';

const DEFAULT_QUOTE = 'native';
const HISTORY_LIMIT = 100;

/**
 * HistoryTabPanel — backs the "History" tab on /assets/[slug].
 *
 * Fetches recent trades from /v1/history with
 * `base=<asset_id>&quote=native` (XLM) — the most active on-chain
 * pair for any classic asset on Stellar. The asset_id comes from
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
  const history = useHistory(assetID, DEFAULT_QUOTE, HISTORY_LIMIT);
  // Live tape (RT-2): refresh the instant this pair trades, event-driven off
  // the observations stream (one connection — single pair, cap-safe).
  useObservationsFollow(assetID, DEFAULT_QUOTE, [
    '/v1/history',
    assetID,
    DEFAULT_QUOTE,
  ]);

  if (history.isError) {
    return (
      <Panel
        title="Recent trades"
        source={asExample('/v1/history', {
          base: assetID,
          quote: DEFAULT_QUOTE,
          limit: HISTORY_LIMIT,
        })}
        bodyClassName="text-sm text-down-strong"
      >
        Failed to load history.
      </Panel>
    );
  }

  if (history.isLoading) {
    return (
      <Panel
        title="Recent trades"
        source={asExample('/v1/history', {
          base: assetID,
          quote: DEFAULT_QUOTE,
          limit: HISTORY_LIMIT,
        })}
        bodyClassName="text-sm text-ink-muted"
      >
        Loading…
      </Panel>
    );
  }

  const rows = history.data ?? [];

  if (rows.length === 0) {
    return (
      <Panel
        title="Recent trades"
        source={asExample('/v1/history', {
          base: assetID,
          quote: DEFAULT_QUOTE,
          limit: HISTORY_LIMIT,
        })}
        bodyClassName="text-sm text-ink-muted"
      >
        No trades observed against XLM in the recent window. Try the Markets tab
        to see other quote pairs that have traded.
      </Panel>
    );
  }

  return (
    <Panel
      title={`Recent trades — last ${rows.length}`}
      source={asExample('/v1/history', {
        base: assetID,
        quote: DEFAULT_QUOTE,
        limit: HISTORY_LIMIT,
      })}
      bodyClassName="overflow-x-auto"
    >
      <table className="w-full min-w-[640px] text-sm">
        <thead className="text-ink-muted text-left text-xs tracking-wider uppercase">
          <tr className="border-line border-b">
            <th className="py-2 pr-3 font-medium">When</th>
            <th className="py-2 pr-3 font-medium">Source</th>
            <th className="py-2 pr-3 font-medium">Ledger</th>
            <th className="py-2 pr-3 text-right font-medium">Base amount</th>
            <th className="py-2 pr-3 text-right font-medium">Quote amount</th>
            <th className="py-2 pr-3 text-right font-medium">Price</th>
          </tr>
        </thead>
        <tbody>
          {rows.map((r) => (
            <tr
              key={`${r.tx_hash}-${r.op_index}`}
              className="border-line-subtle border-b last:border-0"
            >
              <td className="text-ink-body py-2 pr-3 font-mono text-xs">
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
              </td>
              <td className="py-2 pr-3">
                <Link
                  href={`/sources/${r.source}`}
                  className="bg-surface-subtle text-ink-body hover:text-brand-600 rounded-sm px-1.5 py-0.5 font-mono text-[11px]"
                >
                  {r.source}
                </Link>
              </td>
              <td className="text-ink-muted py-2 pr-3 font-mono text-xs">
                {r.ledger}
              </td>
              <td className="py-2 pr-3 text-right font-mono text-xs">
                {formatStroopAmount(r.base_amount, r.base_decimals ?? decimals)}
              </td>
              <td className="py-2 pr-3 text-right font-mono text-xs">
                {/* quote leg is always DEFAULT_QUOTE ('native' XLM, fixed 7
                    decimals) — never the base asset's own `decimals` prop. */}
                {formatStroopAmount(r.quote_amount, r.quote_decimals ?? 7)}
              </td>
              <td className="py-2 pr-3 text-right font-mono text-xs">
                {r.price ?? deriveAvgPrice(r.base_amount, r.quote_amount)}
              </td>
            </tr>
          ))}
        </tbody>
      </table>
    </Panel>
  );
}

// Format a stroop string (10^7 scale) into a human-readable
// fractional. Bigger amounts use compact notation (k/M/B); small
// amounts show up to 4 decimals. Strings throughout per ADR-0003 —
// this is a display-time conversion only, never used for further
// arithmetic.
function formatStroopAmount(s: string, decimals = 7): string {
  const n = Number(s);
  if (!Number.isFinite(n)) return s;
  const v = n / 10 ** decimals;
  if (Math.abs(v) >= 1_000_000) return `${(v / 1_000_000).toFixed(2)}M`;
  if (Math.abs(v) >= 1_000) return `${(v / 1_000).toFixed(2)}k`;
  if (Math.abs(v) >= 1) return v.toFixed(2);
  return v.toFixed(4);
}

function deriveAvgPrice(base: string, quote: string): string {
  const b = Number(base);
  const q = Number(quote);
  if (!b || !q || !Number.isFinite(b) || !Number.isFinite(q)) return '—';
  return (q / b).toFixed(7);
}

export type { TradeRow };
