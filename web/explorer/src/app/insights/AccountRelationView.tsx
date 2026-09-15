'use client';

import Link from 'next/link';
import { useQuery } from '@tanstack/react-query';

import { Panel } from '@/components/reveal';
import { Breadcrumbs, Container, Stat } from '@/components/ui';
import { StellarExpertLink } from '@/components/StellarExpertLink';
import { apiGet, asExample, type Envelope } from '@/api/client';
import { truncateMiddle } from '@/lib/format';
import type { components } from '@/api/types';

import { AccountPositions } from '../accounts/AccountPositions';
import { formatTimestamp, stroopsToXlm } from '../explorer-shared';
import { AccountRelationEdges } from './AccountRelationEdges';
import { AccountRelationHistory } from './AccountRelationHistory';
import { AccountRelationStanding } from './AccountRelationStanding';
import {
  errorStatus,
  isAccountId,
  RELATION,
  type Relation,
} from './accountRelation';

type AccountGraphResp = components['schemas']['AccountGraph'];

const numFmt = new Intl.NumberFormat('en-US');

/**
 * AccountRelationView — one address, seen through ONE of the two graph
 * relations: /insights/creators/{g} and /insights/sponsors/{g}.
 *
 * WHY TWO PAGES AND NOT ONE. Creation and sponsorship are different
 * kinds of fact over different spans — creation is immutable and reaches
 * genesis, sponsorship is revocable and only reaches protocol 14 — and
 * the boards that lead here ask different questions. A single page would
 * have to hedge every sentence to cover both. Each page therefore leads
 * with its own relation and links across to the other when the address
 * has activity there, so the pair is never presented as one number.
 *
 * WHY A CLIENT FETCH. This is a static export, and the creator
 * population alone is 955,023 — no generateStaticParams can enumerate
 * it. The build emits one `shell` document per board (the same pattern
 * as /accounts/[g], /issuers/[g_strkey], /contracts/[id]) and the
 * address is read from the URL at runtime.
 */
