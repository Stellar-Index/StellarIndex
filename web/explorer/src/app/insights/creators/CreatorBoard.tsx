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
import {
  type Envelope,
  stroopsToXlm,
  formatTimestamp,
} from '../../explorer-shared';

// Mirrors api/v1 explorer.AccountCreatorsView (GET /v1/accounts/creators).
// Stroops arrive as strings (ADR-0003) and are never parsed through
// Number() here — stroopsToXlm does the BigInt division.
interface CreatorRow {
  rank: number;
  account: string;
  accounts_created: number;
  funded_stroops: string;
  live_accounts: number;
  live_stroops: string;
  first_ledger: number;
  last_ledger: number;
  first_created_at: string;
  last_created_at: string;
}

interface CreatorsResp {
  creators: CreatorRow[];
  totals: { creators: number; accounts_created: number; live_accounts: number };
  coverage: {
    from_ledger: number;
    thru_ledger: number;
    from_time: string;
    thru_time: string;
  };
  computed_at: string;
}

const BOARD_LIMIT = 50;

// survivalPct is the share of a creator's accounts that still exist. It
// is a display convenience over the two exact counts beside it, which
// remain the auditable figures.
function survivalPct(row: CreatorRow): string {
  if (row.accounts_created === 0) return '—';
  return `${((row.live_accounts / row.accounts_created) * 100).toFixed(1)}%`;
}

/**
 * CreatorBoard — the account-creator league table (#351): which accounts
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
        <p className="text-ink-muted text-[11px]">
          Counts cover ledgers{' '}
          <span className="tnum">
            {d.coverage.from_ledger.toLocaleString('en-US')}
          </span>
          –
          <span className="tnum">
            {d.coverage.thru_ledger.toLocaleString('en-US')}
          </span>{' '}
          ({formatTimestamp(d.coverage.from_time)} to{' '}
          {formatTimestamp(d.coverage.thru_time)}) — the span the rollup
          actually aggregated, not a claim about the whole chain. Snapshot
          computed {formatTimestamp(d.computed_at)}.
        </p>
      </Panel>

      <Panel
        headingLevel={2}
        title={`Top ${d.creators.length} by accounts created`}
        source={source}
        bodyClassName="space-y-3"
      >
        <p className="text-ink-muted text-xs">
          Ranked by accounts created, all-time within the covered span.{' '}
          <strong>Created</strong> and <strong>funded</strong> are immutable
          history — a creation never un-happens. <strong>Live</strong> and{' '}
          <strong>XLM held</strong> are point-in-time: created accounts merge
          away and balances move.
        </p>
        <TableWrap>
          <Table>
            <THead>
              <TR>
                <Th align="right">#</Th>
                <Th>Creator</Th>
                <Th align="right">Created</Th>
                <Th align="right">Funded (XLM)</Th>
                <Th align="right">Live</Th>
                <Th align="right">Survived</Th>
                <Th align="right">XLM held now</Th>
                <Th align="right">Last created</Th>
              </TR>
            </THead>
            <TBody>
              {d.creators.map((c) => (
                <TR key={c.account}>
                  <Td align="right">{c.rank}</Td>
                  <Td>
                    <Link
                      href={`/accounts/${encodeURIComponent(c.account)}/`}
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
          A funded figure of 0 XLM is real, not missing: since CAP-33 an account
          can be created with no balance of its own, its reserve covered by a
          sponsor.
        </p>
      </Panel>
    </>
  );
}
