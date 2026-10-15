'use client';

import { useMemo, useState } from 'react';
import Link from 'next/link';

import { Panel } from '@/components/reveal';
import { asExample } from '@/api/client';
import { InlineBar } from '@/components/ContractCharts';
import {
  Badge,
  Button,
  EmptyState,
  Input,
  Skeleton,
  TBody,
  TR,
  Table,
  Td,
  Th,
  THead,
  LinkRow,
} from '@/components/ui';
import { useIssuers, type IssuerListEntry } from '@/api/hooks';
import { formatCompact, truncateMiddle } from '@/lib/format';
import { useTableSort, SortableTh, type SortColumn } from '@/lib/useTableSort';
import { isSafeHomeDomain } from '@/lib/safe-domain';
import { classifyScamReasonLabel } from '@/lib/directory-tags';

const LIMIT = 100;
const SOURCE = asExample('/v1/issuers', { limit: LIMIT });

type SortKey = 'org' | 'assets' | 'observations';

const SORT_COLUMNS: SortColumn<IssuerListEntry, SortKey>[] = [
  { key: 'org', value: (r) => r.org_name ?? '', initialDir: 'asc' },
  { key: 'assets', value: (r) => r.asset_count },
  { key: 'observations', value: (r) => r.total_observation_count },
];

/**
 * IssuersTable — the top issuers by observations across their classic
 * assets, from /v1/issuers. The API's ranking holds until a header is
 * clicked; the text filter matches organisation, domain and account.
 */
export function IssuersTable() {
  const { data, isLoading, isError, refetch } = useIssuers(LIMIT);
  const [filter, setFilter] = useState('');
  const query = filter.trim().toLowerCase();
  const filtered = useMemo(() => {
    if (!query) return data ?? [];
    return (data ?? []).filter((row) =>
      `${row.org_name ?? ''} ${row.home_domain ?? ''} ${row.g_strkey}`
        .toLowerCase()
        .includes(query),
    );
  }, [data, query]);
  // Rank is the directory's own order (by observations), not the row's
  // position after a sort or filter.
  const rankOf = useMemo(
    () => new Map((data ?? []).map((r, i) => [r.g_strkey, i + 1])),
    [data],
  );
  const maxObservations = Math.max(
    0,
    ...filtered.map((r) => r.total_observation_count),
  );
  const { sorted, sort, toggle, ariaSort } = useTableSort<
    IssuerListEntry,
    SortKey
  >(filtered, SORT_COLUMNS, null);

  return (
    <Panel
      headingLevel={2}
      title="Issuers"
      hint="Top 100 by observations across each issuer’s classic assets"
      source={SOURCE}
      bodyClassName="-mx-4 -mb-4"
    >
      {isError ? (
        <div className="px-4 pb-4">
          <EmptyState
            title="Issuer list unavailable"
            description="The issuers query did not return. Nothing is cached, so retry."
            action={
              <Button variant="secondary" size="sm" onClick={() => refetch()}>
                Retry
              </Button>
            }
          />
        </div>
      ) : !isLoading && data && data.length === 0 ? (
        <div className="px-4 pb-4">
          <EmptyState
            title="No issuers yet"
            description="No account has issued a classic asset seen in a trade or trustline change the index has captured."
          />
        </div>
      ) : (
        <>
          <div className="mx-4 mb-4 flex flex-wrap items-center gap-3">
            <Input
              type="search"
              aria-label="Filter issuers by name, domain, or account"
              placeholder="Filter by name, domain, or account"
              value={filter}
              onChange={(e) => setFilter(e.target.value)}
              className="max-w-xs"
            />
            {query && (
              <Button variant="ghost" size="sm" onClick={() => setFilter('')}>
                Clear
              </Button>
            )}
            <span className="text-ink-muted tnum ml-auto text-xs">
              {data ? `${filtered.length} of ${data.length}` : '—'}
            </span>
          </div>
          <div className="overflow-x-auto">
            <Table>
              <THead>
                <tr>
                  <Th className="w-10">#</Th>
                  <SortableTh
                    label="Organisation"
                    sortKey="org"
                    sort={sort}
                    onSort={toggle}
                    ariaSort={ariaSort}
                  />
                  <Th>Account</Th>
                  <SortableTh
                    label="Assets"
                    sortKey="assets"
                    sort={sort}
                    onSort={toggle}
                    ariaSort={ariaSort}
                    align="right"
                  />
                  <SortableTh
                    label="Observations"
                    sortKey="observations"
                    sort={sort}
                    onSort={toggle}
                    ariaSort={ariaSort}
                    align="right"
                  />
                </tr>
              </THead>
              <TBody>
                {isLoading || !data ? (
                  Array.from({ length: 8 }, (_, i) => (
                    <TR key={i}>
                      <Td colSpan={5}>
                        <Skeleton className="h-5 w-full max-w-md" />
                      </Td>
                    </TR>
                  ))
                ) : sorted.length === 0 ? (
                  <TR>
                    <Td colSpan={5} className="text-ink-muted">
                      No issuers match “{filter.trim()}”.
                    </Td>
                  </TR>
                ) : (
                  sorted.map((row, i) => (
                    <IssuerRow
                      key={row.g_strkey}
                      row={row}
                      rank={rankOf.get(row.g_strkey) ?? i + 1}
                      max={maxObservations}
                    />
                  ))
                )}
              </TBody>
            </Table>
          </div>
        </>
      )}
    </Panel>
  );
}