export function AccountRelationView({
  account,
  relation,
}: {
  account: string;
  relation: Relation;
}) {
  const vocabulary = RELATION[relation];
  const creation = relation === 'created';
  const valid = isAccountId(account);

  // The endpoint's BOUNDED default: no ?relation=, so it serves the
  // summaries and the capped inbound edges and none of the unbounded
  // outbound list. The edge table below asks for its own pages.
  const { data, isLoading, isError, error } = useQuery<AccountGraphResp>({
    queryKey: ['/v1/accounts/{id}/graph', account, '', ''],
    enabled: valid,
    retry: false,
    staleTime: 60_000,
    queryFn: async () => {
      const env = await apiGet<Envelope<AccountGraphResp>>(
        `/v1/accounts/${encodeURIComponent(account)}/graph`,
      );
      return env.data;
    },
  });

  const shortKey = valid ? truncateMiddle(account, 10, 6) : account;
  const crumbs = (
    <Breadcrumbs
      items={[
        { label: 'Home', href: '/' },
        { label: 'Insights', href: '/insights' },
        { label: vocabulary.boardLabel, href: vocabulary.board },
        { label: shortKey || 'Account' },
      ]}
    />
  );

  if (!valid) {
    return (
      <Container className="space-y-6 py-8">
        <header className="space-y-3">
          {crumbs}
          <h1 className="text-2xl font-semibold tracking-tight">
            Not a Stellar account
          </h1>
        </header>
        <Panel
          headingLevel={2}
          title="Unreadable address"
          bodyClassName="text-sm text-ink-body space-y-2"
        >
          <p>
            {account ? (
              <>
                <span className="font-mono break-all">{account}</span> is not a
                Stellar account id.
              </>
            ) : (
              <>No account id was given in the URL.</>
            )}{' '}
            An account id is 56 characters: a leading <code>G</code> followed by
            55 uppercase base32 characters. They are case-sensitive and are
            never lowercased.
          </p>
          <p>
            <Link
              href={`${vocabulary.board}/`}
              className="text-brand-600 hover:underline"
            >
              Back to the {vocabulary.boardLabel.toLowerCase()} board →
            </Link>
          </p>
        </Panel>
      </Container>
    );
  }

  const side = data
    ? creation
      ? data.outbound.created
      : data.outbound.sponsored
    : null;
  const inbound = data
    ? creation
      ? data.inbound.created_by
      : data.inbound.sponsored_by
    : null;
  const coverage = data
    ? creation
      ? data.coverage.creation
      : data.coverage.sponsorship
    : null;
  // The sibling relation, offered only when the address is actually
  // active there — an "also a sponsor" link to a page of zeroes would
  // be a worse answer than no link.
  const otherRelation: Relation = creation ? 'sponsored' : 'created';
  const otherCount = data
    ? creation
      ? data.outbound.sponsored.accounts
      : data.outbound.created.accounts
    : 0;

  return (
    <Container className="space-y-6 py-8">
      <header className="space-y-3">
        {crumbs}
        <p className="text-ink-muted text-[11px] tracking-wider uppercase">
          {vocabulary.boardLabel}
        </p>
        <h1 className="font-mono text-2xl font-semibold tracking-tight break-all">
          {account}
        </h1>
        <div className="text-ink-body flex flex-wrap items-center gap-x-4 gap-y-1 text-sm">
          <Link
            href={`/accounts/${encodeURIComponent(account)}/`}
            className="text-brand-600 hover:underline"
          >
            Full account page →
          </Link>
          {otherCount > 0 && (
            <Link
              href={`${RELATION[otherRelation].board}/${encodeURIComponent(account)}/`}
              className="text-brand-600 hover:underline"
            >
              Also a {RELATION[otherRelation].actor} of{' '}
              {numFmt.format(otherCount)} accounts →
            </Link>
          )}
          <StellarExpertLink
            kind="account"
            id={account}
            className="text-ink-muted hover:text-brand-600 hover:underline"
          >
            stellar.expert ↗
          </StellarExpertLink>
        </div>
      </header>

      {isLoading && (
        <Panel
          headingLevel={2}
          title={`This address as a ${vocabulary.actor}`}
          bodyClassName="text-sm text-ink-muted"
        >
          Loading the graph…
        </Panel>
      )}

      {isError && (
        <Panel
          headingLevel={2}
          title={`This address as a ${vocabulary.actor}`}
          bodyClassName="text-sm text-ink-muted"
        >
          {errorStatus(error) === 503
            ? 'The sponsorship/creation graph is warming — it is rebuilt by a rollup cycle, and this deployment has not completed one yet.'
            : `The graph could not be read${error instanceof Error ? `: ${error.message}` : ''}.`}
        </Panel>
      )}

      {data && side && inbound && coverage && (
        <>
          <Panel
            headingLevel={2}
            title={`This address as a ${vocabulary.actor}`}
            source={asExample(`/v1/accounts/${account}/graph`)}
            bodyClassName="space-y-4"
          >
            <dl className="grid grid-cols-2 gap-x-6 gap-y-4 sm:grid-cols-4">
              <Stat
                label={creation ? 'Accounts created' : 'Accounts sponsored'}
                value={numFmt.format(side.accounts)}
              />
              <Stat
                label={creation ? 'Creations' : 'Sponsorships started'}
                value={numFmt.format(
                  creation
                    ? data.outbound.created.creations
                    : data.outbound.sponsored.sponsorships_started,
                )}
                sub={
                  creation
                    ? 'operations, not addresses'
                    : 'arrangements begun, never held'
                }
              />
              {creation ? (
                <Stat
                  label="XLM funded"
                  value={
                    side.accounts > 0
                      ? stroopsToXlm(data.outbound.created.funded_stroops)
                      : '—'
                  }
                  sub="starting balances paid"
                />
              ) : (
                <Stat
                  label="Revocations issued"
                  value={numFmt.format(
                    data.outbound.sponsored.revocations_issued,
                  )}
                  sub="account-level, not per edge"
                />
              )}
              {/* The active SPAN is prose under the grid, not a fifth
                  tile: a formatted UTC instant is 23 characters and a
                  metric tile is sized for a number. */}
            </dl>

            {side.first_at && side.last_at && (
              <p className="text-ink-muted text-xs">
                Active from {formatTimestamp(side.first_at)} to{' '}
                {formatTimestamp(side.last_at)}
                {side.first_ledger != null && side.last_ledger != null && (
                  <>
                    {' '}
                    (ledgers {numFmt.format(side.first_ledger)}–
                    {numFmt.format(side.last_ledger)})
                  </>
                )}
                .
              </p>
            )}

            <div>
              <div className="text-ink-muted text-[11px] tracking-wider uppercase">
                {creation ? 'Created by' : 'Sponsored by'}
              </div>
              {inbound.edges.length === 0 ? (
                <p className="text-ink-muted mt-1 text-sm">
                  {creation
                    ? 'No CreateAccount operation for this address is indexed — it may predate the coverage below, or the address may never have existed.'
                    : 'No account has begun a sponsorship arrangement covering this one.'}
                </p>
              ) : (
                <ul className="mt-1 space-y-1">
                  {inbound.edges.map((e) => (
                    <li
                      key={e.account}
                      className="flex flex-wrap items-baseline gap-x-2 text-xs"
                    >
                      <Link
                        href={`/accounts/${encodeURIComponent(e.account)}/`}
                        className="text-brand-600 font-mono hover:underline"
                        title={e.account}
                      >
                        {truncateMiddle(e.account, 10, 6)}
                      </Link>
                      <span className="text-ink-faint">
                        {formatTimestamp(e.first_at)}
                      </span>
                    </li>
                  ))}
                </ul>
              )}
              {inbound.truncated && (
                <p className="text-ink-faint mt-1 text-[11px]">
                  Showing {numFmt.format(inbound.edges.length)} of{' '}
                  {numFmt.format(inbound.total)}.
                </p>
              )}
            </div>

            <p className="text-ink-faint text-[11px]">{data.note}</p>
            <p className="text-ink-faint text-[11px]">
              {creation ? 'Creation' : 'Sponsorship'} history covers ledgers{' '}
              {numFmt.format(coverage.from_ledger)}–
              {numFmt.format(coverage.thru_ledger)} (
              {formatTimestamp(coverage.from_time)} to{' '}
              {formatTimestamp(coverage.thru_time)})
              {!creation &&
                ', whose floor is where sponsorship began to exist on the network, not a gap'}
              .
            </p>
          </Panel>

          <AccountRelationStanding account={account} relation={relation} />

          <AccountRelationHistory account={account} relation={relation} />

          <AccountRelationEdges
            account={account}
            relation={relation}
            total={side.accounts}
          />
        </>
      )}

      {/* Assets this address itself holds — native XLM plus every
          trustline, valued at the live VWAP. Reused verbatim from the
          account page rather than reimplemented: one portfolio reader,
          one set of pricing rules. */}
      <AccountPositions id={account} />
    </Container>
  );
}
