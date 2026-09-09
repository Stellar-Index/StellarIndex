'use client';

import { useMemo, useState } from 'react';
import Link from 'next/link';

import { Panel } from '@/components/reveal';
import { asExample } from '@/api/client';
import { useIssuers } from '@/api/hooks';
import { formatCompact } from '@/lib/format';
import { useTableSort, SortableTh, type SortColumn } from '@/lib/useTableSort';
import { isSafeHomeDomain } from '@/lib/safe-domain';

/**
 * Live issuer directory backed by `/v1/issuers`. Ranked by total
 * observation count across the issuer's classic assets — the
 * proxy-for-activity ordering the API serves.
 *
 * G-strkey column deep-links to /issuers/[g_strkey] — the
 * dedicated detail view. /assets?issuer=... is also reachable
 * via "View assets" from there.
 */
export function IssuersTable() {
  const { data, isLoading, isError, error } = useIssuers(100);
  const [filter, setFilter] = useState('');

  const filtered = useMemo(() => {
    const q = filter.trim().toLowerCase();
    if (!q) return data ?? [];
    return (data ?? []).filter((row) => {
      const hay =
        `${row.org_name ?? ''} ${row.home_domain ?? ''} ${row.g_strkey}`.toLowerCase();
      return hay.includes(q);
    });
  }, [data, filter]);

  // Sortable columns (site-audit S36). Default keeps the API's
  // observation-count ranking until a header is clicked.
  const issuerSortColumns: SortColumn<(typeof filtered)[number], string>[] = [
    { key: 'org', value: (r) => r.org_name ?? '', initialDir: 'asc' },
    { key: 'domain', value: (r) => r.home_domain ?? '', initialDir: 'asc' },
    { key: 'assets', value: (r) => r.asset_count },
    { key: 'observations', value: (r) => r.total_observation_count },
  ];
  const {
    sorted: sortedIssuers,
    sort,
    toggle,
    ariaSort,
  } = useTableSort<(typeof filtered)[number], string>(
    filtered,
    issuerSortColumns,
    null,
  );
  if (isError) {
    return (
      <Panel
        headingLevel={2}
        title="Issuers"
        source={asExample('/v1/issuers', { limit: 100 })}
        bodyClassName="text-sm text-down-strong"
      >
        Failed to load issuers:{' '}
        {error instanceof Error ? error.message : 'unknown error'}
      </Panel>
    );
  }
  if (isLoading || !data) {
    return (
      <Panel
        headingLevel={2}
        title="Issuers"
        source={asExample('/v1/issuers', { limit: 100 })}
        bodyClassName="text-sm text-ink-muted"
      >
        Loading…
      </Panel>
    );
  }
  if (data.length === 0) {
    return (
      <Panel
        headingLevel={2}
        title="Issuers"
        source={asExample('/v1/issuers', { limit: 100 })}
        bodyClassName="text-sm text-ink-muted"
      >
        No issuers recorded yet.
      </Panel>
    );
  }

  return (
    <Panel
      headingLevel={2}
      title={`${data.length} top issuers`}
      hint="Ranked by total observation count across each issuer's assets"
      source={asExample('/v1/issuers', { limit: 100 })}
      bodyClassName="-mx-4"
    >
      <div className="px-4 pt-1 pb-3">
        <div className="flex flex-wrap items-center gap-3 text-xs">
          <input
            type="search"
            aria-label="Filter issuers by name, domain, or G-strkey"
            placeholder="Filter by name, domain, or G-strkey…"
            value={filter}
            onChange={(e) => setFilter(e.target.value)}
            className="border-line bg-surface placeholder:text-ink-faint focus:border-brand-500 focus:ring-brand-500 w-72 rounded-md border px-2.5 py-1 text-xs focus:ring-1 focus:outline-hidden"
          />
          <span className="text-ink-muted font-mono text-[11px]">
            {filtered.length} of {data.length} rows
            {filter && (
              <button
                type="button"
                onClick={() => setFilter('')}
                className="text-brand-600 ml-2 hover:underline"
              >
                clear
              </button>
            )}
          </span>
        </div>
      </div>
      <div className="overflow-x-auto">
        <table className="divide-line min-w-full divide-y text-sm">
          <thead>
            <tr className="text-ink-muted text-left text-[11px] tracking-wider uppercase">
              <Th>#</Th>
              <SortableTh
                label="Organisation"
                sortKey="org"
                sort={sort}
                onSort={toggle}
                ariaSort={ariaSort}
              />
              <Th>G-strkey</Th>
              <SortableTh
                label="Home domain"
                sortKey="domain"
                sort={sort}
                onSort={toggle}
                ariaSort={ariaSort}
              />
              <SortableTh
                label="Assets"
                sortKey="assets"
                sort={sort}
                onSort={toggle}
                ariaSort={ariaSort}
                align="right"
              />
              <SortableTh
                label="Total observations"
                sortKey="observations"
                sort={sort}
                onSort={toggle}
                ariaSort={ariaSort}
                align="right"
              />
            </tr>
          </thead>
          <tbody className="divide-line-subtle divide-y">
            {filtered.length === 0 && filter && (
              <tr>
                <td
                  colSpan={6}
                  className="text-ink-muted px-4 py-8 text-center text-sm"
                >
                  No issuers match &quot;{filter}&quot;.
                </td>
              </tr>
            )}
            {sortedIssuers.map((row, i) => (
              <tr key={row.g_strkey} className="hover:bg-surface-muted">
                <Td>
                  <span className="text-ink-faint">{i + 1}</span>
                </Td>
                <Td>
                  <div className="flex items-center gap-1.5">
                    {row.org_name ? (
                      <Link
                        href={`/issuers/${row.g_strkey}`}
                        className="hover:text-brand-600 font-medium"
                      >
                        {row.org_name}
                      </Link>
                    ) : (
                      <span className="text-ink-faint text-xs">—</span>
                    )}
                    {row.org_verified && row.org_name && (
                      <span
                        title="SEP-1 verified — the organisation's stellar.toml lists this issuer back (bidirectional)"
                        className="bg-up-subtle text-up-strong rounded-sm px-1.5 py-0.5 text-[9px] font-medium tracking-wider uppercase"
                      >
                        ✓ Verified
                      </span>
                    )}
                    {row.scam_reason && (
                      <span
                        title={row.scam_reason}
                        className="bg-down-subtle text-down rounded-sm px-1.5 py-0.5 text-[9px] font-medium tracking-wider uppercase"
                      >
                        {/* S-010: the flag categories differ materially —
                            a deprecated legacy issuer of a real org is not
                            a scam. Derive the label from the reason. */}
                        {/^deprecated/i.test(row.scam_reason)
                          ? 'DEPRECATED'
                          : /scam|counterfeit|fraud/i.test(row.scam_reason)
                            ? 'SCAM'
                            : 'UNSAFE'}
                      </span>
                    )}
                  </div>
                </Td>
                <Td>
                  <Link
                    href={`/issuers/${row.g_strkey}`}
                    className="hover:text-brand-600 font-mono text-xs"
                    title={row.g_strkey}
                  >
                    {row.g_strkey.slice(0, 8)}…{row.g_strkey.slice(-4)}
                  </Link>
                </Td>
                <Td>
                  {isSafeHomeDomain(row.home_domain) ? (
                    <a
                      href={`https://${row.home_domain}`}
                      target="_blank"
                      rel="noreferrer noopener nofollow"
                      className="hover:text-brand-600 text-xs hover:underline"
                    >
                      {row.home_domain}
                    </a>
                  ) : row.home_domain ? (
                    // Attacker-controlled on-chain value that doesn't
                    // parse as a strict hostname — render as plain
                    // text, never a clickable link (phishing guard).
                    <span
                      className="text-ink-muted text-xs"
                      title="Unverified issuer-supplied domain"
                    >
                      {row.home_domain}
                    </span>
                  ) : (
                    <span className="text-ink-faint text-xs">—</span>
                  )}
                </Td>
                <Td align="right">
                  <span className="font-mono tabular-nums">
                    {row.asset_count}
                  </span>
                </Td>
                <Td align="right">
                  <span className="font-mono tabular-nums">
                    {formatCompact(row.total_observation_count)}
                  </span>
                </Td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    </Panel>
  );
}

function Th({
  children,
  align,
}: {
  children: React.ReactNode;
  align?: 'left' | 'right';
}) {
  return (
    <th
      className={`px-4 py-2 ${align === 'right' ? 'text-right' : 'text-left'}`}
      scope="col"
    >
      {children}
    </th>
  );
}

function Td({
  children,
  align,
}: {
  children: React.ReactNode;
  align?: 'left' | 'right';
}) {
  return (
    <td
      className={`px-4 py-3 ${align === 'right' ? 'text-right' : 'text-left'}`}
    >
      {children}
    </td>
  );
}
