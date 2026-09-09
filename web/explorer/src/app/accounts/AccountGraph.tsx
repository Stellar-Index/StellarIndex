'use client';

import { useState } from 'react';
import Link from 'next/link';
import { useQuery, keepPreviousData } from '@tanstack/react-query';

import { Panel } from '@/components/reveal';
import {
  Segmented,
  Table,
  TableWrap,
  TBody,
  Td,
  Th,
  THead,
  TR,
} from '@/components/ui';
import { apiGet, asExample } from '@/api/client';
import { truncateMiddle } from '@/lib/format';
import type { components } from '@/api/types';
import {
  type Envelope,
  formatTimestamp,
  stroopsToXlm,
} from '../explorer-shared';

type AccountGraphResp = components['schemas']['AccountGraph'];
type AccountGraphEdge = components['schemas']['AccountGraphEdge'];
type Relation = 'created' | 'sponsored';

// PAGE_SIZE bounds what this panel ever renders at once. It is not a
// display preference: the busiest sponsor on the network covers 785,543
// distinct accounts, so a view that tried to draw "everyone X sponsored"
// would not be a feature. The panel pages, and says how far into the set
// the page reaches.
const PAGE_SIZE = 25;

const numFmt = new Intl.NumberFormat('en-US');

function AccountCell({ account }: { account: string }) {
  return (
    <Link
      href={`/accounts/${encodeURIComponent(account)}/`}
      className="text-brand-600 font-mono text-xs hover:underline"
      title={account}
    >
      {truncateMiddle(account, 10, 6)}
    </Link>
  );
}

/**
 * InboundList — one inbound direction ("created by" / "sponsored by").
 *
 * Rendered as a list rather than a single value on purpose: an address
 * can be created, merged away and created again, and it can be sponsored
 * by several accounts over its life. The API caps this side and reports
 * the exact total, so the cap is stated when it bites instead of a short
 * list quietly reading as the whole truth.
 */
function InboundList({
  label,
  side,
  empty,
}: {
  label: string;
  side: AccountGraphResp['inbound']['created_by'];
  empty: string;
}) {
  return (
    <div>
      <div className="text-ink-muted text-[11px] tracking-wider uppercase">
        {label}
      </div>
      {side.edges.length === 0 ? (
        <p className="text-ink-muted mt-1 text-sm">{empty}</p>
      ) : (
        <ul className="mt-1 space-y-1">
          {side.edges.map((e) => (
            <li
              key={e.account}
              className="flex flex-wrap items-baseline gap-x-2 text-xs"
            >
              <AccountCell account={e.account} />
              <span className="text-ink-muted">
                {e.creations != null && e.creations > 1
                  ? `${numFmt.format(e.creations)}× created`
                  : null}
                {e.sponsorships_started != null
                  ? `${numFmt.format(e.sponsorships_started)} started`
                  : null}
              </span>
              <span className="text-ink-faint">
                {formatTimestamp(e.first_at)}
              </span>
            </li>
          ))}
        </ul>
      )}
      {side.truncated && (
        <p className="text-ink-faint mt-1 text-[11px]">
          Showing {numFmt.format(side.edges.length)} of{' '}
          {numFmt.format(side.total)}.
        </p>
      )}
    </div>
  );
}

/**
 * AccountGraphPanel — the account's place in the sponsorship and
 * account-creation graph, over GET /v1/accounts/{g}/graph.
 *
 * Two directions, deliberately shaped differently because the data is:
 * INBOUND ("where did this account come from") is small and always
 * shown; OUTBOUND ("whom did it create/sponsor") runs to hundreds of
 * thousands of edges for the busiest accounts, so it is summarised by
 * default and its edge list is fetched only when a reader asks for one
 * of the two relations, PAGE_SIZE rows at a time.
 *
 * Everything rendered is HISTORY. A sponsorship edge means an
 * arrangement was started, never that one is in force — the API's `note`
 * is rendered verbatim so that contract travels with the numbers.
 */
