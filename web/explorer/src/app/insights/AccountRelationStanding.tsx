'use client';

import Link from 'next/link';
import { useQuery } from '@tanstack/react-query';

import { Panel } from '@/components/reveal';
import { Stat } from '@/components/ui';
import { apiGet, asExample, type Envelope } from '@/api/client';
import type { operations } from '@/api/types';
import { formatCompact } from '@/lib/format';

import { formatTimestamp, stroopsToXlm } from '../explorer-shared';
import { RELATION, type Relation } from './accountRelation';

// The board endpoints take `?account=`, a keyed read that returns one
// row with its whole-aggregation rank. That is why this panel asks for
// one address rather than paging: rank is a property of all 955,023
// creators and 2,427 sponsors, and pulling a 500-row page to look for
// one address would leave every address past the cap indistinguishable
// from one that never appears at all.
//
// An empty result is therefore unambiguous here — it means this address
// holds no row on this board, not that it fell outside a page.

// Both board bodies are derived from the generated OpenAPI contract
// (src/api/types.ts, `make web-generate-api`) rather than restated here:
// a hand-written copy matches the wire today and is free to drift from it
// the day the spec moves.
type CreatorsResp = NonNullable<
  operations['getAccountCreators']['responses'][200]['content']['application/json']['data']
>;
type SponsorsResp = NonNullable<
  operations['getAccountSponsors']['responses'][200]['content']['application/json']['data']
>;
type CreatorRow = CreatorsResp['creators'][number];
type SponsorRow = SponsorsResp['sponsors'][number];

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
  const source = asExample(path, { account });

  const { data, isLoading, isError } = useQuery<CreatorsResp | SponsorsResp>({
    queryKey: [path, 'account', account],
    retry: false,
    staleTime: 10 * 60_000,
    queryFn: async () => {
      const env = await apiGet<Envelope<CreatorsResp | SponsorsResp>>(path, {
        account,
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

  // The two bodies are told apart by the shape the wire actually served
  // — the board that answered — rather than by the path this panel asked
  // for, so a body of the other kind cannot be read through the wrong
  // field names.
  const board = 'creators' in data ? data.creators : data.sponsors;
  const population =
    'creators' in data ? data.totals.creators : data.totals.sponsors;
  // The keyed read returns at most this address's row, but the filter is
  // still checked rather than assumed: a deployment whose API predates
  // `?account=` ignores the parameter and serves the default page, and
  // taking rows[0] there would publish the top-ranked account's rank as
  // this address's.
  const row: CreatorRow | SponsorRow | undefined = board.find(
    (r) => r.account === account,
  );

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
          {'live_accounts' in row && (
            <>
              <Stat
                label="Created set still live"
                value={numFmt.format(row.live_accounts)}
                sub={survival(row)}
              />
              <Stat
                label="XLM the set holds now"
                value={stroopsToXlm(row.live_stroops)}
                sub="native only"
              />
            </>
          )}
          {'revocations_issued' in row && (
            <Stat
              label="Revocations issued"
              value={numFmt.format(row.revocations_issued)}
              sub="a lower bound on arrangements ended"
            />
          )}
        </dl>
      ) : (
        <p className="text-ink-muted text-sm">
          This address holds no row on the{' '}
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
          , as of the snapshot below. Every other figure on this page is exact
          for this address and does not depend on the board.
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
