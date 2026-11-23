'use client';

import { useState } from 'react';
import { useQuery } from '@tanstack/react-query';

import { Panel } from '@/components/reveal';
import { AssetLink } from '@/components/AssetLink';
import { InlineMeter } from '@/components/charts/Bars';
import {
  Button,
  Segmented,
  TBody,
  TR,
  Table,
  Td,
  Th,
  THead,
} from '@/components/ui';
import { apiGet, asExample } from '@/api/client';
import type { paths } from '@/api/types';
import { formatBaseUnits, formatCompactUnits } from '@/lib/format';
import { reasonText, UsdFigure, useAssetText } from '../lendingUsd';
import { AccountLink } from './AuctionsPanel';

type Resp =
  paths['/lending/pools/{pool}/top-accounts']['get']['responses'][200]['content']['application/json'];
type Data = Resp['data'];
type Side = Data['side'];
const LIMIT = 20;

const EXCLUSION_TEXT: Record<string, string> = {
  rate_unread: "the reserve's rate could not be read",
  positions_unreadable: 'some positions could not be read',
  positions_truncated: 'too many positions to rank in one read',
  positions_overcounted:
    "the positions read add up to more than the reserve's total",
  reserve_list_unreadable: "the pool's reserve list could not be read",
};

const ARCHIVED_NOTE =
  'Positions include archived entries: they keep their value and can be restored.';

const RANK_FLOOR_TITLE =
  'Not every position in this reserve was read, and an unread one could be larger, so this rank may be lower.';

function sharePercent(share: string): string {
  const pct = Number(share) * 100;
  return pct < 0.01 ? 'under 0.01%' : `${pct.toFixed(2)}%`;
}

/** Everything that makes the ranking partial: left-out positions, then each reserve's unattributed share. */
function partialLine(d: Data, assetText: (a: string) => string): string {
  const parts = d.excluded.map((e) => {
    const why = EXCLUSION_TEXT[e.reason] ?? reasonText(e.reason);
    return e.asset ? `${assetText(e.asset)} (${why})` : why;
  });
  for (const u of d.unattributed) {
    parts.push(
      `${assetText(u.asset)} (${sharePercent(u.share_of_reserve)} of this reserve's ${u.side} is not attributed to a read position)`,
    );
  }
  return [...new Set(parts)].join(', ');
}

function amountCell(raw: string, decimals: number | null) {
  return decimals == null
    ? `${raw} base units`
    : formatCompactUnits(raw, decimals);
}

/** 503 is "the position index is not wired"; 404 is "not a lending pool we read". */
function isUnsupported(err: unknown): boolean {
  return err instanceof Error && /^(503|404)\b/.test(err.message);
}

