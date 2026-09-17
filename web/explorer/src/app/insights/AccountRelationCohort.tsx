'use client';

import Link from 'next/link';
import { useQuery } from '@tanstack/react-query';

import { Panel } from '@/components/reveal';
import { LineChart, type LinePoint } from '@/components/charts/LineChart';
import {
  Callout,
  Stat,
  Table,
  TableWrap,
  TBody,
  Td,
  Th,
  THead,
  TR,
} from '@/components/ui';
import { apiGet, asExample, type Envelope } from '@/api/client';
import { formatCompact, truncateMiddle } from '@/lib/format';
import type { components } from '@/api/types';

import { formatTimestamp } from '../explorer-shared';
import { RELATION, errorStatus, type Relation } from './accountRelation';

type CohortResp = components['schemas']['AccountCohort'];
type CohortHolding = components['schemas']['AccountCohortHolding'];
type CohortPoint = components['schemas']['AccountCohortFlowPoint'];

const numFmt = new Intl.NumberFormat('en-US');
const usdFmt = new Intl.NumberFormat('en-US', {
  style: 'currency',
  currency: 'USD',
  maximumFractionDigits: 0,
});

const HOLDINGS_SHOWN = 25;

/** A canonical asset id as a short label: XLM, the code of CODE-ISSUER, a
 * clipped pool or contract id otherwise. */
function assetLabel(asset: string, kind: CohortHolding['kind']): string {
  if (kind === 'native') return 'XLM';
  if (kind === 'classic') return asset.split('-')[0] ?? asset;
  if (kind === 'pool_share') return `pool ${truncateMiddle(asset.slice(5), 10)}`;
  return truncateMiddle(asset, 12);
}

function usd(s: string | undefined): string {
  if (s === undefined) return '—';
  const n = Number(s);
  return Number.isFinite(n) ? usdFmt.format(n) : s;
}

/** Sums a month's per-asset USD figures over the assets that carry one.
 * Assets without a price contribute nothing and are not zero. */
function monthUSD(p: CohortPoint, side: 'inflow_usd' | 'outflow_usd'): number | null {
  let sum = 0;
  let any = false;
  for (const a of p.by_asset) {
    const v = a[side];
    if (v === undefined) continue;
    const n = Number(v);
    if (!Number.isFinite(n)) continue;
    sum += n;
    any = true;
  }
  return any ? sum : null;
}

function monthTime(p: CohortPoint): number {
  return Math.floor(Date.parse(p.period_start) / 1000);
}

/**
 * The cohort panels of a sponsor / creator detail page: what the accounts
 * this address stood behind went on to hold and do. Every figure is the
 * cohort rollup's last cycle, from the cohort's own ledger footprint.
 */