export function AccountGraphPanel({ id }: { id: string }) {
  const [relation, setRelation] = useState<Relation | ''>('');
  const [cursor, setCursor] = useState('');

  const { data, isLoading, isError, error } = useQuery<AccountGraphResp>({
    queryKey: ['/v1/accounts/{id}/graph', id, relation, cursor],
    enabled: id.length > 0,
    retry: false,
    placeholderData: keepPreviousData,
    queryFn: async () => {
      const env = await apiGet<Envelope<AccountGraphResp>>(
        `/v1/accounts/${encodeURIComponent(id)}/graph`,
        {
          ...(relation ? { relation, limit: PAGE_SIZE } : {}),
          ...(cursor ? { cursor } : {}),
        },
      );
      return env.data;
    },
    staleTime: 60_000,
  });

  const source = asExample(
    `/v1/accounts/${id}/graph`,
    relation ? { relation, limit: PAGE_SIZE } : undefined,
  );
  const panelHint =
    'who created and sponsored this account, and whom it created and sponsored';

  if (isError) {
    return (
      <Panel
        title="Sponsorship + creation graph"
        hint={panelHint}
        source={source}
        bodyClassName="text-sm text-ink-body"
      >
        The account graph is warming or failed — reload to retry
        {error instanceof Error ? `: ${error.message}` : ''}.
      </Panel>
    );
  }

  if (isLoading || !data) {
    return (
      <Panel
        title="Sponsorship + creation graph"
        hint={panelHint}
        source={source}
        bodyClassName="text-sm text-ink-muted"
      >
        Loading…
      </Panel>
    );
  }

  const created = data.outbound.created;
  const sponsored = data.outbound.sponsored;
  const shown = relation === 'created' ? created.accounts : sponsored.accounts;

  // Only the directions that exist are offered, and "Summary" is always
  // the default: the edge list behind either option is the unbounded
  // side of the graph, so it is never fetched until a reader asks.
  const options = [
    { label: 'Summary', value: '' },
    ...(created.accounts > 0
      ? [{ label: 'Accounts created', value: 'created' }]
      : []),
    ...(sponsored.accounts > 0
      ? [{ label: 'Accounts sponsored', value: 'sponsored' }]
      : []),
  ];

  return (
    <Panel
      title="Sponsorship + creation graph"
      hint={panelHint}
      source={source}
      bodyClassName="space-y-4"
    >
      <div className="grid gap-4 sm:grid-cols-2">
        <InboundList
          label="Created by"
          side={data.inbound.created_by}
          empty="No CreateAccount operation for this address is indexed — it may predate the coverage below, or the address may never have existed."
        />
        <InboundList
          label="Sponsored by"
          side={data.inbound.sponsored_by}
          empty="No account has begun a sponsorship arrangement covering this one."
        />
      </div>

      <dl className="grid grid-cols-2 gap-x-6 gap-y-4 sm:grid-cols-4">
        <GraphStat
          label="Accounts created"
          value={numFmt.format(created.accounts)}
        />
        <GraphStat
          label="XLM funded"
          value={
            created.accounts > 0 ? stroopsToXlm(created.funded_stroops) : '—'
          }
        />
        <GraphStat
          label="Accounts sponsored"
          value={numFmt.format(sponsored.accounts)}
        />
        <GraphStat
          label="Revocations issued"
          value={numFmt.format(sponsored.revocations_issued)}
        />
      </dl>

      {options.length > 1 && (
        <Segmented
          ariaLabel="Outbound graph direction"
          options={options}
          value={relation}
          onChange={(v) => {
            setRelation(v as Relation | '');
            setCursor('');
          }}
        />
      )}

      {relation && data.edges && (
        <div className="space-y-2">
          <TableWrap>
            <Table>
              <THead>
                <TR>
                  <Th>Account</Th>
                  <Th align="right">
                    {relation === 'created' ? 'Creations' : 'Started'}
                  </Th>
                  {relation === 'created' && <Th align="right">XLM funded</Th>}
                  <Th align="right">First</Th>
                  <Th align="right">Last</Th>
                </TR>
              </THead>
              <TBody>
                {data.edges.map((e: AccountGraphEdge) => (
                  <TR key={e.account}>
                    <Td>
                      <AccountCell account={e.account} />
                    </Td>
                    <Td align="right">
                      {numFmt.format(
                        e.creations ?? e.sponsorships_started ?? 0,
                      )}
                    </Td>
                    {relation === 'created' && (
                      <Td align="right">{stroopsToXlm(e.funded_stroops)}</Td>
                    )}
                    <Td align="right">{formatTimestamp(e.first_at)}</Td>
                    <Td align="right">{formatTimestamp(e.last_at)}</Td>
                  </TR>
                ))}
              </TBody>
            </Table>
          </TableWrap>
          <div className="flex items-center gap-2 text-xs">
            {/* The page is a window on a set that can hold hundreds of
                thousands of edges, so the panel says how big the set is
                rather than implying the page is all of it. */}
            <span className="text-ink-faint">
              Showing {numFmt.format(data.edges.length)} of{' '}
              {numFmt.format(shown)}.
            </span>
            {cursor && (
              <button
                onClick={() => setCursor('')}
                className="border-line text-ink-body hover:border-brand-500 rounded-md border px-2.5 py-1"
              >
                ← First
              </button>
            )}
            {data.next_cursor && (
              <button
                onClick={() => setCursor(data.next_cursor ?? '')}
                className="border-line text-ink-body hover:border-brand-500 ml-auto rounded-md border px-2.5 py-1"
              >
                More →
              </button>
            )}
          </div>
        </div>
      )}

      {/* The honesty contract travels with the numbers, not only with
          the docs: nothing here is a live-sponsorship figure. */}
      <p className="text-ink-faint text-[11px]">{data.note}</p>
      <p className="text-ink-faint text-[11px]">
        Creation history covers ledgers{' '}
        {numFmt.format(data.coverage.creation.from_ledger)}–
        {numFmt.format(data.coverage.creation.thru_ledger)}; sponsorship history
        covers {numFmt.format(data.coverage.sponsorship.from_ledger)}–
        {numFmt.format(data.coverage.sponsorship.thru_ledger)}, whose floor is
        where sponsorship began to exist on the network, not a gap.
      </p>
    </Panel>
  );
}

function GraphStat({ label, value }: { label: string; value: string }) {
  return (
    <div>
      <dt className="text-ink-muted text-[11px] tracking-wider uppercase">
        {label}
      </dt>
      <dd className="mt-0.5 font-mono text-sm tabular-nums">{value}</dd>
    </div>
  );
}
