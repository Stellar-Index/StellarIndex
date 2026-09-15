'use client';

import { useState } from 'react';
import Link from 'next/link';
import { useQuery, keepPreviousData } from '@tanstack/react-query';

import { Panel } from '@/components/reveal';
import { Table, TableWrap, TBody, Td, Th, THead, TR } from '@/components/ui';
import { apiGet, asExample, type Envelope } from '@/api/client';
import { truncateMiddle } from '@/lib/format';
import type { components } from '@/api/types';

import { formatTimestamp, stroopsToXlm } from '../explorer-shared';
import {
  edgeEvents,
  errorStatus,
  RELATION,
  sortEdges,
  type EdgeSortKey,
  type Relation,
  type SortDirection,
} from './accountRelation';

type AccountGraphResp = components['schemas']['AccountGraph'];

// PAGE_SIZE bounds what this table ever holds. It is not a display
// preference: the busiest sponsor on the network covers 785,615 distinct
// accounts, and the endpoint is keyset-paged for that reason. The table
// walks the cursor; it never tries to hold the set.
const PAGE_SIZE = 50;

const numFmt = new Intl.NumberFormat('en-US');

/**
 * AccountRelationEdges — every account this address created or
 * sponsored, over GET /v1/accounts/{g}/graph?relation=…
 *
 * SORTING IS PAGE-LOCAL AND SAYS SO. The endpoint orders by counterparty
 * account id (that ordering IS the cursor), so there is no server-side
 * "top by creations" to ask for. Sorting the loaded page is genuinely
 * useful and genuinely not a ranking of the whole set, so the header
 * controls reorder the page and the note under the table states the
 * scope. Presenting a page-local sort as a leaderboard would be the
 * same error as reading a snapshot as a total.
 */
