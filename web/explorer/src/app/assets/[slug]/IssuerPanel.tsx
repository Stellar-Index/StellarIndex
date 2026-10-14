'use client';

import { useState } from 'react';

import Link from 'next/link';

import { Panel } from '@/components/reveal';
import {
  Badge,
  Callout,
  Skeleton,
  Table,
  TBody,
  Td,
  Th,
  THead,
  TR,
} from '@/components/ui';
import { PANEL_LABEL_CLASS } from '@/components/ui/Page';
import { asExample } from '@/api/client';
import { useIssuer, type Issuer } from '@/api/hooks';
import { assetHref } from '@/lib/fiat-slugs';
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
        bodyClassName="space-y-3"
      >
        <span className="sr-only">Loading issuer</span>
        <Skeleton className="h-4 w-40" aria-hidden />
        <Skeleton className="h-4 w-64" aria-hidden />
        <Skeleton className="h-4 w-52" aria-hidden />
      </Panel>
    );
  }

  return (
    <div className="space-y-4">
      {data.scam_reason && (
        <Callout tone="bad" title="Scam issuer.">
          Every asset below was minted by this flagged account — do not
          establish trustlines or trust its prices.
        </Callout>
      )}
      <Panel
        headingLevel={2}
        title="Issuer identity"
        source={asExample(`/v1/issuers/${gStrkey}`)}
      >
        <dl className="grid grid-cols-1 gap-3 text-sm sm:grid-cols-2">
          {/* Only surface the "Organisation" attribution when verified;
              otherwise home_domain (below) carries the real on-chain value
              without laundering an unverified name as authoritative. */}
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
            <Stat label="SEP-1 resolved" mono value={data.sep1_resolved_at} />
          )}
        </dl>
        <div className="mt-4 flex flex-wrap gap-1.5">
          <FlagPill
            on={data.auth_required ?? undefined}
            label="auth_required"
          />
          <FlagPill
            on={data.auth_revocable ?? undefined}
            label="auth_revocable"
          />
          <FlagPill
            on={data.auth_immutable ?? undefined}
            label="auth_immutable"
          />
          <FlagPill
            on={data.auth_clawback ?? undefined}
            label="auth_clawback"
          />
        </div>
        {data.auth_flags_source === 'last_known_before_removal' && (
          // Recovered from the account's state at its REMOVAL ledger.
          // Unlabelled pills would read as this issuer's current policy for
          // an account that no longer exists.
          <p className="text-ink-muted mt-2 text-xs">
            Flags are the last known values before this account was removed
            {data.auth_flags_as_of_ledger != null && (
              <>
                {' '}
                (ledger {data.auth_flags_as_of_ledger.toLocaleString('en-US')})
              </>
            )}
            , not its current policy.
          </p>
        )}
      </Panel>

      <IssuedAssetsTable issuer={data} />
    </div>
  );
}

type LedgerSeen = {
  first_seen_ledger?: number | null;
  last_seen_ledger?: number | null;
};

/** ledgerAxis — the earliest first-seen and latest last-seen ledger across assets, or null without two distinct ledgers. */
export function ledgerAxis(
  assets: readonly LedgerSeen[],
): { lo: number; hi: number } | null {
  let lo = Infinity;
  let hi = -Infinity;
  for (const a of assets) {
    if (a.first_seen_ledger == null || a.last_seen_ledger == null) continue;
    lo = Math.min(lo, a.first_seen_ledger);
    hi = Math.max(hi, a.last_seen_ledger);
  }
  return hi > lo ? { lo, hi } : null;
}

