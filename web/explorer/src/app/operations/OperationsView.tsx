'use client';

import Link from 'next/link';
import { useEffect, useRef, useState } from 'react';
import { useSearchParams } from 'next/navigation';
import { useQuery, useQueryClient } from '@tanstack/react-query';

import { Panel } from '@/components/reveal';
import { Callout, TxStatusBadge } from '@/components/ui';
import { OperationMixPanel, ThroughputPanel } from '@/components/NetworkInsight';
import { apiGet, asExample } from '@/api/client';
import { cn } from '@/lib/cn';
import { formatCompact } from '@/lib/format';
import { isFrameStale, LEDGER_LIVE_STALE_MS, useLedgerStream, useLiveClock } from '@/lib/live/hooks';
import {
  type Envelope,
  type TxOperation,
  formatTimestamp,
  renderOpFieldValue,
} from '../explorer-shared';
import type { paths } from '@/api/types';

// GET /v1/operations response body + row shapes from the generated
// OpenAPI contract (src/api/types.ts, `make web-generate-api`). Rows
// are the spec Operation schema (aliased TxOperation in explorer-shared).
type OperationsResp = NonNullable<
  paths['/operations']['get']['responses'][200]['content']['application/json']['data']
>;

type OpView = TxOperation;

const PAGE_SIZE = 50;

/** Minimum gap between live-follow refetches (RT-2). */
const LIVE_REFETCH_MIN_MS = 10_000;

// A one-line summary of the decoded op body — the fields that matter
// most per type, best-effort. Falls back to nothing (the type badge
// already conveys the gist).
function summarize(op: OpView): string {
  const f = op.fields;
  if (!f) return '';
  const pick = (k: string) => (f[k] != null ? String(f[k]) : '');
  // Amount keys are raw stroop integers (ADR-0003) — scale them to XLM
  // via the shared keyed renderer (exact BigInt divide), never String().
  const pickAmount = (k: string) =>
    f[k] != null ? renderOpFieldValue(k, f[k]) : '';
  const amount = pickAmount('amount') || pickAmount('starting_balance') || pickAmount('limit');
  const asset = pick('asset') || pick('selling') || pick('send_asset');
  const dest = pick('destination') || pick('to') || pick('trustor');
  const parts: string[] = [];
  if (amount) parts.push(asset ? `${amount} ${asset}` : amount);
  if (dest) parts.push(`→ ${dest.length > 12 ? `${dest.slice(0, 4)}…${dest.slice(-4)}` : dest}`);
  return parts.join(' ');
}