export function AccountRelationEdges({
  account,
  relation,
  /** Whole-set size from the graph summary, for the "x of y" line. */
  total,
}: {
  account: string;
  relation: Relation;
  total: number;
}) {
  const [cursor, setCursor] = useState('');
  const [pageIndex, setPageIndex] = useState(0);
  const [sortKey, setSortKey] = useState<EdgeSortKey>('account');
  const [direction, setDirection] = useState<SortDirection>('asc');

  const vocabulary = RELATION[relation];
  const creation = relation === 'created';
  const params = { relation, limit: PAGE_SIZE, ...(cursor ? { cursor } : {}) };

  const { data, isLoading, isError, error, isPlaceholderData } =
    useQuery<AccountGraphResp>({
      queryKey: ['/v1/accounts/{id}/graph', account, relation, cursor],
      enabled: account.length > 0,
      retry: false,
      placeholderData: keepPreviousData,
      staleTime: 60_000,
      queryFn: async () => {
        const env = await apiGet<Envelope<AccountGraphResp>>(
          `/v1/accounts/${encodeURIComponent(account)}/graph`,
          params,
        );
        return env.data;
      },
    });

  const title = `Accounts ${creation ? 'created' : 'sponsored'}`;
  const source = asExample(`/v1/accounts/${account}/graph`, params);

  if (isLoading) {
    return (
      <Panel
        headingLevel={2}
        title={title}
        source={source}
        bodyClassName="text-sm text-ink-muted"
      >
        Loading the edge list…
      </Panel>
    );
  }

  if (isError || !data) {
    const status = errorStatus(error);
    return (
      <Panel
        headingLevel={2}
        title={title}
        source={source}
        bodyClassName="text-sm text-ink-muted"
      >
        {status === 503
          ? 'The graph is warming — it is rebuilt by a rollup cycle, and this deployment has not completed one yet.'
          : `The edge list could not be read${error instanceof Error ? `: ${error.message}` : ''}.`}
      </Panel>
    );
  }

  const edges = data.edges ?? [];

  if (edges.length === 0) {
    return (
      <Panel
        headingLevel={2}
        title={title}
        source={source}
        bodyClassName="text-sm text-ink-muted"
      >
        This address has no {vocabulary.counterparties} in the covered span.
      </Panel>
    );
  }

  const rows = sortEdges(edges, sortKey, direction);

  function toggle(key: EdgeSortKey) {
    if (key === sortKey) {
      setDirection((d) => (d === 'asc' ? 'desc' : 'asc'));
      return;
    }
    setSortKey(key);
    // Account id reads naturally ascending; every quantity reads
    // largest-first, which is what a reader opening a sort wants.
    setDirection(key === 'account' ? 'asc' : 'desc');
  }

  const header = (key: EdgeSortKey, label: string, align?: 'right') => (
    <SortableTh
      align={align}
      label={label}
      active={sortKey === key}
      direction={direction}
      onClick={() => toggle(key)}
    />
  );

  return (
    <Panel
      headingLevel={2}
      title={title}
      hint={`one row per counterparty — ${numFmt.format(total)} in all`}
      source={source}
      bodyClassName="space-y-3"
    >
      <TableWrap>
        <Table>
          <THead>
            <TR className="hover:bg-transparent">
              {header('account', 'Account')}
              {header('events', vocabulary.eventColumn, 'right')}
              {creation && header('funded', 'XLM funded', 'right')}
              {header('first', 'First', 'right')}
              {header('last', 'Last', 'right')}
            </TR>
          </THead>
          <TBody>
            {rows.map((e) => (
              <TR key={e.account}>
                <Td>
                  <Link
                    href={`/accounts/${encodeURIComponent(e.account)}/`}
                    className="text-brand-600 font-mono text-xs hover:underline"
                    title={e.account}
                  >
                    {truncateMiddle(e.account, 10, 6)}
                  </Link>
                </Td>
                <Td align="right">{numFmt.format(edgeEvents(e))}</Td>
                {creation && (
                  <Td align="right" className="font-mono">
                    {stroopsToXlm(e.funded_stroops)}
                  </Td>
                )}
                <Td align="right">{formatTimestamp(e.first_at)}</Td>
                <Td align="right">{formatTimestamp(e.last_at)}</Td>
              </TR>
            ))}
          </TBody>
        </Table>
      </TableWrap>

      <div className="flex flex-wrap items-center gap-2 text-xs">
        <span className="text-ink-faint">
          Page {pageIndex + 1}: {numFmt.format(rows.length)} of{' '}
          {numFmt.format(total)}
          {isPlaceholderData ? ' (loading…)' : ''}.
        </span>
        {/* Named explicitly: the visible "← First" would otherwise share
            an accessible name with the sortable "First" column header
            two rows above it, leaving a screen-reader user two identical
            controls that do unrelated things. */}
        {cursor && (
          <button
            type="button"
            aria-label="First page"
            onClick={() => {
              setCursor('');
              setPageIndex(0);
            }}
            className="border-line text-ink-body hover:border-brand-500 rounded-md border px-2.5 py-1"
          >
            ← First
          </button>
        )}
        {data.next_cursor && (
          <button
            type="button"
            aria-label="Next page"
            onClick={() => {
              setCursor(data.next_cursor ?? '');
              setPageIndex((i) => i + 1);
            }}
            className="border-line text-ink-body hover:border-brand-500 ml-auto rounded-md border px-2.5 py-1"
          >
            More →
          </button>
        )}
      </div>

      <p className="text-ink-faint text-[11px]">
        The column headers sort <strong>this page</strong>, not the whole set.
        The endpoint is keyset-paged by counterparty account id — that ordering
        is the cursor — so there is no whole-set &quot;top by{' '}
        {vocabulary.eventColumn.toLowerCase()}&quot; to request, and a client
        that sorted {numFmt.format(total)} rows would have to hold them all
        first.{' '}
        {creation
          ? 'A funded figure of 0 XLM is real, not missing: since CAP-33 an account can be created with no balance of its own, its reserve covered by a sponsor.'
          : 'Each row counts arrangements this pair BEGAN. It is never a count of sponsorships in force — an arrangement also ends silently when the sponsored entry is deleted or the account merges away.'}
      </p>
    </Panel>
  );
}

/**
 * A sortable column header. `aria-sort` on the <th> is what tells a
 * screen-reader user the table is ordered and by which column, and the
 * button inside it is what puts the control in the tab order — a bare
 * onClick on the <th> would be neither.
 */
function SortableTh({
  label,
  align,
  active,
  direction,
  onClick,
}: {
  label: string;
  align?: 'right';
  active: boolean;
  direction: SortDirection;
  onClick: () => void;
}) {
  return (
    <Th
      align={align}
      aria-sort={
        active ? (direction === 'asc' ? 'ascending' : 'descending') : 'none'
      }
    >
      <button
        type="button"
        onClick={onClick}
        className="focus-visible:ring-brand-500/60 hover:text-ink rounded-sm focus-visible:ring-2 focus-visible:outline-hidden"
      >
        {label}
        <span aria-hidden="true" className="ml-1">
          {active ? (direction === 'asc' ? '▲' : '▼') : '↕'}
        </span>
      </button>
    </Th>
  );
}