export function AccountRelationCohort({
  account,
  relation,
}: {
  account: string;
  relation: Relation;
}) {
  const vocabulary = RELATION[relation];
  const path = `/v1/accounts/${encodeURIComponent(account)}/graph/cohort?relation=${relation}`;
  const { data, isLoading, isError, error } = useQuery<CohortResp>({
    queryKey: ['/v1/accounts/{id}/graph/cohort', account, relation],
    enabled: account.length > 0,
    staleTime: 5 * 60_000,
    retry: false,
    queryFn: async () => {
      const env = await apiGet<Envelope<CohortResp>>(path);
      return env.data;
    },
  });

  const title = `What the ${vocabulary.counterparties} went on to hold and do`;
  const source = asExample(path);

  if (isLoading) {
    return (
      <Panel headingLevel={2} title={title} source={source} bodyClassName="text-sm text-ink-muted">
        Loading the cohort…
      </Panel>
    );
  }
  if (isError) {
    const status = errorStatus(error);
    return (
      <Panel headingLevel={2} title={title} source={source} bodyClassName="text-sm text-ink-muted">
        {status === 503
          ? 'The cohort rollup has not completed its first cycle on this deployment yet — retry shortly.'
          : 'The cohort could not be read.'}
      </Panel>
    );
  }
  if (!data) return null;

  if (!data.covered) {
    return (
      <Panel headingLevel={2} title={title} source={source} bodyClassName="space-y-2 text-sm text-ink-muted">
        <p>
          The rollup does not carry this {vocabulary.actor}&rsquo;s cohort: it
          covers every sponsor and every creator with at least ten accounts to
          its name, and this address has fewer edges than that in this relation
          — the per-account pages are the better read for a handful of
          accounts. This is not a statement that the cohort holds nothing.
        </p>
        {data.cycle && (
          <p className="text-ink-faint text-[11px]">
            Rollup last computed {formatTimestamp(data.cycle.computed_at)} at ledger{' '}
            {numFmt.format(data.cycle.tip_ledger)}.
          </p>
        )}
      </Panel>
    );
  }

  const cohort = data.cohort;
  const holdings = data.holdings.slice(0, HOLDINGS_SHOWN);
  const hidden = data.holdings.length - holdings.length;
  const activeLine: LinePoint[] = data.flows.points.map((p) => ({
    time: monthTime(p),
    value: p.active_accounts,
  }));
  const inLine: LinePoint[] = [];
  const outLine: LinePoint[] = [];
  for (const p of data.flows.points) {
    const i = monthUSD(p, 'inflow_usd');
    const o = monthUSD(p, 'outflow_usd');
    if (i !== null) inLine.push({ time: monthTime(p), value: i });
    if (o !== null) outLine.push({ time: monthTime(p), value: o });
  }
  const anyUSD = inLine.length > 0 || outLine.length > 0;
  const labelled = data.contracts.filter((c) => c.protocol);
  const unlabelled = data.contracts.length - labelled.length;

  return (
    <>
      <Panel headingLevel={2} title={title} source={source} bodyClassName="space-y-4">
        {cohort && (
          <dl className="grid grid-cols-2 gap-x-6 gap-y-3 sm:grid-cols-3 lg:grid-cols-6">
            <Stat label="Accounts" value={numFmt.format(cohort.accounts)} sub={`${vocabulary.counterparties}`} />
            <Stat label="Still live" value={numFmt.format(cohort.live_accounts)} sub="have an account entry now" />
            <Stat label="Active 30d" value={numFmt.format(cohort.active_30d)} sub="seen in the last 30 days" />
            <Stat label="Active 90d" value={numFmt.format(cohort.active_90d)} />
            <Stat label="Active 1y" value={numFmt.format(cohort.active_365d)} />
            <Stat
              label="Holdings, priced"
              value={data.valuation.total_usd !== undefined ? usd(data.valuation.total_usd) : '—'}
              sub={
                data.valuation.priced_holdings > 0
                  ? `${data.valuation.priced_holdings} priced · ${data.valuation.unpriced_holdings} unpriced`
                  : 'nothing here has a live price'
              }
            />
          </dl>
        )}

        <TableWrap>
          <Table>
            <THead>
              <TR>
                <Th>Asset</Th>
                <Th align="right">Holders</Th>
                <Th align="right">Balance</Th>
                <Th align="right">Value (USD)</Th>
              </TR>
            </THead>
            <TBody>
              {holdings.map((h) => (
                <TR key={h.asset}>
                  <Td>
                    <span className="font-mono text-xs" title={h.asset}>
                      {assetLabel(h.asset, h.kind)}
                    </span>
                    {h.kind === 'pool_share' && (
                      <span className="text-ink-faint ml-2 text-[11px]">liquidity-pool share</span>
                    )}
                  </Td>
                  <Td align="right">{numFmt.format(h.holders)}</Td>
                  <Td align="right">{formatCompact(Number(h.balance))}</Td>
                  <Td align="right">{h.value_usd !== undefined ? usd(h.value_usd) : <span className="text-ink-faint">unpriced</span>}</Td>
                </TR>
              ))}
              {holdings.length === 0 && (
                <TR>
                  <Td colSpan={4} className="text-ink-muted">
                    No live balances in the cohort.
                  </Td>
                </TR>
              )}
            </TBody>
          </Table>
        </TableWrap>
        {(hidden > 0 || data.holdings_truncated) && (
          <p className="text-ink-faint text-[11px]">
            {hidden > 0 && `${numFmt.format(hidden)} more asset${hidden === 1 ? '' : 's'} held, not shown. `}
            {data.holdings_truncated && 'The read cap applied: these are the most widely held assets, not all of them.'}
          </p>
        )}
        <p className="text-ink-faint text-[11px]">
          Current balances of the cohort, valued at the live rate where one exists.
          A pool share is a classic liquidity-pool position and is never priced;
          nothing unpriced is counted at zero.
        </p>
      </Panel>

      <Panel headingLevel={2} title="Value moved by month" source={source} bodyClassName="space-y-3">
        {data.flows.points.length === 0 ? (
          <p className="text-sm text-ink-muted">No movements recorded for this cohort.</p>
        ) : (
          <>
            <LineChart
              data={activeLine}
              height={200}
              positive
              ariaLabel={`Members of the ${vocabulary.actor}'s cohort active each month`}
              legend={{ valueLabel: 'Active accounts', formatValue: (n) => numFmt.format(n) }}
            />
            {anyUSD ? (
              <LineChart
                data={inLine}
                series={[
                  { label: 'Moved in', data: inLine, tone: 'up' },
                  { label: 'Moved out', data: outLine, tone: 'down' },
                ]}
                height={220}
                ariaLabel="USD value moved into and out of the cohort each month, at today's prices"
                legend={{ valueLabel: 'USD, at today’s prices', formatValue: (n) => usd(String(n)) }}
              />
            ) : (
              <Callout tone="info" title="No priced asset moved">
                The cohort&rsquo;s movements are in assets nothing prices live, so
                there is no USD line to draw; the activity line above is exact.
              </Callout>
            )}
            <p className="text-ink-faint text-[11px]">
              Received minus sent per asset per calendar month, from the movements
              archive. The USD line values each month&rsquo;s quantity at
              today&rsquo;s price — one unit across months, not what the month was
              worth then. Broken out for{' '}
              {data.flows.assets.length} asset{data.flows.assets.length === 1 ? '' : 's'}
              ; the activity line counts every asset. A month with no movement
              emits no point.
            </p>
          </>
        )}
      </Panel>

      <Panel headingLevel={2} title="Protocols the cohort moved value through" source={source} bodyClassName="space-y-3">
        {data.contracts.length === 0 ? (
          <p className="text-sm text-ink-muted">No contract counterparties in the cohort&rsquo;s movements.</p>
        ) : (
          <TableWrap>
            <Table>
              <THead>
                <TR>
                  <Th>Protocol</Th>
                  <Th>Contract</Th>
                  <Th align="right">Accounts</Th>
                  <Th align="right">Movements</Th>
                  <Th>First</Th>
                  <Th>Last</Th>
                </TR>
              </THead>
              <TBody>
                {data.contracts.map((c) => (
                  <TR key={c.contract_id}>
                    <Td>
                      {c.protocol ??
                        (c.label ? (
                          <span className="text-ink-muted">{c.label}</span>
                        ) : (
                          <span className="text-ink-faint">unlabelled</span>
                        ))}
                    </Td>
                    <Td>
                      <Link href={`/contracts/${c.contract_id}`} className="font-mono text-xs underline-offset-2 hover:underline" title={c.contract_id}>
                        {truncateMiddle(c.contract_id, 14)}
                      </Link>
                    </Td>
                    <Td align="right">{numFmt.format(c.active_accounts)}</Td>
                    <Td align="right">{numFmt.format(c.movements)}</Td>
                    <Td>{formatTimestamp(c.first_at)}</Td>
                    <Td>{formatTimestamp(c.last_at)}</Td>
                  </TR>
                ))}
              </TBody>
            </Table>
          </TableWrap>
        )}
        <p className="text-ink-faint text-[11px]">
          The C… counterparties of the cohort&rsquo;s movements — the value-moving
          subset of interaction; a call that moved no balance is not counted.
          {unlabelled > 0 &&
            ` ${numFmt.format(unlabelled)} contract${unlabelled === 1 ? '' : 's'} no protocol on the roster claims; a token contract the lake can name is shown as \u201ctoken \u2026\u201d.`}
        </p>
      </Panel>

      <Panel headingLevel={2} title="DeFi positions held by the cohort" source={source} bodyClassName="space-y-3">
        {data.positions.length === 0 ? (
          <p className="text-sm text-ink-muted">No open positions in the protocols the served tier folds.</p>
        ) : (
          <TableWrap>
            <Table>
              <THead>
                <TR>
                  <Th>Protocol</Th>
                  <Th>Position</Th>
                  <Th>Venue</Th>
                  <Th>Asset</Th>
                  <Th align="right">Holders</Th>
                  <Th align="right">Amount</Th>
                </TR>
              </THead>
              <TBody>
                {data.positions.map((p) => (
                  <TR key={`${p.protocol}:${p.venue}:${p.asset ?? ''}:${p.position_kind}`}>
                    <Td>{p.protocol}</Td>
                    <Td>{p.position_kind.replace(/_/g, ' ')}</Td>
                    <Td>
                      <Link href={`/contracts/${p.venue}`} className="font-mono text-xs underline-offset-2 hover:underline" title={p.venue}>
                        {truncateMiddle(p.venue, 14)}
                      </Link>
                    </Td>
                    <Td>{p.asset_label ?? (p.asset ? truncateMiddle(p.asset, 12) : <span className="text-ink-faint">venue shares</span>)}</Td>
                    <Td align="right">{numFmt.format(p.holders)}</Td>
                    <Td align="right">{formatCompact(Number(p.amount))}</Td>
                  </TR>
                ))}
              </TBody>
            </Table>
          </TableWrap>
        )}
        <p className="text-ink-faint text-[11px]">
          From the served tier&rsquo;s per-protocol folds (blend, blend backstop,
          phoenix stake, defindex vaults, sorocredit, aquarius gauges) joined to
          the cohort. Amount is the fold&rsquo;s own unit summed across holders —
          a magnitude, not a settlement figure.
          {data.cycle && ` Rollup computed ${formatTimestamp(data.cycle.computed_at)}.`}
        </p>
      </Panel>
    </>
  );
}
