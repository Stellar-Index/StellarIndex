'use client';

import { formatCompact } from '@/lib/format';

export type SupplyFlowRow = {
  label: string;
  /** Whole-asset-unit value (already scaled by 10^decimals), or null when unserved. */
  value: number | null;
  /** Bar direction: additions grow supply (up tone), removals shrink it (down tone). */
  direction: 'add' | 'remove';
};

/**
 * buildSupplyFlowRows — mint/burn/clawback totals (smallest-unit wire
 * strings) → display rows in whole asset units. Null/absent totals stay
 * null so they render "—", never a fabricated zero; a served "0" is a
 * real zero and renders as one. Exported for tests.
 */
export function buildSupplyFlowRows(
  totals: {
    mint_total?: string | null;
    burn_total?: string | null;
    clawback_total?: string | null;
  },
  decimals: number,
): SupplyFlowRow[] {
  const scale = (s: string | null | undefined): number | null => {
    if (s == null) return null;
    const n = Number(s);
    return Number.isFinite(n) ? n / 10 ** decimals : null;
  };
  return [
    { label: 'Minted', value: scale(totals.mint_total), direction: 'add' },
    { label: 'Burned', value: scale(totals.burn_total), direction: 'remove' },
    { label: 'Clawed back', value: scale(totals.clawback_total), direction: 'remove' },
  ];
}

/**
 * SupplyFlowsBar — comparative mint / burn / clawback bars from the
 * supply_flows lake totals (GET /v1/assets/{id}/supply). Linear scale on
 * the largest flow: a burn that's a sliver of the mint bar IS the honest
 * picture, so every row carries its value label rather than inflating
 * small bars. Additions render in the up tone, removals in the down
 * tone, with the direction also written in the row label (never colour
 * alone).
 */
export function SupplyFlowsBar({ rows }: { rows: SupplyFlowRow[] }) {
  const max = Math.max(...rows.map((r) => (r.value != null ? r.value : 0)));
  if (!(max > 0)) return null;
  return (
    <div className="space-y-1.5" data-testid="supply-flows-bar">
      {rows.map((r) => {
        const pct = r.value != null && r.value > 0 ? Math.max(0.5, (r.value / max) * 100) : 0;
        return (
          <div key={r.label} className="flex items-center gap-2 text-xs">
            <span className="w-24 shrink-0 text-[11px] uppercase tracking-wider text-ink-muted">
              {r.label}
              <span className="ml-1 text-ink-faint" aria-hidden>
                {r.direction === 'add' ? '+' : '−'}
              </span>
            </span>
            <div className="h-2.5 flex-1 overflow-hidden rounded-sm bg-surface-subtle">
              {r.value != null && r.value > 0 && (
                <div
                  className={`h-full ${r.direction === 'add' ? 'bg-up/70' : 'bg-down/70'}`}
                  style={{ width: `${pct}%` }}
                />
              )}
            </div>
            <span className="w-20 shrink-0 text-right font-mono tabular-nums text-ink-body">
              {r.value != null ? formatCompact(r.value) : '—'}
            </span>
          </div>
        );
      })}
    </div>
  );
}
