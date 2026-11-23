import { apiGet } from '@/api/client';
import type { paths } from '@/api/types';
import { formatReadable, sumDecimalStrings } from '@/lib/format';

type Resp =
  paths['/lending/pools/{pool}/liquidations']['get']['responses'][200]['content']['application/json'];
export type Liquidation = NonNullable<Resp['data']>['liquidations'][number];
export type LiquidationLeg = Liquidation['lot'][number];

export interface LiquidationsPage {
  fills: Liquidation[];
  next: string | undefined;
}

/** One page of a pool's liquidation fills, newest first. */
export async function fetchLiquidations(
  pool: string,
  limit: number,
  cursor?: string,
): Promise<LiquidationsPage> {
  const env = await apiGet<Resp>(
    `/v1/lending/pools/${encodeURIComponent(pool)}/liquidations`,
    { limit, cursor: cursor || undefined },
  );
  return {
    fills: env.data?.liquidations ?? [],
    next: env.pagination?.next ?? undefined,
  };
}

const LIQ_REASON_TEXT: Record<string, string> = {
  no_price_within_bound: 'No price was recorded close enough to the fill.',
  price_withheld:
    'The price at the fill is withheld because a source behind it was excluded.',
  price_thin_market: 'The market was too thin at the fill to give a price.',
  price_unread: 'The price at the fill could not be read.',
  rate_at_fill_unrecorded:
    "The reserve's exchange rate at the fill was not recorded, so the token amount is shown unvalued.",
  rate_unread: "The reserve's exchange rate at the fill could not be read.",
  leg_kind_unknown:
    'Whether this is collateral or debt is not known for this protocol, so it is not valued.',
  backstop_token_not_valued_at_event_time:
    'Backstop tokens are not valued at the time of a fill.',
  amount_unparseable: 'The amount could not be read.',
  amounts_not_emitted:
    'This Blend V1 pool does not record amounts in its fill events.',
};

/** Plain words for a liquidation `reason` / `usd_reason`; the raw code when unmapped. */
export function liquidationReasonText(reason: string | null | undefined) {
  if (!reason) return 'No USD value.';
  return LIQ_REASON_TEXT[reason] ?? reason.replace(/_/g, ' ');
}

export interface DailyRepaid {
  day: string;
  usd: string;
  /** Some fill that day has no or a partial USD debt figure. */
  lowerBound: boolean;
  /** Bar geometry only; the label uses `display`. */
  value: number;
  display: string;
}

/** Debt repaid in USD per UTC day over the given fills, newest day first; days with no valued debt are left out. */
export function dailyDebtRepaid(fills: readonly Liquidation[]): DailyRepaid[] {
  const byDay = new Map<string, { usd: string[]; partial: boolean }>();
  for (const f of fills) {
    const day = f.time.slice(0, 10);
    const d = byDay.get(day) ?? { usd: [], partial: false };
    if (f.bid_usd != null) d.usd.push(f.bid_usd);
    if (f.bid_usd == null || f.bid_usd_lower_bound) d.partial = true;
    byDay.set(day, d);
  }
  const out: DailyRepaid[] = [];
  for (const [day, d] of byDay) {
    const usd = sumDecimalStrings(d.usd);
    if (usd == null) continue;
    const shown = formatReadable(usd, true) ?? usd;
    out.push({
      day,
      usd,
      lowerBound: d.partial,
      value: Number(usd),
      display: `${d.partial ? '≥' : ''}${shown}`,
    });
  }
  return out.sort((a, b) => (a.day < b.day ? 1 : -1));
}
