import type { components } from '@/api/types';
import { useSACWrappers } from '@/api/hooks';
import { formatDecimalAmount, formatReadable } from '@/lib/format';
import { isNativeXlmSac, shortAssetText } from '@/lib/asset-label';

/** Reserve ids are SAC contracts; label them with the wrapped asset's code, not the C-address. */
export function useAssetText(): (asset: string | null | undefined) => string {
  const { data: sac } = useSACWrappers();
  return (asset) =>
    isNativeXlmSac(asset)
      ? 'XLM'
      : shortAssetText((asset && sac?.[asset]) || asset);
}

type Exclusion = components['schemas']['LendingStatsExclusion'];

const REASON_TEXT: Record<Exclusion['reason'], string> = {
  pool_list_read_failed: 'pool list could not be read',
  pool_read_failed: 'pool could not be read',
  reserves_read_failed: 'reserves could not be read',
  reserves_read_failed_recently: 'reserves could not be read',
  reserves_read_timed_out: 'reserves read timed out',
  pool_lineage_unknown: 'pool version unknown',
  no_decoded_reserve_state: 'no decoded reserve state',
  reserve_archived: 'archived on-chain',
  decode_failed: 'reserve state could not be decoded',
  not_captured: 'reserve state not captured',
  no_usd_value: 'no reliable price',
  price_read_failed: 'price could not be read',
  backstop_reader_unavailable: 'backstop not readable',
  backstop_read_failed: 'backstop could not be read',
  blend_v1_unsupported: 'Blend v1 pool not supported',
  pool_instance_unavailable: 'pool storage unavailable',
  backstop_balance_unavailable: 'backstop balance unavailable',
  token_not_in_catalogue: 'token not verified',
  token_read_failed: 'token could not be read',
  withheld: 'price withheld',
  unpriced: 'no reliable price',
};

/** Plain words for an exclusion or a `usd_reason`; the raw code when unmapped. */
export function reasonText(reason: string | null | undefined): string {
  if (!reason) return 'no reliable price';
  return (
    REASON_TEXT[reason as Exclusion['reason']] ?? reason.replace(/_/g, ' ')
  );
}

const PRICE_REASONS = new Set(['no_usd_value', 'unpriced', 'withheld']);
// An archived entry is the chain's state, read correctly, not a failed read.
const STATE_REASONS = new Set(['reserve_archived']);

/**
 * One line for a lower bound ("12 reserves have no reliable price, 2 are
 * archived on-chain, 24 could not be read") and the excluded names grouped by
 * reason for a disclosure.
 */
export function exclusionGroups(
  excluded: readonly Exclusion[],
  name: (e: Exclusion) => string,
): { line: string; groups: { reason: string; names: string[] }[] } {
  const unpriced = excluded.filter((e) => PRICE_REASONS.has(e.reason));
  const archived = excluded.filter((e) => STATE_REASONS.has(e.reason));
  const unread = excluded.filter(
    (e) => !PRICE_REASONS.has(e.reason) && !STATE_REASONS.has(e.reason),
  );
  const noun = (es: Exclusion[]) =>
    es.every((e) => e.asset) ? 'reserve' : 'item';
  const count = (es: Exclusion[], verb: [string, string]) =>
    es.length === 0
      ? []
      : [
          `${es.length} ${noun(es)}${es.length === 1 ? '' : 's'} ${es.length === 1 ? verb[0] : verb[1]}`,
        ];
  const line = [
    ...count(unpriced, ['has no reliable price', 'have no reliable price']),
    ...count(archived, ['is archived on-chain', 'are archived on-chain']),
    ...count(unread, ['could not be read', 'could not be read']),
  ].join(', ');
  const by = new Map<string, Set<string>>();
  for (const e of excluded) {
    const r = reasonText(e.reason);
    if (!by.has(r)) by.set(r, new Set());
    by.get(r)!.add(name(e));
  }
  return {
    line,
    groups: [...by].map(([reason, names]) => ({
      reason,
      names: [...names].sort(),
    })),
  };
}

/** "USDC (no reliable price)", or "pool CABC…1234 (pool could not be read)". */
export function exclusionText(
  e: Exclusion,
  poolName?: (p: string) => string,
  assetText: (a: string) => string = shortAssetText,
) {
  const what = e.asset
    ? assetText(e.asset)
    : e.pool
      ? (poolName?.(e.pool) ?? `pool ${e.pool.slice(0, 6)}…${e.pool.slice(-4)}`)
      : e.kind.replace(/_/g, ' ');
  return `${what} (${reasonText(e.reason)})`;
}

/**
 * UsdFigure — a USD decimal string, compact with the exact value on hover. A
 * lower bound renders "≥"; an absent value renders "—" carrying its reason,
 * never "$0".
 */
export function UsdFigure({
  value,
  lowerBound = false,
  reason,
  showReason = false,
}: {
  value: string | null | undefined;
  lowerBound?: boolean;
  reason?: string;
  /** Print the reason under the dash instead of only on hover. */
  showReason?: boolean;
}) {
  const shown = formatReadable(value, true);
  if (shown == null)
    return (
      <span className="text-ink-faint" title={reason ?? 'No USD value'}>
        —
        {showReason && reason && (
          <span className="text-ink-muted block text-xs font-normal">
            {reason}
          </span>
        )}
      </span>
    );
  const exact = `$${formatDecimalAmount(value, 2)}`;
  return (
    <span
      title={
        lowerBound ? `At least ${exact}${reason ? `; ${reason}` : ''}` : exact
      }
    >
      {lowerBound && (
        <>
          <span aria-hidden="true">≥</span>
          <span className="sr-only">at least </span>
        </>
      )}
      {shown}
    </span>
  );
}
