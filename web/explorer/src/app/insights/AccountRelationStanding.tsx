'use client';

import Link from 'next/link';
import { useQuery } from '@tanstack/react-query';

import { Panel } from '@/components/reveal';
import { Stat } from '@/components/ui';
import { apiGet, asExample, type Envelope } from '@/api/client';
import { formatCompact } from '@/lib/format';

import { formatTimestamp, stroopsToXlm } from '../explorer-shared';
import { RELATION, type Relation } from './accountRelation';

// The board endpoints cap a page at 500 rows and take no account filter,
// so a rank lookup is "fetch the top 500 and look". That is a real reach
// limit, not a rendering choice: 500 of 2,427 sponsors and 500 of
// 955,023 creators. The panel states which side of it the address fell
// on rather than showing an absent rank as "unranked".
const BOARD_LIMIT = 500;

interface CreatorRow {
  rank: number;
  account: string;
  accounts_created: number;
  funded_stroops: string;
  live_accounts: number;
  live_stroops: string;
}

interface SponsorRow {
  rank: number;
  account: string;
  sponsorships_started: number;
  distinct_sponsored: number;
  revocations_issued: number;
}

interface CreatorsResp {
  creators: CreatorRow[];
  totals: { creators: number };
  computed_at: string;
}

interface SponsorsResp {
  sponsors: SponsorRow[];
  totals: { sponsors: number };
  computed_at: string;
}

const numFmt = new Intl.NumberFormat('en-US');

/**
 * AccountRelationStanding — where this address sits on its board, and
 * (for a creator) what the set it created holds TODAY.
 *
 * It deliberately shows only what /v1/accounts/{g}/graph cannot. The
 * graph serves every count on this page exactly, for any address; the
 * board adds two things it does not have — a rank against every other
 * account, and `live_accounts` / `live_stroops`, the point-in-time
 * survival and native balance of the created cohort. Restating the
 * graph's own figures here would give a reader two sources for one
 * number, computed by two cycles, free to disagree by a cycle's age.
 *
 * The live figures are the answer to "what does the cohort hold" the
 * API actually has. It is NATIVE XLM only — there is no per-cohort
 * trustline aggregate on any endpoint — and the copy says so rather
 * than letting an XLM figure pass for a portfolio.
 */
export function AccountRelationStanding({
  account,
  relation,
}: {
  account: string;
  relation: Relation;
}) {
  const creation = relation === 'created';
  const path = creation ? '/v1/accounts/creators' : '/v1/accounts/sponsors';
  const vocabulary = RELATION[relation];
  const source = asExample(path, { limit: BOARD_LIMIT });

  const { data, isLoading, isError } = useQuery<CreatorsResp | SponsorsResp>({
    queryKey: [path, BOARD_LIMIT],
    retry: false,
    staleTime: 10 * 60_000,
    queryFn: async () => {
      const env = await apiGet<Envelope<CreatorsResp | SponsorsResp>>(path, {
        limit: BOARD_LIMIT,
      });
      return env.data;
    },
  });

  const title = 'Board standing';

  if (isLoading) {
    return (
      <Panel
        headingLevel={2}
        title={title}
        source={source}
        bodyClassName="text-sm text-ink-muted"
      >
        Loading the board…
      </Panel>
    );
  }
  if (isError || !data) {
    return (
      <Panel
        headingLevel={2}
        title={title}
        source={source}
        bodyClassName="text-sm text-ink-muted"
      >
        The {vocabulary.boardLabel.toLowerCase()} board is warming — it is
        rebuilt by a rollup cycle, and this deployment has not completed one
        yet.
      </Panel>
    );
  }

  const rows: (CreatorRow | SponsorRow)[] = creation
    ? ((data as CreatorsResp).creators ?? [])
    : ((data as SponsorsResp).sponsors ?? []);
  const population = creation
    ? (data as CreatorsResp).totals?.creators
    : (data as SponsorsResp).totals?.sponsors;
  const row = rows.find((r) => r.account === account);

  return (
    <Panel
      headingLevel={2}
      title={title}
      source={source}
      bodyClassName="space-y-3"
    >
      {row ? (
        <dl className="grid grid-cols-2 gap-x-6 gap-y-3 sm:grid-cols-4">
          <Stat
            label="Rank"
            value={`#${numFmt.format(row.rank)}`}
            sub={
              population != null
                ? `of ${numFmt.format(population)} ${creation ? 'creators' : 'sponsors'}`
                : undefined
            }
          />
          {creation && (
            <>
              <Stat
                label="Created set still live"
                value={numFmt.format((row as CreatorRow).live_accounts)}
                sub={survival(row as CreatorRow)}
              />
              <Stat
                label="XLM the set holds now"
                value={stroopsToXlm((row as CreatorRow).live_stroops)}
                sub="native only"
              />
            </>
          )}
          {!creation && (
            <Stat
              label="Revocations issued"
              value={numFmt.format((row as SponsorRow).revocations_issued)}
              sub="a lower bound on arrangements ended"
            />
          )}
        </dl>
      ) : (
        <p className="text-ink-muted text-sm">
          This address is not in the top {numFmt.format(BOARD_LIMIT)} of the{' '}
          <Link
            href={`${vocabulary.board}/`}
            className="underline decoration-dotted"
          >
            {vocabulary.boardLabel.toLowerCase()} board
          </Link>
          {population != null && (
            <>
              {' '}
              (of {numFmt.format(population)}
              {creation ? ' creators' : ' sponsors'})
            </>
          )}
          , which is as deep as the board endpoint pages. Its rank is therefore
          unknown here rather than low — every other figure on this page is
          exact for this address and does not depend on the board.
        </p>
      )}

      <p className="text-ink-faint text-[11px]">
        Rollup snapshot computed {formatTimestamp(data.computed_at)}.
        {creation && (
          <>
            {' '}
            The two live figures are point-in-time as of that snapshot, not
            history: created accounts merge away and balances move. &quot;XLM
            the set holds now&quot; is the <strong>native</strong> balance of
            the surviving accounts only — no endpoint aggregates a created
            cohort&apos;s trustlines, so this is not the cohort&apos;s
            portfolio.
          </>
        )}
      </p>
    </Panel>
  );
}

function survival(row: CreatorRow): string | undefined {
  if (row.accounts_created === 0) return undefined;
  return `${((row.live_accounts / row.accounts_created) * 100).toFixed(1)}% of ${formatCompact(row.accounts_created)}`;
}