export function TopAccountsPanel({ pool }: { pool: string }) {
  const [side, setSide] = useState<Side>('supply');
  const assetText = useAssetText();
  const q = useQuery({
    queryKey: ['/v1/lending/pools/{pool}/top-accounts', pool, side],
    retry: false,
    staleTime: 60_000,
    queryFn: async () => {
      const env = await apiGet<Resp>(
        `/v1/lending/pools/${encodeURIComponent(pool)}/top-accounts`,
        { side, limit: LIMIT },
      );
      return env.data;
    },
  });
  const d = q.data;
  const verb = side === 'supply' ? 'supplying' : 'borrowing';
  const unranked = d != null && d.rows.length === 0 && d.lower_bound;
  const partial =
    d != null && (d.excluded.length > 0 || d.unattributed.length > 0);

  return (
    <Panel
      headingLevel={2}
      title="Top accounts"
      hint="The pool's largest positions on one side, valued in USD at the reserve price. A position in a reserve with no reliable price is left out of the ranking and named."
      source={asExample(`/v1/lending/pools/${pool}/top-accounts`, {
        side,
      })}
      bodyClassName="space-y-3"
    >
      <Segmented
        ariaLabel="Side"
        value={side}
        onChange={(v) => setSide(v as Side)}
        options={[
          { value: 'supply', label: 'Supply' },
          { value: 'borrow', label: 'Borrow' },
        ]}
      />
      {q.isLoading && (
        <p className="text-ink-muted text-sm">Loading top accounts…</p>
      )}
      {q.isError &&
        (isUnsupported(q.error) ? (
          <p className="text-ink-muted text-sm">
            Top accounts are not available for this pool.
          </p>
        ) : (
          <p className="text-ink-muted text-sm">
            Top accounts are unavailable right now.
            <Button
              variant="secondary"
              size="sm"
              className="ml-3"
              onClick={() => void q.refetch()}
            >
              Retry
            </Button>
          </p>
        ))}
      {d && unranked && (
        <div className="text-sm">
          <p className="text-ink-body">No rankable positions.</p>
          {partial && (
            <p className="text-ink-muted">Why: {partialLine(d, assetText)}.</p>
          )}
        </div>
      )}
      {d && !unranked && (
        <p className="text-ink-body text-sm">
          <span className="text-ink font-mono tabular-nums">
            {d.lower_bound ? 'at least ' : ''}
            {d.accounts.toLocaleString('en-US')}
          </span>{' '}
          account{d.accounts === 1 ? '' : 's'} {verb}
          {d.lower_bound && partial && (
            <span className="text-ink-muted">
              {' '}
              · partial: {partialLine(d, assetText)}
            </span>
          )}
        </p>
      )}
      {d && d.rows.length === 0 && !unranked && (
        <p className="text-ink-muted text-sm">No positions.</p>
      )}
      {d && d.rows.length > 0 && (
        <div className="overflow-x-auto">
          <Table>
            <THead>
              <tr>
                <Th align="right">#</Th>
                <Th>Account</Th>
                <Th>Asset</Th>
                <Th align="right" className="hidden sm:table-cell">
                  Amount
                </Th>
                <Th align="right">USD</Th>
                <Th className="hidden md:table-cell">Share of reserve</Th>
              </tr>
            </THead>
            <TBody>
              {d.rows.map((r) => {
                const raw =
                  r.share_of_reserve == null
                    ? null
                    : Number(r.share_of_reserve);
                // Matches the meter's clamp so the text never reads above 100%.
                const share =
                  raw == null ? null : Math.min(1, Math.max(0, raw));
                return (
                  <TR key={`${r.account}-${r.asset}`}>
                    <Td align="right" className="font-mono tabular-nums">
                      {r.rank_lower_bound && (
                        <span
                          className="text-ink-faint"
                          title={RANK_FLOOR_TITLE}
                        >
                          ≥
                        </span>
                      )}
                      {r.rank}
                    </Td>
                    <Td className="font-mono whitespace-nowrap">
                      <AccountLink id={r.account} />
                    </Td>
                    <Td className="whitespace-nowrap" title={r.asset}>
                      <AssetLink canonical={r.asset} />
                    </Td>
                    <Td
                      align="right"
                      className="hidden font-mono tabular-nums sm:table-cell"
                      title={
                        r.decimals == null
                          ? undefined
                          : formatBaseUnits(r.amount, r.decimals, r.decimals)
                      }
                    >
                      {amountCell(r.amount, r.decimals)}
                    </Td>
                    <Td align="right" className="font-mono tabular-nums">
                      <UsdFigure
                        value={r.value_usd}
                        reason={
                          r.value_usd == null
                            ? reasonText(r.usd_reason)
                            : undefined
                        }
                      />
                    </Td>
                    <Td className="hidden md:table-cell">
                      {share == null || !Number.isFinite(share) ? (
                        <span className="text-ink-faint">—</span>
                      ) : (
                        <span className="flex items-center gap-2 whitespace-nowrap">
                          <InlineMeter
                            frac={share}
                            label={`${(share * 100).toFixed(2)}% of the reserve's ${side}`}
                            className="w-20"
                          />
                          <span className="font-mono text-xs tabular-nums">
                            {(share * 100).toFixed(2)}%
                          </span>
                        </span>
                      )}
                    </Td>
                  </TR>
                );
              })}
            </TBody>
          </Table>
        </div>
      )}
      {d && !/archived/i.test(d.note) && (
        <p className="text-ink-faint text-xs">{ARCHIVED_NOTE}</p>
      )}
    </Panel>
  );
}
