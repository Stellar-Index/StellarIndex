'use client';

import { useQuery } from '@tanstack/react-query';

import { Panel } from '@/components/reveal';
import { apiGet, asExample } from '@/api/client';
import { useIssuer } from '@/api/hooks';
import { scamFlagTags } from '@/lib/directory-tags';
import type { paths } from '@/api/types';

type HoldersResp = NonNullable<
  paths['/assets/{asset_id}/holders']['get']['responses'][200]['content']['application/json']['data']
>;

type VolumeCharacter = 'market' | 'operational' | 'concentrated';
type VolumeSignals = {
  window_days: number;
  top_account_pair_vol_share: number;
};

type Fact = {
  label: string;
  value: string;
  source: string;
  asOf: string;
};

const NOT_REPORTED = 'not reported';

/**
 * Share of the served rows' balance held by the first ten, as a percentage
 * string. BigInt throughout: balances are integer strings that can exceed
 * 2^53 (ADR-0003). Null when there is nothing to compare.
 */
export function topTenShare(
  holders: readonly { balance?: string }[],
): string | null {
  if (holders.length <= 10) return null;
  let top = BigInt(0);
  let total = BigInt(0);
  holders.forEach((h, i) => {
    if (!h.balance || !/^\d+$/.test(h.balance)) return;
    const b = BigInt(h.balance);
    total += b;
    if (i < 10) top += b;
  });
  if (total <= BigInt(0)) return null;
  const tenths = (top * BigInt(1000)) / total;
  return `${(Number(tenths) / 10).toFixed(1)}%`;
}

function flagList(i: {
  auth_required?: boolean | null;
  auth_revocable?: boolean | null;
  auth_immutable?: boolean | null;
  auth_clawback?: boolean | null;
}): string | null {
  const known = [
    ['auth_required', i.auth_required],
    ['auth_revocable', i.auth_revocable],
    ['auth_immutable', i.auth_immutable],
    ['auth_clawback', i.auth_clawback],
  ] as const;
  if (known.every(([, v]) => v == null)) return null;
  const on = known.filter(([, v]) => v).map(([k]) => k);
  return on.length > 0 ? `on: ${on.join(', ')}` : 'no restrictive flags set';
}

/**
 * Trust facts for an asset: each fact states its serving endpoint and its
 * as_of. Facts with no reader yet are listed as not captured rather than
 * omitted, so absence is never read as "fine".
 */
export function AssetTrustFacts({
  assetID,
  issuer,
  directoryTags,
  scamReason,
  directoryDomain,
  volumeCharacter,
  volumeCharacterSignals,
}: {
  assetID: string;
  issuer: string | null;
  directoryTags?: readonly string[] | null;
  scamReason?: string | null;
  directoryDomain?: string | null;
  volumeCharacter?: VolumeCharacter | null;
  volumeCharacterSignals?: VolumeSignals | null;
}) {
  const issuerQ = useIssuer(issuer ?? undefined);
  const holdersQ = useQuery<HoldersResp>({
    queryKey: ['/v1/assets/{id}/holders', assetID],
    retry: false,
    staleTime: 60_000,
    queryFn: async () => {
      const env = await apiGet<{ data: HoldersResp }>(
        `/v1/assets/${encodeURIComponent(assetID)}/holders`,
        { limit: 100 },
      );
      return env.data;
    },
  });

  const facts: Fact[] = [];
  const iss = issuerQ.data;
  const issuerSrc = issuer ? `/v1/issuers/${issuer}` : '';

  const created = iss?.creation_ledger;
  if (typeof created === 'number') {
    facts.push({
      label: 'Issuer account age',
      value: `created at ledger #${created.toLocaleString('en-US')}`,
      source: issuerSrc,
      asOf: NOT_REPORTED,
    });
  }
  const flags = iss ? flagList(iss) : null;
  if (iss && flags) {
    facts.push({
      label: 'Authorization posture',
      value:
        iss.auth_flags_source === 'last_known_before_removal'
          ? `${flags} (last known before the account was removed)`
          : flags,
      source: issuerSrc,
      asOf:
        iss.auth_flags_as_of_ledger != null
          ? `ledger ${iss.auth_flags_as_of_ledger.toLocaleString('en-US')}`
          : NOT_REPORTED,
    });
  }

  const holders = holdersQ.data;
  const holdersSrc = `/v1/assets/${assetID}/holders`;
  const holdersAsOf =
    holders?.as_of_ledger != null
      ? `ledger ${holders.as_of_ledger.toLocaleString('en-US')}`
      : NOT_REPORTED;
  const holderCount = holders?.holder_count;
  if (holders && holderCount === 0) {
    facts.push({
      label: 'Holders',
      value: '0',
      source: holdersSrc,
      asOf: holdersAsOf,
    });
  } else if (holders && holderCount != null) {
    facts.push({
      label: 'Holders',
      value: holderCount.toLocaleString('en-US'),
      source: holdersSrc,
      asOf: holdersAsOf,
    });
    const share = topTenShare(holders.holders ?? []);
    if (share) {
      facts.push({
        label: 'Concentration',
        value: `top 10 hold ${share} of the served top ${holders.holders?.length} rows (not of total supply)`,
        source: holdersSrc,
        asOf: holdersAsOf,
      });
    }
  }

  if (volumeCharacter) {
    const days = volumeCharacterSignals?.window_days;
    facts.push({
      label: 'Volume character',
      value: volumeCharacter,
      source: `/v1/assets/${assetID}`,
      asOf:
        days != null
          ? `trailing ${days}-day window (no ledger stamp)`
          : NOT_REPORTED,
    });
  }

  const flagged = scamFlagTags(directoryTags);
  const reason = scamReason?.trim();
  facts.push({
    label: 'Directory / scam status',
    value: [
      reason
        ? `flagged: ${reason}`
        : flagged.length > 0
          ? `flagged by community directory: ${flagged.join(', ')}`
          : 'no scam flag recorded',
      directoryDomain
        ? `directory lists issuer domain: ${directoryDomain}`
        : '',
    ]
      .filter(Boolean)
      .join(' · '),
    source: `stellar.expert community directory via /v1/assets/${assetID}`,
    asOf: NOT_REPORTED,
  });

  const pending: string[] = [];
  if (!issuer) pending.push('issuer account facts (native or contract asset)');
  if (issuer && issuerQ.isError) pending.push('issuer facts (read failed)');
  if (holdersQ.isError) pending.push('holder facts (read failed)');
  else if (holders && holderCount == null) {
    pending.push('holder count (not reported)');
  }
  if (!volumeCharacter) pending.push('volume character');
  pending.push(
    'holder history',
    'currency-authority fields (status, anchoring, redemption)',
  );

  return (
    <Panel
      headingLevel={2}
      title="Trust facts"
      hint="Evidence with its source and as-of; not a verdict"
      source={asExample(`/v1/assets/${assetID}`)}
    >
      <dl className="grid grid-cols-1 gap-3 sm:grid-cols-2">
        {facts.map((f) => (
          <div key={f.label}>
            <dt className="text-ink-muted text-[11px] tracking-wider uppercase">
              {f.label}
            </dt>
            <dd>{f.value}</dd>
            <dd className="text-ink-faint font-mono text-[11px] break-all">
              source {f.source} · as of {f.asOf}
            </dd>
          </div>
        ))}
      </dl>
      <p className="text-ink-muted mt-3 text-xs">
        Not yet captured: {pending.join('; ')}.
      </p>
    </Panel>
  );
}
