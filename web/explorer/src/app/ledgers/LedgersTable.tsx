'use client';

import { useState } from 'react';
import Link from 'next/link';
import { useQuery, keepPreviousData } from '@tanstack/react-query';

import { Panel } from '@/components/reveal';
import { apiGet, asExample } from '@/api/client';
import { formatCompact } from '@/lib/format';
import {
  type Envelope,
  type LedgersPage,
  formatTimestamp,
  relativeAge,
} from '../explorer-shared';

const PAGE_SIZE = 50;

/**
 * Live ledgers table backed by /v1/ledgers?limit=50.
 *
 * "Load older" walks backwards via the envelope's `next_before`
 * cursor (passed as ?before=). Each page replaces the view; the
 * cursor stack keeps the rows in descending-sequence order.
 */
export function LedgersTable() {
  // `before` is the cursor for the currently-displayed page. undefined
  // = the live tip. Each "Load older" pushes the page's next_before.
  const [before, setBefore] = useState<number | undefined>(undefined);

  const { data, isLoading, isError, error, isFetching } = useQuery<LedgersPage>(
    {
      queryKey: ['/v1/ledgers', PAGE_SIZE, before ?? 'tip'],
      queryFn: async () => {
        const env = await apiGet<Envelope<LedgersPage>>('/v1/ledgers', {
          limit: PAGE_SIZE,
          ...(before !== undefined ? { before } : {}),
        });
        return env.data;
      },
      placeholderData: keepPreviousData,
      staleTime: 10_000,
    },
  );

  const source = asExample('/v1/ledgers', {
    limit: PAGE_SIZE,
    ...(before !== undefined ? { before } : {}),
  });

  if (isError) {
    return (
      <Panel
        title="Ledgers"
        source={source}
        bodyClassName="text-sm text-down-strong"
      >
        Failed to load ledgers:{' '}
        {error instanceof Error ? error.message : 'unknown error'}
      </Panel>
    );
  }
  if (isLoading || !data) {
    return (
      <Panel
        title="Ledgers"
        source={source}
        bodyClassName="text-sm text-ink-muted"
      >
        Loading…
      </Panel>
    );
  }
  if (data.ledgers.length === 0) {
    return (
      <Panel
        title="Ledgers"
        source={source}
        bodyClassName="text-sm text-ink-muted"
      >
        No ledgers returned.
      </Panel>
    );
  }

  const newest = data.ledgers[0]?.sequence;
  const oldest = data.ledgers[data.ledgers.length - 1]?.sequence;

  return (
    <Panel
      title={`${data.ledgers.length} ledgers`}
      hint={
        newest != null && oldest != null
          ? `#${oldest.toLocaleString()} → #${newest.toLocaleString()}`
          : undefined
      }
      source={source}
      bodyClassName="-mx-4"
    >
      <div className="overflow-x-auto">
        <table className="min-w-full divide-y divide-line text-sm">
          <thead>
            <tr className="text-left text-[11px] uppercase tracking-wider text-ink-muted">
              <Th>Sequence</Th>
              <Th>Close time</Th>
              <Th align="right">Txs</Th>
              <Th align="right">Ops</Th>
              <Th align="right">Soroban events</Th>
            </tr>
          </thead>
          <tbody className="divide-y divide-line-subtle">
            {data.ledgers.map((l) => (
              <tr
                key={l.sequence}
                className="hover:bg-surface-muted"
              >
                <Td>
                  <Link
                    href={`/ledger?seq=${l.sequence}`}
                    className="font-mono font-medium text-ink-body hover:text-brand-600"
                  >
                    #{l.sequence.toLocaleString()}
                  </Link>
                </Td>
                <Td>
                  <span
                    className="font-mono text-xs text-ink-muted"
                    title={formatTimestamp(l.close_time)}
                  >
                    {relativeAge(l.close_time)}
                  </span>
                </Td>
                <Td align="right">
                  <span className="font-mono tabular-nums">
                    {formatCompact(l.tx_count)}
                  </span>
                </Td>
                <Td align="right">
                  <span className="font-mono tabular-nums text-ink-body">
                    {formatCompact(l.op_count)}
                  </span>
                </Td>
                <Td align="right">
                  <span className="font-mono tabular-nums text-ink-body">
                    {l.soroban_event_count > 0
                      ? formatCompact(l.soroban_event_count)
                      : '—'}
                  </span>
                </Td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>

      <div className="flex items-center justify-between px-4 pb-1 pt-4 text-xs">
        <button
          type="button"
          onClick={() => setBefore(undefined)}
          disabled={before === undefined || isFetching}
          className="rounded-md border border-line px-3 py-1.5 text-ink-body hover:border-brand-500 hover:text-brand-600 disabled:cursor-not-allowed disabled:opacity-40"
        >
          ← Newest
        </button>
        <span className="font-mono text-[11px] text-ink-faint">
          {isFetching ? 'Loading…' : ''}
        </span>
        <button
          type="button"
          onClick={() => {
            if (data.next_before != null) setBefore(data.next_before);
          }}
          disabled={data.next_before == null || isFetching}
          className="rounded-md border border-line px-3 py-1.5 text-ink-body hover:border-brand-500 hover:text-brand-600 disabled:cursor-not-allowed disabled:opacity-40"
        >
          Load older →
        </button>
      </div>
    </Panel>
  );
}

function Th({
  children,
  align,
}: {
  children: React.ReactNode;
  align?: 'left' | 'right';
}) {
  return (
    <th
      className={`px-4 py-2 ${align === 'right' ? 'text-right' : 'text-left'}`}
      scope="col"
    >
      {children}
    </th>
  );
}

function Td({
  children,
  align,
}: {
  children: React.ReactNode;
  align?: 'left' | 'right';
}) {
  return (
    <td
      className={`px-4 py-3 ${align === 'right' ? 'text-right' : 'text-left'}`}
    >
      {children}
    </td>
  );
}
