'use client';

import { useQuery } from '@tanstack/react-query';

import { apiGet, type EnvelopeFlags } from '@/api/client';
import { useLedgerFollow } from '@/lib/live/hooks';
import type { PoolConfig, ReserveConfig } from '../poolConfig';

export interface ReserveRow {
  asset: string;
  decimals: number | null;
  supplied: string;
  borrowed: string;
  supplied_usd: string | null;
  borrowed_usd: string | null;
  utilization_pct: number;
  borrow_apr: number | null;
  supply_apr: number | null;
  price_usd?: string | null;
  price_stale?: boolean;
  config?: ReserveConfig | null;
  ir_mod?: string | null;
  ir_mod_decimals?: number;
  backstop_credit?: string | null;
  last_update?: string | null;
}

export interface ReservesResp {
  pool: string;
  tvl_usd: string | null;
  lower_bound?: boolean;
  omitted_reserves?: string[];
  reserves: ReserveRow[];
  pool_config?: PoolConfig | null;
  config_decimals?: number;
  /** The response envelope's flags, carried so the page can show them. */
  flags?: EnvelopeFlags;
}

// One query shared by the reserve table and the pool details panel.
export function usePoolReserves(pool: string) {
  // Live (RT-2): refresh lending-pool reserves on each ledger close.
  useLedgerFollow(['/v1/lending/pools/{pool}/reserves']);
  return useQuery<ReservesResp>({
    queryKey: ['/v1/lending/pools/{pool}/reserves', pool],
    retry: false,
    queryFn: async () => {
      const env = await apiGet<{ data: ReservesResp; flags?: EnvelopeFlags }>(
        `/v1/lending/pools/${encodeURIComponent(pool)}/reserves`,
        {},
      );
      return { ...env.data, flags: env.flags };
    },
    staleTime: 60_000,
  });
}
