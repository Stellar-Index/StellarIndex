'use client';

import Link from 'next/link';
import { useQuery } from '@tanstack/react-query';

import { Panel } from '@/components/reveal';
import {
  Badge,
  Stat,
  TableWrap,
  Table,
  THead,
  TBody,
  TR,
  Th,
  Td,
} from '@/components/ui';
import { apiGet, asExample } from '@/api/client';
import type { CreatorRow, CreatorsResp } from '@/api/relationTypes';
import { formatCompact, truncateMiddle } from '@/lib/format';
import { PairedBars } from '@/components/charts/Bars';
import { ConcentrationDonut } from '../ConcentrationDonut';
import {
  type Envelope,
  stroopsToXlm,
  formatTimestamp,
} from '../../explorer-shared';

// Both board shapes come from the generated OpenAPI contract via
// src/api/relationTypes.ts — never restated here. Stroops arrive as
// strings (ADR-0003) and are never parsed through Number() here —
// stroopsToXlm does the BigInt division.

const BOARD_LIMIT = 50;

// survivalPct is the share of a creator's accounts that still exist. It
// is a display convenience over the two exact counts beside it, which
// remain the auditable figures.
function survivalPct(row: CreatorRow): string {
  if (row.accounts_created === 0) return '—';
  return `${((row.live_accounts / row.accounts_created) * 100).toFixed(1)}%`;
}

/**
 * CreatorBoard — the account-creator league table: which accounts
 * brought the most other accounts onto the network, and what that
 * created set holds today.
 *
 * The coverage strip under the board is not decoration. The board is a
 * rollup over the ledger span the cycle actually aggregated, and that
 * span is what the API reports — so the page states it rather than
 * letting a reader assume the counts are all of history.
 */
export function CreatorBoard() {
  const q = useQuery<CreatorsResp>({
    queryKey: ['/v1/accounts/creators', BOARD_LIMIT],
    queryFn: async () => {
      const env = await apiGet<Envelope<CreatorsResp>>(
        '/v1/accounts/creators',
        {
          limit: BOARD_LIMIT,
        },
      );
      return env.data;
    },
    retry: false,
    refetchInterval: 10 * 60 * 1000,
  });

  const source = asExample('/v1/accounts/creators', { limit: BOARD_LIMIT });

  if (q.isLoading) {
    return (
      <Panel
        headingLevel={2}
        title="Account creators"
        source={source}
        bodyClassName="text-sm text-ink-muted"
      >
        Loading the creator board…
      </Panel>
    );
  }
  if (q.isError || !q.data) {
    return (
      <Panel
        headingLevel={2}
        title="Account creators"
        source={source}
        bodyClassName="text-sm text-ink-muted"
      >
        The creator board is warming — it is rebuilt by a rollup cycle, and this
        deployment has not completed one yet.
      </Panel>
    );
  }

  const d = q.data;

  return (
    <>
      <Panel
        headingLevel={2}
        title="Account creators"
        source={source}
        bodyClassName="space-y-5"
      >
        <dl className="grid grid-cols-2 gap-x-6 gap-y-4 sm:grid-cols-4">
          <Stat label="Creators" value={formatCompact(d.totals.creators)} />
          <Stat
            label="Accounts created"
            value={formatCompact(d.totals.accounts_created)}
          />
          <Stat
            label="Still live"
            value={formatCompact(d.totals.live_accounts)}
          />
          <Stat
            label="Ledgers covered"
            value={`${formatCompact(d.coverage.from_ledger)}–${formatCompact(d.coverage.thru_ledger)}`}
          />
        </dl>
        <Badge title="The span the rollup actually aggregated, not a claim about the whole chain.">
          ledgers {d.coverage.from_ledger.toLocaleString('en-US')}–
          {d.coverage.thru_ledger.toLocaleString('en-US')} · computed{' '}
          {formatTimestamp(d.computed_at)}
        </Badge>
      </Panel>

      <Panel
        headingLevel={2}
        title={`Top ${d.creators.length} by accounts created`}
        source={source}
        bodyClassName="space-y-3"
      >
        <ConcentrationDonut
          noun="creators"
          what="accounts created"
          total={d.totals.accounts_created}
          rows={d.creators.map((c) => ({
            id: c.account,
            label: truncateMiddle(c.account, 6, 4),
            count: c.accounts_created,
            href: `/insights/creators/${encodeURIComponent(c.account)}/`,
          }))}
        />
        <PairedBars
          ariaLabel="Accounts created and still live, top 10 creators"
          aLabel="Created"
          bLabel="Still live"
          rows={d.creators.slice(0, 10).map((c) => ({
            id: c.account,
            label: truncateMiddle(c.account, 6, 4),
            a: c.accounts_created,
            b: c.live_accounts,
          }))}
        />
        <TableWrap>
          <Table>
            <THead>
              <TR>
                <Th align="right">#</Th>
                <Th>Creator</Th>
                <Th
                  align="right"
                  title="Immutable history: a creation never un-happens"
                >
                  Created
                </Th>
                <Th align="right" title="Immutable history">
                  Funded (XLM)
                </Th>
                <Th
                  align="right"
                  title="Point-in-time: created accounts merge away"
                >
                  Live
                </Th>
                <Th align="right">Survived</Th>
                <Th align="right" title="Point-in-time: balances move">
                  XLM held now
                </Th>
                <Th align="right">Last created</Th>
              </TR>
            </THead>
            <TBody>
              {d.creators.map((c) => (
                <TR key={c.account}>
                  <Td align="right">{c.rank}</Td>
                  <Td>
                    {/* Through to the creator's own page, not straight to
                        the generic account page: the board's columns are
                        summaries of a set, and the question a row raises
                        — WHICH accounts, and when — is what
                        /insights/creators/{g} answers. That page links on
                        to /accounts/{g} for everything else. */}
                    <Link
                      href={`/insights/creators/${encodeURIComponent(c.account)}/`}
                      className="text-brand-600 font-mono text-xs hover:underline"
                      title={c.account}
                    >
                      {truncateMiddle(c.account, 10, 6)}
                    </Link>
                  </Td>
                  <Td align="right">
                    {c.accounts_created.toLocaleString('en-US')}
                  </Td>
                  <Td align="right">{stroopsToXlm(c.funded_stroops)}</Td>
                  <Td align="right">
                    {c.live_accounts.toLocaleString('en-US')}
                  </Td>
                  <Td align="right">{survivalPct(c)}</Td>
                  <Td align="right">{stroopsToXlm(c.live_stroops)}</Td>
                  <Td align="right">{formatTimestamp(c.last_created_at)}</Td>
                </TR>
              ))}
            </TBody>
          </Table>
        </TableWrap>
        <p className="text-ink-muted text-[11px]">
          0 XLM funded is real: since CAP-33 a sponsor can cover the reserve.
        </p>
      </Panel>
    </>
  );
}
