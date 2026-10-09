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
import type { SponsorRow, SponsorsResp } from '@/api/relationTypes';
import { formatCompact, truncateMiddle } from '@/lib/format';
import { PairedBars } from '@/components/charts/Bars';
import { ConcentrationDonut } from '../ConcentrationDonut';
import { type Envelope, formatTimestamp } from '../../explorer-shared';

// Both board shapes come from the generated OpenAPI contract via
// src/api/relationTypes.ts — never restated here.

const BOARD_LIMIT = 50;

// repeatRate shows how often a sponsor re-sponsors the same accounts —
// a display convenience over the two exact counts beside it.
function repeatRate(row: SponsorRow): string {
  if (row.distinct_sponsored === 0) return '—';
  const r = row.sponsorships_started / row.distinct_sponsored;
  return r < 1.05 ? '1×' : `${r.toFixed(1)}×`;
}

/**
 * SponsorBoard — the sponsor league table: which accounts have
 * paid the base reserves for other accounts' ledger entries.
 *
 * Everything on this board is HISTORY. It comes from replaying
 * sponsorship operations, which can say what an account has done but
 * never what is currently in force — an arrangement also lapses when the
 * sponsored entry is deleted or the account merges away, and neither
 * emits an operation. So there is deliberately no "currently sponsoring"
 * column here, and the copy says why rather than leaving a reader to
 * assume one is implied.
 */
export function SponsorBoard() {
  const q = useQuery<SponsorsResp>({
    queryKey: ['/v1/accounts/sponsors', BOARD_LIMIT],
    queryFn: async () => {
      const env = await apiGet<Envelope<SponsorsResp>>(
        '/v1/accounts/sponsors',
        {
          limit: BOARD_LIMIT,
        },
      );
      return env.data;
    },
    retry: false,
    refetchInterval: 10 * 60 * 1000,
  });

  const source = asExample('/v1/accounts/sponsors', { limit: BOARD_LIMIT });

  if (q.isLoading) {
    return (
      <Panel
        headingLevel={2}
        title="Account sponsors"
        source={source}
        bodyClassName="text-sm text-ink-muted"
      >
        Loading the sponsor board…
      </Panel>
    );
  }
  if (q.isError || !q.data) {
    return (
      <Panel
        headingLevel={2}
        title="Account sponsors"
        source={source}
        bodyClassName="text-sm text-ink-muted"
      >
        The sponsor board is warming — it is rebuilt by a rollup cycle, and this
        deployment has not completed one yet.
      </Panel>
    );
  }

  const d = q.data;

  return (
    <>
      <Panel
        headingLevel={2}
        title="Account sponsors"
        source={source}
        bodyClassName="space-y-5"
      >
        <dl className="grid grid-cols-2 gap-x-6 gap-y-4 sm:grid-cols-4">
          <Stat label="Sponsors" value={formatCompact(d.totals.sponsors)} />
          <Stat
            label="Sponsorships started"
            value={formatCompact(d.totals.sponsorships_started)}
          />
          <Stat
            label="Accounts sponsored"
            value={formatCompact(d.totals.distinct_sponsored)}
          />
          <Stat
            label="Revocations issued"
            value={formatCompact(d.totals.revocations_issued)}
          />
        </dl>
        <div className="flex flex-wrap items-center gap-2">
          <Badge title="The floor is where sponsorship began on the network (protocol 14); no sponsorship operation exists earlier, so this is the whole history of the feature, not a truncated window.">
            ledgers {d.coverage.from_ledger.toLocaleString('en-US')}–
            {d.coverage.thru_ledger.toLocaleString('en-US')} · computed{' '}
            {formatTimestamp(d.computed_at)}
          </Badge>
          {d.coverage.ambiguous_transactions > 0 && (
            <Badge
              tone="warn"
              title="Transactions carrying more than one sponsor are excluded from per-sponsor attribution."
            >
              {d.coverage.ambiguous_transactions.toLocaleString('en-US')}{' '}
              multi-sponsor txs excluded
            </Badge>
          )}
        </div>
      </Panel>

      <Panel
        headingLevel={2}
        title={`Top ${d.sponsors.length} by sponsorships started`}
        source={source}
        bodyClassName="space-y-3"
      >
        <ConcentrationDonut
          noun="sponsors"
          what="sponsorships started"
          total={d.totals.sponsorships_started}
          rows={d.sponsors.map((s) => ({
            id: s.account,
            label: truncateMiddle(s.account, 6, 4),
            count: s.sponsorships_started,
            href: `/insights/sponsors/${encodeURIComponent(s.account)}/`,
          }))}
        />
        <PairedBars
          ariaLabel="Sponsorships started and distinct accounts sponsored, top 10 sponsors"
          aLabel="Started"
          bLabel="Distinct sponsored"
          rows={d.sponsors.slice(0, 10).map((s) => ({
            id: s.account,
            label: truncateMiddle(s.account, 6, 4),
            a: s.sponsorships_started,
            b: s.distinct_sponsored,
          }))}
        />
        <TableWrap>
          <Table>
            <THead>
              <TR>
                <Th align="right">#</Th>
                <Th>Sponsor</Th>
                <Th align="right" title="Arrangements begun">
                  Started
                </Th>
                <Th align="right" title="Distinct accounts covered">
                  Accounts
                </Th>
                <Th
                  align="right"
                  title="Started / accounts: diverges when a sponsor re-sponsors the same accounts"
                >
                  Repeat
                </Th>
                <Th align="right" title="Revocations this account issued">
                  Revoked
                </Th>
                <Th align="right">First seen</Th>
                <Th align="right">Last seen</Th>
              </TR>
            </THead>
            <TBody>
              {d.sponsors.map((s) => (
                <TR key={s.account}>
                  <Td align="right">{s.rank}</Td>
                  <Td>
                    {/* Through to the sponsor's own page, not straight to
                        the generic account page: the board's columns are
                        summaries of a set, and the question a row raises
                        — WHICH accounts, and when — is what
                        /insights/sponsors/{g} answers. That page links on
                        to /accounts/{g} for everything else. */}
                    <Link
                      href={`/insights/sponsors/${encodeURIComponent(s.account)}/`}
                      className="text-brand-600 font-mono text-xs hover:underline"
                      title={s.account}
                    >
                      {truncateMiddle(s.account, 10, 6)}
                    </Link>
                  </Td>
                  <Td align="right">
                    {s.sponsorships_started.toLocaleString('en-US')}
                  </Td>
                  <Td align="right">
                    {s.distinct_sponsored.toLocaleString('en-US')}
                  </Td>
                  <Td align="right">{repeatRate(s)}</Td>
                  <Td align="right">
                    {s.revocations_issued.toLocaleString('en-US')}
                  </Td>
                  <Td align="right">{formatTimestamp(s.first_seen_at)}</Td>
                  <Td align="right">{formatTimestamp(s.last_seen_at)}</Td>
                </TR>
              ))}
            </TBody>
          </Table>
        </TableWrap>
      </Panel>
    </>
  );
}
