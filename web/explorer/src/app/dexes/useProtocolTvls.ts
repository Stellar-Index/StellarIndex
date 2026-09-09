'use client';

import { useQuery } from '@tanstack/react-query';

import { apiGet } from '@/api/client';
import type { components } from '@/api/types';
import type { DexTvlTotal } from '@/app/protocols/ProtocolTvlPanel';

/**
 * ProtocolTvl — the additive `tvl` snapshot object on a
 * /v1/protocols directory row (generated from the OpenAPI spec).
 * `tvl_usd` is a decimal STRING (ADR-0003) and an honest lower
 * bound whenever `unpriced_pools` > 0.
 */
export type ProtocolTvl = NonNullable<
  components['schemas']['ProtocolRow']['tvl']
>;

type ProtocolRow = components['schemas']['ProtocolRow'];

/**
 * ProtocolTvls — what one /v1/protocols read yields for this route:
 * the per-protocol snapshots keyed by name, and the served headline.
 *
 * `total` is the envelope's `tvl_total`, which is `omitempty` and is
 * OMITTED whenever the server's reconciliation could admit no
 * per-protocol figure. It stays `undefined` here in that case —
 * callers must not substitute a zero.
 */
export interface ProtocolTvls {
  byProtocol: Record<string, ProtocolTvl>;
  total?: DexTvlTotal;
}

/**
 * useProtocolTvls — one query over /v1/protocols returning the
 * per-protocol TVL snapshots plus the headline total.
 * Protocols without an absolute reserve source (sdex, the lending and
 * bridge protocols) are simply absent from `byProtocol` — callers
 * render "—", never a fabricated zero. The backing snapshot refreshes
 * server-side every ~10 min, so a 5-min client staleTime adds no
 * staleness of its own.
 */
export function useProtocolTvls() {
  return useQuery<ProtocolTvls>({
    queryKey: ['/v1/protocols', 'tvl-map'],
    queryFn: async () => {
      const env = await apiGet<{
        data?: { protocols?: ProtocolRow[]; tvl_total?: DexTvlTotal };
      }>('/v1/protocols');
      const byProtocol: Record<string, ProtocolTvl> = {};
      for (const p of env.data?.protocols ?? []) {
        if (p.tvl) byProtocol[p.name] = p.tvl;
      }
      return { byProtocol, total: env.data?.tvl_total };
    },
    staleTime: 300_000,
  });
}
