import { baseUnitsDecimal, formatCompactTokenUnits } from '@/lib/format';
import type { components } from '@/api/types';

export type ReserveConfig = components['schemas']['LendingReserveConfig'];
export type PoolConfig = components['schemas']['LendingPoolConfig'];

export type ConfigKind =
  | 'queue_set_reserve'
  | 'cancel_set_reserve'
  | 'set_reserve'
  | 'update_pool'
  | 'set_status'
  | 'set_admin';

const KIND_LABELS: Record<ConfigKind, string> = {
  queue_set_reserve: 'Reserve queued',
  set_reserve: 'Reserve applied',
  cancel_set_reserve: 'Reserve change cancelled',
  update_pool: 'Pool updated',
  set_status: 'Status changed',
  set_admin: 'Admin changed',
};

/** Human label for a config-history kind; an unknown kind shows as sent. */
export function configKindLabel(kind: string): string {
  return KIND_LABELS[kind as ConfigKind] ?? kind;
}

const STATUS_LABELS: Record<string, string> = {
  admin_active: 'Admin active',
  active: 'Active',
  admin_on_ice: 'Admin on ice',
  on_ice: 'On ice',
  admin_frozen: 'Admin frozen',
  frozen: 'Frozen',
  setup: 'Setup',
};

/** Pool status as "Label" or, with no label, "Status code N"; null when unknown. */
export function poolStatusText(
  p: PoolConfig | null | undefined,
): string | null {
  if (!p) return null;
  if (p.status_label) return STATUS_LABELS[p.status_label] ?? p.status_label;
  return p.status == null ? null : `Status code ${p.status}`;
}

/**
 * A fixed-point fraction as a percent string: 9500000 at 7 decimals is
 * "95%". Rounded to two places, trailing zeros trimmed; "—" when absent
 * or not an integer, never a fake 0.
 */
export function fixedPct(
  raw: string | null | undefined,
  decimals: number | null | undefined,
): string {
  const t = raw?.trim() ?? '';
  if (!/^\d+$/.test(t) || decimals == null || !Number.isInteger(decimals))
    return '—';
  if (decimals < 0) return '—';
  // hundredths of a percent, rounded half up
  const num = BigInt(t) * 10_000n;
  const den = 10n ** BigInt(decimals);
  const h = (2n * num + den) / (2n * den);
  const whole = h / 100n;
  const frac = (h % 100n).toString().padStart(2, '0').replace(/0+$/, '');
  return `${whole.toLocaleString('en-US')}${frac ? `.${frac}` : ''}%`;
}

/** A fixed-point integer as its exact plain decimal ("1.0000000"), or "—". */
export function fixedDecimal(
  raw: string | null | undefined,
  decimals: number | null | undefined,
): string {
  if (decimals == null) return '—';
  return baseUnitsDecimal(raw, decimals) ?? '—';
}

// The cap is an i128 with no sentinel in the contracts; a value at or above
// i64::MAX is a "max" placeholder (i64/u64/i128::MAX), not a real limit.
const UNLIMITED_CAP = 9223372036854775807n;

/** True for a cap set to a max-integer placeholder. */
export function isUnlimitedCap(cap: string | null | undefined): boolean {
  return cap != null && /^\d+$/.test(cap) && BigInt(cap) >= UNLIMITED_CAP;
}

/** Supply cap in whole tokens (compact); "No cap" for a max placeholder; "—" when absent or unscaled. */
export function supplyCapText(
  cap: string | null | undefined,
  decimals: number | null | undefined,
): string {
  if (isUnlimitedCap(cap)) return 'No cap';
  if (cap == null || decimals == null) return '—';
  return formatCompactTokenUnits(cap, decimals);
}

export interface ChangeLike {
  kind: string;
  config?: ReserveConfig | null;
  pool_config?: PoolConfig | null;
}

/** The "label value" pairs a config-history row shows; empty when nothing is known. */
export function changeValues(
  c: ChangeLike,
  configDecimals: number | null | undefined,
  tokenDecimals?: number | null,
): { label: string; value: string }[] {
  const out: { label: string; value: string }[] = [];
  const cfg = c.config;
  if (cfg) {
    out.push(
      {
        label: 'Collateral factor',
        value: fixedPct(cfg.collateral_factor, configDecimals),
      },
      {
        label: 'Liability factor',
        value: fixedPct(cfg.liability_factor, configDecimals),
      },
    );
    if (cfg.supply_cap != null)
      out.push({
        label: 'Supply cap',
        value: supplyCapText(cfg.supply_cap, tokenDecimals ?? cfg.decimals),
      });
  }
  const p = c.pool_config;
  if (p) {
    if (p.status != null || p.status_label)
      out.push({ label: 'Status', value: poolStatusText(p) ?? '—' });
    if (p.backstop_take_rate != null)
      out.push({
        label: 'Backstop take rate',
        value: fixedPct(p.backstop_take_rate, configDecimals),
      });
    if (p.max_positions != null)
      out.push({ label: 'Max positions', value: String(p.max_positions) });
    if (p.min_collateral != null)
      out.push({ label: 'Min collateral', value: p.min_collateral });
    if (p.oracle) out.push({ label: 'Oracle', value: p.oracle });
    if (p.admin) out.push({ label: 'Admin', value: p.admin });
  }
  return out;
}
