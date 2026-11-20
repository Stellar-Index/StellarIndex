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
  no_usd_value: 'no reliable price',
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
}: {
  value: string | null | undefined;
  lowerBound?: boolean;
  reason?: string;
}) {
  const shown = formatReadable(value, true);
  if (shown == null)
    return (
      <span className="text-ink-faint" title={reason ?? 'No USD value'}>
        —
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