function SpanBar({
  a,
  axis,
}: {
  a: LedgerSeen;
  axis: { lo: number; hi: number };
}) {
  if (a.first_seen_ledger == null || a.last_seen_ledger == null) {
    return <span className="text-ink-muted text-xs">—</span>;
  }
  const w = axis.hi - axis.lo;
  const left = ((a.first_seen_ledger - axis.lo) / w) * 100;
  const width = Math.max(
    ((a.last_seen_ledger - a.first_seen_ledger) / w) * 100,
    1,
  );
  return (
    <div
      className="bg-surface-muted relative h-1.5 w-32 rounded-full"
      role="img"
      aria-label={`Seen from ledger ${a.first_seen_ledger} to ${a.last_seen_ledger}`}
    >
      <div
        className="bg-brand-500 absolute top-0 h-1.5 rounded-full"
        style={{ left: `${left}%`, width: `${Math.min(width, 100 - left)}%` }}
      />
    </div>
  );
}

const PREVIEW_ROWS = 12;

function IssuedAssetsTable({ issuer }: { issuer: Issuer }) {
  const [expanded, setExpanded] = useState(false);
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

  const axis = ledgerAxis(assets);
  const shown = expanded ? assets : assets.slice(0, PREVIEW_ROWS);

  return (
    <Panel
      headingLevel={2}
      title="Issued assets"
      hint={`${assets.length} asset${assets.length === 1 ? '' : 's'}`}
      source={asExample(`/v1/issuers/${issuer.g_strkey}`)}
      bodyClassName="-mx-4 -mb-4"
    >
      <div className="overflow-x-auto">
        <Table>
          <THead>
            <tr>
              <Th>Code</Th>
              <Th align="right">Observations</Th>
              {axis && <Th>Active span</Th>}
              <Th align="right">First seen</Th>
              <Th align="right">Last seen</Th>
            </tr>
          </THead>
          <TBody>
            {shown.map((a) => (
              <TR key={a.asset_id}>
                <Td>
                  <Link
                    href={assetHref(a.slug ?? a.asset_id ?? a.code ?? '')}
                    className="hover:text-brand-600 font-medium"
                  >
                    {a.code}
                  </Link>
                </Td>
                <Td align="right">
                  <span className="font-mono tabular-nums">
                    {formatCompact(a.observation_count ?? 0)}
                  </span>
                </Td>
                {axis && (
                  <Td>
                    <SpanBar a={a} axis={axis} />
                  </Td>
                )}
                {/* Ledger 0 does not exist (genesis is 1) — an absent
                    first/last_seen_ledger is unknown, not "#0". */}
                <Td align="right">
                  <span className="text-ink-muted font-mono text-xs tabular-nums">
                    {a.first_seen_ledger != null
                      ? `#${a.first_seen_ledger.toLocaleString('en-US')}`
                      : '—'}
                  </span>
                </Td>
                <Td align="right">
                  <span className="text-ink-muted font-mono text-xs tabular-nums">
                    {a.last_seen_ledger != null
                      ? `#${a.last_seen_ledger.toLocaleString('en-US')}`
                      : '—'}
                  </span>
                </Td>
              </TR>
            ))}
          </TBody>
        </Table>
      </div>
      {assets.length > PREVIEW_ROWS && (
        <div className="border-line border-t px-4 py-2">
          <button
            type="button"
            onClick={() => setExpanded((v) => !v)}
            className="text-brand-600 text-xs font-medium hover:underline"
          >
            {expanded
              ? `Show first ${PREVIEW_ROWS}`
              : `Show all ${assets.length} assets`}
          </button>
        </div>
      )}
    </Panel>
  );
}

function FlagPill({ on, label }: { on?: boolean; label: string }) {
  const tone = on === undefined ? 'neutral' : on ? 'warn' : 'ok';
  return (
    <Badge tone={tone} className="font-mono">
      {label}: {on === undefined ? 'unknown' : on ? 'on' : 'off'}
    </Badge>
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
      <dt className={PANEL_LABEL_CLASS}>{label}</dt>
      <dd
        className={
          mono
            ? 'text-ink font-mono text-xs break-all'
            : 'text-ink tabular-nums'
        }
      >
        {value}
      </dd>
    </div>
  );
}

function is404(err: unknown): boolean {
  if (!err) return false;
  const msg = err instanceof Error ? err.message : String(err);
  return /\b404\b/.test(msg);
}
