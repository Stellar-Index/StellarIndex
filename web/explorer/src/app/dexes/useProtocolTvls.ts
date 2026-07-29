'use client';

import { useQuery } from '@tanstack/react-query';

import { apiGet } from '@/api/client';
import type { components } from '@/api/types';

/**
 * ProtocolTvl — the additive `tvl` snapshot object on a
 * /v1/protocols directory row (generated from the OpenAPI spec).
 * `tvl_usd` is a decimal STRING (ADR-0003) and an honest lower
 * bound whenever `unpriced_pools` > 0.
 */
export type ProtocolTvl = NonNullable<components['schemas']['ProtocolRow']['tvl']>;

type ProtocolRow = components['schemas']['ProtocolRow'];

/**
 * useProtocolTvls — one query over /v1/protocols returning a
 * name → tvl map for the protocols that carry a TVL snapshot.
 * Protocols without an absolute reserve source (phoenix, comet,
 * sdex) are simply absent — callers render "—", never a fabricated
 * zero. The backing snapshot refreshes server-side every ~10 min,
 * so a 5-min client staleTime adds no staleness of its own.
 */
export function useProtocolTvls() {
  return useQuery<Record<string, ProtocolTvl>>({
    queryKey: ['/v1/protocols', 'tvl-map'],
    queryFn: async () => {
      const env = await apiGet<{ data?: { protocols?: ProtocolRow[] } }>('/v1/protocols');
      const out: Record<string, ProtocolTvl> = {};
      for (const p of env.data?.protocols ?? []) {
        if (p.tvl) out[p.name] = p.tvl;
      }
      return out;
    },
    staleTime: 300_000,
  });
}
