'use client';

import Link from 'next/link';

import { Panel } from '@/components/reveal';
import { asExample } from '@/api/client';
import { useIssuer, type Issuer } from '@/api/hooks';
import { formatCompact } from '@/lib/format';

/**
 * IssuerPanel — backs the "Issuer" tab on /assets/[slug]. Fetches
 * the live issuer row + embedded assets from /v1/issuers/{g_strkey}
 * so users see the full directory of assets a single issuer has
 * minted (USDC issuer alone covers ~20 distinct codes).
 *
 * Auth flag pills surface the SEP-1 / on-chain account flags that
 * matter most for asset risk: `auth_required` (issuer can refuse
 * trustlines), `auth_revocable` (issuer can freeze), `auth_clawback`
 * (issuer can claw back balances).
 */
export function IssuerPanel({ gStrkey }: { gStrkey: string }) {
  const { data, isLoading, isError, error } = useIssuer(gStrkey);

  if (isError) {
    return (
      <Panel
        headingLevel={2}
        title="Issuer"
        source={asExample(`/v1/issuers/${gStrkey}`)}
        bodyClassName="text-sm text-ink-muted"
      >
        {is404(error)
          ? 'No issuer record yet — this G-strkey hasn’t been observed as an issuer.'
          : `Failed to load issuer: ${error instanceof Error ? error.message : 'unknown error'}`}
      </Panel>
    );
  }
  if (isLoading || !data) {
    return (
      <Panel
        headingLevel={2}
        title="Issuer"
        source={asExample(`/v1/issuers/${gStrkey}`)}
        bodyClassName="text-sm text-ink-muted"
      >
        Loading…
      </Panel>
    );
  }

  return (
    <div className="space-y-4">
      {data.scam_reason && (
        // Warning consolidation (2026-08-25): the full scam reason + guidance
        // live in ONE primary banner in the page header (same stellar.expert
        // finding). This is the slim, table-scoped restatement — it adds the
        // one fact the header can't (every asset in the table below shares
        // this flagged issuer) and stays SELF-SUFFICIENT: a standalone alert,
        // not a pointer to the header, because the two warnings are sourced
        // from different endpoints (/v1/issuers here vs the asset-detail
        // payload) and must not degrade to a dangling "see above" if they
        // ever diverge.
        <div
          role="alert"
          className="flex items-center gap-2 rounded-md border border-bad-300 bg-bad-50 px-3 py-2 text-xs text-bad-700"
        >
          <span aria-hidden>⚠</span>
          <span>
            <strong className="font-semibold">Scam issuer.</strong> Every asset
            below was minted by this flagged account — do not establish
            trustlines or trust its prices.
          </span>
        </div>
      )}
      <Panel
        headingLevel={2}
        title="Issuer identity"
        // CS-100: org_name is only an authoritative organisation identity
        // when SEP-1 verified (bidirectional). Unverified it is self-declared
        // metadata a scam issuer can spoof, so it must not headline the panel
        // as the issuer's identity — fall back to the real on-chain home_domain.
        hint={(data.org_verified ? data.org_name : undefined) ?? data.home_domain ?? '—'}
        source={asExample(`/v1/issuers/${gStrkey}`)}
      >
        <dl className="grid grid-cols-1 gap-3 text-sm sm:grid-cols-2">
          {/* Only surface the "Organisation" attribution when verified;
              otherwise home_domain (below) carries the real on-chain value
              without laundering an unverified name as authoritative (CS-100). */}
          {data.org_verified && data.org_name && (
            <Stat label="Organisation" value={data.org_name} />
          )}
          <Stat label="G-strkey" mono value={data.g_strkey} />
          {data.home_domain && (
            <Stat label="Home domain" mono value={data.home_domain} />
          )}
          {typeof data.creation_ledger === 'number' && (
            <Stat
              label="Creation ledger"
              mono
              value={`#${data.creation_ledger.toLocaleString('en-US')}`}
            />
          )}
          {data.sep1_resolved_at && (
            <Stat label="SEP-1 resolved" value={data.sep1_resolved_at} />
          )}
        </dl>
        <div className="mt-4 flex flex-wrap gap-1">
          <FlagPill on={data.auth_required ?? undefined} label="auth_required" />
          <FlagPill on={data.auth_revocable ?? undefined} label="auth_revocable" />
          <FlagPill on={data.auth_immutable ?? undefined} label="auth_immutable" />
          <FlagPill on={data.auth_clawback ?? undefined} label="auth_clawback" />
        </div>
        {data.auth_flags_source === 'last_known_before_removal' && (
          // #374: recovered from the account's state at its REMOVAL ledger.
          // Unlabelled pills would read as this issuer's current policy for
          // an account that no longer exists.
          <p className="text-ink-muted mt-2 text-xs">
            Flags are the last known values before this account was removed
            {data.auth_flags_as_of_ledger != null && (
              <> (ledger {data.auth_flags_as_of_ledger.toLocaleString()})</>
            )}
            , not its current policy.
          </p>
        )}
      </Panel>

      <IssuedAssetsTable issuer={data} />
    </div>
  );
}

