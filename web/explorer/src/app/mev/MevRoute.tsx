'use client';

import { AssetText } from '@/components/AssetLink';
import { CATEGORICAL_PALETTE } from '@/components/charts/DonutChart';

export interface RouteLeg {
  source: string;
  base: string;
  quote: string;
  base_amount: string;
  quote_amount: string;
}

export interface RouteHop {
  from: string;
  to: string;
  source: string;
  // smallest units; legs carry no decimals, so these are never rescaled
  fromAmount: string;
  toAmount: string;
}

/**
 * routeHops — legs in on-chain order → the asset path they trace, each hop's
 * output being the asset it shares with the next leg. null when the legs do
 * not chain unambiguously, so a caller never draws a path the evidence does not show.
 */
export function routeHops(legs: RouteLeg[]): RouteHop[] | null {
  if (legs.length < 2) return null;
  const has = (l: RouteLeg, a: string) => l.base === a || l.quote === a;
  const other = (l: RouteLeg, a: string) => (l.base === a ? l.quote : l.base);
  const [l0, l1] = legs;
  // a leg sharing both assets with the next (an A↔B round trip) has no
  // provable direction
  if (has(l1, l0.base) === has(l1, l0.quote)) return null;
  let from = has(l1, l0.base) ? l0.quote : l0.base;
  const hops: RouteHop[] = [];
  for (const l of legs) {
    if (!has(l, from) || l.base === l.quote) return null;
    const to = other(l, from);
    const sold = l.base === from;
    hops.push({
      from,
      to,
      source: l.source,
      fromAmount: sold ? l.base_amount : l.quote_amount,
      toAmount: sold ? l.quote_amount : l.base_amount,
    });
    from = to;
  }
  return hops;
}

/** The asset path of one arbitrage, one coloured arrow per venue. */
export function MevRoute({ hops }: { hops: RouteHop[] }) {
  const venues = [...new Set(hops.map((h) => h.source))];
  return (
    <span className="inline-flex flex-wrap items-center gap-1 font-mono text-xs">
      <AssetText canonical={hops[0].from} />
      {hops.map((h, i) => {
        const color =
          CATEGORICAL_PALETTE[
            venues.indexOf(h.source) % CATEGORICAL_PALETTE.length
          ];
        return (
          <span key={i} className="inline-flex items-center gap-1">
            <span
              className="inline-flex flex-col items-center leading-none"
              title={`${h.source}: ${h.fromAmount} → ${h.toAmount} (smallest units)`}
            >
              <span className="text-[9px]" style={{ color }}>
                {h.source}
              </span>
              <svg width="28" height="8" viewBox="0 0 28 8" aria-hidden>
                <path
                  d="M0 4 H24 M20 1 L25 4 L20 7"
                  stroke={color}
                  fill="none"
                  strokeWidth="1.5"
                />
              </svg>
            </span>
            <AssetText canonical={h.to} />
          </span>
        );
      })}
    </span>
  );
}
