'use client';

import Link from 'next/link';
import { useQuery } from '@tanstack/react-query';

import { Panel } from '@/components/reveal';
import {
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
import { formatCompact, truncateMiddle } from '@/lib/format';
import { type Envelope, formatTimestamp } from '../../explorer-shared';

// Mirrors api/v1 explorer.AccountSponsorsView (GET /v1/accounts/sponsors).
interface SponsorRow {
  rank: number;
  account: string;
  sponsorships_started: number;
  distinct_sponsored: number;
  revocations_issued: number;
  first_ledger: number;
  last_ledger: number;
  first_seen_at: string;
  last_seen_at: string;
}

interface SponsorsResp {
  sponsors: SponsorRow[];
  totals: {
    sponsors: number;
    sponsorships_started: number;
    distinct_sponsored: number;
    revocations_issued: number;
  };
  coverage: {
    from_ledger: number;
    thru_ledger: number;
    from_time: string;
    thru_time: string;
    ambiguous_transactions: number;
  };
  computed_at: string;
}

const BOARD_LIMIT = 50;

// repeatRate shows how often a sponsor re-sponsors the same accounts —
// a display convenience over the two exact counts beside it.
function repeatRate(row: SponsorRow): string {
  if (row.distinct_sponsored === 0) return '—';
  const r = row.sponsorships_started / row.distinct_sponsored;
  return r < 1.05 ? '1×' : `${r.toFixed(1)}×`;
}

/**
 * SponsorBoard — the sponsor league table (#351): which accounts have
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
        <p className="text-ink-muted text-[11px]">
          Covers ledgers{' '}
          <span className="tnum">
            {d.coverage.from_ledger.toLocaleString('en-US')}
          </span>
          –
          <span className="tnum">
            {d.coverage.thru_ledger.toLocaleString('en-US')}
          </span>{' '}
          ({formatTimestamp(d.coverage.from_time)} to{' '}
          {formatTimestamp(d.coverage.thru_time)}). That floor is where
          sponsorship began on the network — protocol 14 introduced it, and no
          sponsorship operation exists before that ledger — so this is the whole
          history of the feature, not a truncated window. Snapshot computed{' '}
          {formatTimestamp(d.computed_at)}.
          {d.coverage.ambiguous_transactions > 0 && (
            <>
              {' '}
              {d.coverage.ambiguous_transactions.toLocaleString('en-US')}{' '}
              transactions carried more than one sponsor and are excluded from
              per-sponsor attribution.
            </>
          )}
        </p>
      </Panel>

      <Panel
        headingLevel={2}
        title={`Top ${d.sponsors.length} by sponsorships started`}
        source={source}
        bodyClassName="space-y-3"
      >
        <p className="text-ink-muted text-xs">
          <strong>Started</strong> counts arrangements begun;{' '}
          <strong>accounts</strong> counts the distinct accounts they covered.
          They diverge when a sponsor re-sponsors the same accounts, which is
          common — the <strong>repeat</strong> column is the ratio.{' '}
          <strong>Revoked</strong> counts revocations this account issued.
        </p>
        <TableWrap>
          <Table>
            <THead>
              <TR>
                <Th align="right">#</Th>
                <Th>Sponsor</Th>
                <Th align="right">Started</Th>
                <Th align="right">Accounts</Th>
                <Th align="right">Repeat</Th>
                <Th align="right">Revoked</Th>
                <Th align="right">First seen</Th>
                <Th align="right">Last seen</Th>
              </TR>
            </THead>
            <TBody>
              {d.sponsors.map((s) => (
                <TR key={s.account}>
                  <Td align="right">{s.rank}</Td>
                  <Td>
                    <Link
                      href={`/accounts/${encodeURIComponent(s.account)}/`}
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