function IssuedAssetsTable({ issuer }: { issuer: Issuer }) {
  // `assets` is soft-failed to nil server-side when the per-asset
  // fan-out errors or blows its 8s deadline (internal/api/v1/issuers.go)
  // — absent is "we couldn't read the list", not "nothing issued".
  const assets = issuer.assets;
  if (!assets) {
    return (
      <Panel
        headingLevel={2}
        title="Issued assets"
        source={asExample(`/v1/issuers/${issuer.g_strkey}`)}
        bodyClassName="text-sm text-ink-muted"
      >
        Issued-asset list unavailable — the per-asset read didn&apos;t return,
        so this is unknown rather than empty. Retry shortly.
      </Panel>
    );
  }
  if (assets.length === 0) {
    return (
      <Panel
        headingLevel={2}
        title="Issued assets"
        source={asExample(`/v1/issuers/${issuer.g_strkey}`)}
        bodyClassName="text-sm text-ink-muted"
      >
        No issued assets recorded.
      </Panel>
    );
  }

  return (
    <Panel
      headingLevel={2}
      title="Issued assets"
      hint={`${assets.length} asset${assets.length === 1 ? '' : 's'}`}
      source={asExample(`/v1/issuers/${issuer.g_strkey}`)}
      bodyClassName="-mx-4"
    >
      <div className="overflow-x-auto">
        <table className="min-w-full divide-y divide-line text-sm">
          <thead>
            <tr className="text-left text-[11px] uppercase tracking-wider text-ink-muted">
              <Th>Code</Th>
              <Th>Slug</Th>
              <Th align="right">Observations</Th>
              <Th align="right">First seen</Th>
              <Th align="right">Last seen</Th>
            </tr>
          </thead>
          <tbody className="divide-y divide-line-subtle">
            {assets.map((a) => (
              <tr
                key={a.asset_id}
                className="hover:bg-surface-muted"
              >
                <Td>
                  <Link
                    href={`/assets/${a.slug}`}
                    className="font-medium hover:text-brand-600"
                  >
                    {a.code}
                  </Link>
                </Td>
                <Td>
                  <span className="font-mono text-xs text-ink-muted">
                    {a.slug}
                  </span>
                </Td>
                <Td align="right">
                  <span className="font-mono tabular-nums">
                    {formatCompact(a.observation_count ?? 0)}
                  </span>
                </Td>
                {/* Ledger 0 does not exist (genesis is 1) — an absent
                    first/last_seen_ledger is unknown, not "#0". */}
                <Td align="right">
                  <span className="font-mono tabular-nums text-xs text-ink-muted">
                    {a.first_seen_ledger != null
                      ? `#${a.first_seen_ledger.toLocaleString('en-US')}`
                      : '—'}
                  </span>
                </Td>
                <Td align="right">
                  <span className="font-mono tabular-nums text-xs text-ink-muted">
                    {a.last_seen_ledger != null
                      ? `#${a.last_seen_ledger.toLocaleString('en-US')}`
                      : '—'}
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

function FlagPill({ on, label }: { on?: boolean; label: string }) {
  if (on === undefined) {
    return (
      <span className="inline-block rounded-sm bg-surface-subtle px-1.5 py-0.5 text-[10px] uppercase tracking-wider text-ink-muted">
        {label}: unknown
      </span>
    );
  }
  const cls = on
    ? 'bg-warn-50 text-warn-700'
    : 'bg-up-soft text-up-strong';
  return (
    <span
      className={`inline-block rounded-sm px-1.5 py-0.5 text-[10px] uppercase tracking-wider ${cls}`}
    >
      {label}: {on ? 'on' : 'off'}
    </span>
  );
}

function Stat({
  label,
  value,
  mono,
}: {
  label: string;
  value: string;
  mono?: boolean;
}) {
  return (
    <div>
      <dt className="text-[11px] uppercase tracking-wider text-ink-muted">
        {label}
      </dt>
      <dd className={mono ? 'break-all font-mono text-xs' : 'tabular-nums'}>
        {value}
      </dd>
    </div>
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

function is404(err: unknown): boolean {
  if (!err) return false;
  const msg = err instanceof Error ? err.message : String(err);
  return /\b404\b/.test(msg);
}