export function OperationsView() {
  const params = useSearchParams();
  const cursor = params.get('cursor') ?? '';

  const q = useQuery<OperationsResp>({
    queryKey: ['/v1/operations', cursor],
    queryFn: async () => {
      const args: Record<string, string | number> = { limit: PAGE_SIZE };
      if (cursor) args.cursor = cursor;
      const env = await apiGet<Envelope<OperationsResp>>('/v1/operations', args);
      return env.data;
    },
    staleTime: 20_000,
    retry: false,
  });

  const ops = q.data?.operations ?? [];

  // Live follow (RT-2, same shape as /ledgers): while page 1 is on
  // screen, a ledger close newer than the newest row triggers a
  // refetch, and rows from ledgers that weren't there before animate
  // in. Deep-paged readers are never shifted. THROTTLED: the newest
  // served operation structurally trails the ingest-tip stream by a
  // beat, so the newer-than guard alone can never settle — unthrottled
  // this refetched every close (~12 req/min per viewer, observed live
  // 2026-08-09).
  const frame = useLedgerStream();
  const clock = useLiveClock();
  const queryClient = useQueryClient();
  const lastRefetchRef = useRef(0);
  const streamLatest = frame?.data.latest_ledger;
  const newestShown = ops[0]?.ledger;
  useEffect(() => {
    if (cursor || streamLatest == null) return;
    // Wait for the initial page before following — invalidating while
    // the first fetch is in flight just cancels and restarts it.
    if (newestShown == null || streamLatest <= newestShown) return;
    const now = Date.now();
    if (now - lastRefetchRef.current < LIVE_REFETCH_MIN_MS) return;
    lastRefetchRef.current = now;
    void queryClient.invalidateQueries({ queryKey: ['/v1/operations', ''] });
  }, [cursor, streamLatest, newestShown, queryClient]);
  const following =
    !cursor && frame != null && !isFrameStale(clock, frame.receivedAt, LEDGER_LIVE_STALE_MS);
  const [prevNewest, setPrevNewest] = useState<number | null>(null);
  const [flashAbove, setFlashAbove] = useState<number | null>(null);
  if (newestShown != null && newestShown !== prevNewest) {
    setPrevNewest(newestShown);
    setFlashAbove(!cursor ? prevNewest : null);
  }

  return (
    <div className="mx-auto max-w-7xl space-y-6 px-6 py-8">
      <header className="space-y-1">
        <p className="text-xs uppercase tracking-wider text-ink-muted">Explorer</p>
        <h1 className="text-2xl font-semibold tracking-tight text-ink">Operations</h1>
        <p className="max-w-2xl text-sm text-ink-muted">
          Every operation on the network, newest first, decoded straight from
          the certified lake. Click a hash for the full transaction.
        </p>
      </header>

      {/* S-005: the chips row becomes the shared ranked-bars mix +
          the daily ops series — same components /network uses. Only on
          page 1; deep-paging visitors came for the rows. */}
      {!cursor && (
        <div className="grid gap-6 lg:grid-cols-2">
          <OperationMixPanel linkRows={false} />
          <ThroughputPanel defaultMetric="ops" />
        </div>
      )}

      {q.data?.coverage_note && (
        // Honest-degrade banner: the parent-transaction outcome read failed,
        // so rows without a status below are of UNKNOWN outcome (possibly a
        // FAILED transaction), not applied.
        <Callout tone="warn" title="Transaction outcomes partially unavailable">
          {q.data.coverage_note}
        </Callout>
      )}

      <Panel
        title={ops.length > 0 ? `Recent operations (${formatCompact(ops.length)})` : 'Recent operations'}
        hint={following ? 'live' : undefined}
        source={asExample('/v1/operations', { limit: PAGE_SIZE })}
        bodyClassName="-mx-4"
      >
        {q.isError && (
          <p className="px-4 text-sm text-down-strong">
            Failed to load operations:{' '}
            {q.error instanceof Error ? q.error.message : 'unknown error'}
          </p>
        )}
        {(q.isLoading || q.data == null) && !q.isError && (
          <p className="px-4 text-sm text-ink-muted">Loading…</p>
        )}
        {q.data && ops.length === 0 && (
          <p className="px-4 text-sm text-ink-muted">No operations in this page.</p>
        )}
        {ops.length > 0 && (
          <div className="overflow-x-auto">
            <table className="min-w-full divide-y divide-line text-sm">
              <thead>
                <tr className="text-left text-[11px] uppercase tracking-wider text-ink-muted">
                  <th scope="col" className="px-4 py-2">Type</th>
                  <th scope="col" className="px-4 py-2">Status</th>
                  <th scope="col" className="px-4 py-2">Detail</th>
                  <th scope="col" className="px-4 py-2">Source</th>
                  <th scope="col" className="px-4 py-2 text-right">Ledger</th>
                  <th scope="col" className="px-4 py-2">Tx</th>
                  <th scope="col" className="px-4 py-2">When</th>
                </tr>
              </thead>
              <tbody className="divide-y divide-line-subtle">
                {ops.map((op) => (
                  <tr
                    key={`${op.tx_hash}:${op.op_index}`}
                    className={cn(
                      'hover:bg-surface-muted',
                      flashAbove != null && (op.ledger ?? 0) > flashAbove && 'live-tick',
                    )}
                  >
                    <td className="px-4 py-3">
                      <Link
                        href={`/operation?tx=${op.tx_hash}&i=${op.op_index}`}
                        className="rounded-sm bg-surface-muted px-1.5 py-0.5 text-[11px] text-ink-body hover:text-brand-600"
                        title="Operation detail"
                      >
                        <code>{op.type}</code>
                      </Link>
                    </td>
                    <td className="px-4 py-3">
                      {/* Failed ops stay listed — marked, never hidden. */}
                      <TxStatusBadge
                        successful={op.transaction_successful}
                        result={op.transaction_result}
                        code={op.result_code}
                      />
                    </td>
                    <td className="px-4 py-3 font-mono text-[11px] text-ink-muted">
                      {summarize(op) || <span className="text-ink-faint">—</span>}
                    </td>
                    <td className="px-4 py-3">
                      {op.source_account ? (
                        <Link
                          href={`/accounts/${encodeURIComponent(op.source_account)}/`}
                          className="font-mono text-xs text-ink-body hover:text-brand-600"
                          title={op.source_account}
                        >
                          {op.source_account.slice(0, 6)}…{op.source_account.slice(-4)}
                        </Link>
                      ) : (
                        <span className="text-ink-faint">—</span>
                      )}
                    </td>
                    <td className="px-4 py-3 text-right">
                      <Link
                        href={`/ledgers/${op.ledger}/`}
                        className="font-mono tabular-nums text-xs text-ink-body hover:text-brand-600"
                      >
                        {(op.ledger ?? 0).toLocaleString('en-US')}
                      </Link>
                    </td>
                    <td className="px-4 py-3">
                      <Link
                        href={`/transactions/${op.tx_hash}/`}
                        className="font-mono text-xs text-brand-600 hover:underline"
                        title={op.tx_hash}
                      >
                        {(op.tx_hash ?? '').slice(0, 8)}…
                      </Link>
                    </td>
                    <td className="px-4 py-3 font-mono text-[11px] text-ink-muted">
                      {formatTimestamp(op.close_time)}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </Panel>

      {q.data?.next_cursor && (
        <div className="flex justify-center">
          <Link
            href={`/operations?cursor=${encodeURIComponent(q.data.next_cursor)}`}
            className="rounded-md border border-line px-4 py-2 text-sm text-ink-body hover:border-brand-500 hover:text-brand-600"
          >
            Older operations →
          </Link>
        </div>
      )}
      {cursor && (
        <div className="flex justify-center">
          <Link href="/operations" className="text-xs text-brand-600 hover:underline">
            ← Back to latest
          </Link>
        </div>
      )}
    </div>
  );
}