function IssuerRow({
  row,
  rank,
  max,
}: {
  row: IssuerListEntry;
  rank: number;
  max: number;
}) {
  const href = `/issuers/${encodeURIComponent(row.g_strkey)}`;
  const safeDomain = isSafeHomeDomain(row.home_domain);
  return (
    <LinkRow href={href}>
      <Td className="text-ink-faint font-mono tabular-nums">{rank}</Td>
      <Td>
        <div className="flex flex-wrap items-center gap-2 whitespace-nowrap">
          {row.org_name ? (
            <Link
              href={href}
              className="text-ink hover:text-brand-600 font-medium"
            >
              {row.org_name}
            </Link>
          ) : (
            <span className="text-ink-faint">—</span>
          )}
          {row.org_verified && (
            <Badge
              tone="ok"
              dot
              title="The organisation’s own stellar.toml lists this account as an issuer"
            >
              Verified
            </Badge>
          )}
          {row.scam_reason && (
            <Badge tone="bad" title={row.scam_reason}>
              {classifyScamReasonLabel(row.scam_reason)}
            </Badge>
          )}
        </div>
        {row.home_domain && (
          <div className="mt-0.5 text-xs">
            {safeDomain ? (
              <a
                href={`https://${row.home_domain}`}
                target="_blank"
                rel="noreferrer noopener nofollow"
                className="text-ink-muted hover:text-brand-600 hover:underline"
              >
                {row.home_domain}
              </a>
            ) : (
              <span
                className="text-ink-muted"
                title="Not linked: this account-supplied domain is not a plain hostname"
              >
                {row.home_domain}
              </span>
            )}
          </div>
        )}
      </Td>
      <Td className="whitespace-nowrap">
        <Link
          href={href}
          className="text-ink-muted hover:text-brand-600 font-mono text-xs"
          title={row.g_strkey}
        >
          {truncateMiddle(row.g_strkey, 6, 4)}
        </Link>
      </Td>
      <Td align="right" className="font-mono tabular-nums">
        {row.asset_count}
      </Td>
      <Td align="right" className="font-mono tabular-nums">
        <div className="flex items-center justify-end gap-3 whitespace-nowrap">
          <InlineBar
            value={row.total_observation_count}
            max={max}
            label={`${row.total_observation_count.toLocaleString('en-US')} observations`}
          />
          <span className="inline-block w-14 text-right">
            {formatCompact(row.total_observation_count)}
          </span>
        </div>
      </Td>
    </LinkRow>
  );
}
