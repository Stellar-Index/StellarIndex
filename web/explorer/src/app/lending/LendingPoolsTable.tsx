'use client';

import Link from 'next/link';
import { useQuery } from '@tanstack/react-query';

import { Panel } from '@/components/reveal';
import { apiGet, asExample } from '@/api/client';
import { formatRelative } from '@/lib/format';
import type { paths } from '@/api/types';

// One /v1/lending/pools row, derived from the generated OpenAPI
// contract (src/api/types.ts, `make web-generate-api`).
type LendingPool = NonNullable<
  paths['/lending/pools']['get']['responses'][200]['content']['application/json']['data']
>[number];

// Compact display of a token base-units magnitude (string big-int).
// Display-only; precision loss past 2^53 is fine for an at-a-glance
// column (the API ships the exact decimal string).
// Curated metadata for every Blend mainnet contract we know of.
// Sourced from docs/operations/wasm-audits/blend.md (Phase 4 walk,
// last verified 2026-05-03). Reserve-asset breakdown per pool
// needs a Blend-pool-storage reader that doesn't exist yet (#84);
// until then this table at least gives users deploy timestamps +
// initiator addresses so pools are distinguishable.
interface PoolMeta {
  label: string;
  deployedAt?: string;
  initiator?: string;
}

const BLEND_POOL_META: Record<string, PoolMeta> = {
  CAQQR5SWBXKIGZKPBZDH3KM5GQ5GUTPKB7JAFCINLZBC5WXPJKRG3IM7: {
    label: 'Backstop V2',
    deployedAt: '2025-04-14',
    initiator: 'GAX2VVWVHU5YQY5J3NJBXKHI3FFKZN54BE6GRJCWSIKSBZTQWJJNJMPC',
  },
  CDSYOAVXFY7SM5S64IZPPPYB4GVGGLMQVFREPSQQEZVIWXX5R23G4QSU: {
    label: 'Pool Factory V2',
    deployedAt: '2025-04-14',
    initiator: 'GAX2VVWVHU5YQY5J3NJBXKHI3FFKZN54BE6GRJCWSIKSBZTQWJJNJMPC',
  },
  CAJJZSGMMM3PD7N33TAPHGBUGTB43OC73HVIK2L2G6BNGGGYOSSYBXBD: {
    label: 'Pool #1 (genesis)',
    deployedAt: '2025-04-14',
    initiator: 'GAX2VVWVHU5YQY5J3NJBXKHI3FFKZN54BE6GRJCWSIKSBZTQWJJNJMPC',
  },
  CBNR7PYFY775UG7W37B4OJG2OBBUKLFW6VIBHFDKKLR2HECPRMRZMDK3: {
    label: 'Pool #2',
    deployedAt: '2025-04-15',
    initiator: 'GBCAS7XIGDRZY4BMABJMGGW7J3YTITRRV5BTEMFQE5ZZSSVWHHX2ZSS4',
  },
  CCCCIQSDILITHMM7PBSLVDT5MISSY7R26MNZXCX4H7J5JQ5FPIYOGYFS: {
    label: 'Pool #3',
    deployedAt: '2025-04-17',
    initiator: 'GBCAS7XIGDRZY4BMABJMGGW7J3YTITRRV5BTEMFQE5ZZSSVWHHX2ZSS4',
  },
  CB4OFHAY2TAEYUVPOJS36S657C6NYMSIFUNCCA5AHYT46Y5XUID3O2ED: {
    label: 'Pool #4',
    deployedAt: '2025-05-01',
    initiator: 'GBIWJGAOSFC4KUPHXM573TKTWHMI7VW7D4GCHYZYH243Q6HVBV7ORBIT',
  },
  CAE7QVOMBLZ53CDRGK3UNRRHG5EZ5NQA7HHTFASEMYBWHG6MDFZTYHXC: {
    label: 'Pool #5',
    deployedAt: '2025-05-01',
    initiator: 'GBIWJGAOSFC4KUPHXM573TKTWHMI7VW7D4GCHYZYH243Q6HVBV7ORBIT',
  },
  CBYOBT7ZCCLQCBUYYIABZLSEGDPEUWXCUXQTZYOG3YBDR7U357D5ZIRF: {
    label: 'Pool #6',
    deployedAt: '2025-07-13',
    initiator: 'GCCI7K6QU6FVVIXWSLKRPTBKJCFBLEJKPTZMP27A2KL37N4ZL3OCM3GI',
  },
  CALRF5I2OCJCU577R6MZBCY5IIXNMAAG6PNMN7GUKEYIXBJCJN2FJRVI: {
    label: 'Pool #7',
    deployedAt: '2025-11-22',
    initiator: 'GDH3FRHOOWXYXEASH43N2VOVFOPJSVJF3EQFSLBLJYFPHOUAF4N4AETH',
  },
  CADR6Q2UOCDJAGXMAB2E6SRT35STLZ2IGLZUCXJQG7TC2LNKCU5RTQVY: {
    label: 'Pool #8',
    deployedAt: '2025-11-25',
    initiator: 'GDH3FRHOOWXYXEASH43N2VOVFOPJSVJF3EQFSLBLJYFPHOUAF4N4AETH',
  },
  CDMAVJPFXPADND3YRL4BSM3AKZWCTFMX27GLLXCML3PD62HEQS5FPVAI: {
    label: 'Pool #9',
    deployedAt: '2025-11-25',
    initiator: 'GDH3FRHOOWXYXEASH43N2VOVFOPJSVJF3EQFSLBLJYFPHOUAF4N4AETH',
  },
};

export function LendingPoolsTable() {
/**
 * PoolRealStats — TVL (USD) + true utilization from the pool-storage
 * reader via /v1/lending/pools/{pool}/reserves (site audit S-013: the
 * list used to render window event proxies in raw base-units —
 * "578.1T supplied", "222.4% util" — impossible numbers presented as
 * real ones while the real ones existed one endpoint over).
 */
function PoolRealStats({ pool }: { pool: string }) {
  const q = useQuery<{
    tvl_usd?: string;
    reserves?: { supplied_usd?: string; borrowed_usd?: string }[];
  }>({
    queryKey: ['/v1/lending/pools/{pool}/reserves', pool],
    staleTime: 300_000,
    retry: false,
    queryFn: async () =>
      (
        await apiGet<{ data: { tvl_usd?: string; reserves?: { supplied_usd?: string; borrowed_usd?: string }[] } }>(
          `/v1/lending/pools/${encodeURIComponent(pool)}/reserves`,
          {},
        )
      ).data,
  });
  const tvl = q.data?.tvl_usd ? Number(q.data.tvl_usd) : null;
  const supplied = (q.data?.reserves ?? []).reduce((s, r) => s + Number(r.supplied_usd ?? 0), 0);
  const borrowed = (q.data?.reserves ?? []).reduce((s, r) => s + Number(r.borrowed_usd ?? 0), 0);
  const util = supplied > 0 ? (borrowed / supplied) * 100 : null;
  const fmtUsd = (n: number) =>
    n >= 1e9 ? `$${(n / 1e9).toFixed(2)}B` : n >= 1e6 ? `$${(n / 1e6).toFixed(2)}M` : `$${Math.round(n).toLocaleString()}`;
  return (
    <>
      <Td align="right">
        <span className="font-mono tabular-nums text-ink-body">
          {q.isLoading ? '…' : tvl != null ? fmtUsd(tvl) : '—'}
        </span>
      </Td>
      <Td align="right">
        <span className="font-mono tabular-nums text-ink-body">
          {q.isLoading ? '…' : util != null ? `${util.toFixed(1)}%` : '—'}
        </span>
      </Td>
    </>
  );
}

  const q = useQuery<LendingPool[]>({
    queryKey: ['/v1/lending/pools'],
    queryFn: async () => {
      const env = await apiGet<{ data: LendingPool[] }>('/v1/lending/pools', {});
      return env.data ?? [];
    },
  });

  const rows = q.data ?? [];

  return (
    <Panel
      title={`Pools${rows.length > 0 ? ` (${rows.length})` : ''}`}
      hint="One row per Blend pool. TVL + utilization read live from pool storage (per-reserve USD); auctions and users from the indexed event stream."
      source={asExample('/v1/lending/pools', {})}
      bodyClassName="-mx-4"
    >
      <div className="overflow-x-auto">
        <table className="min-w-full divide-y divide-line text-sm">
          <thead>
            <tr className="text-left text-[10px] uppercase tracking-wider text-ink-muted">
              <Th>Protocol</Th>
              <Th>Pool</Th>
              <Th>Deployed</Th>
              <Th align="right">24h auctions</Th>
              <Th align="right">All-time auctions</Th>
              <Th align="right">TVL</Th>
              <Th align="right">Utilization</Th>
              <Th align="right">Users (30d)</Th>
              <Th align="right">Last activity</Th>
            </tr>
          </thead>
          <tbody className="divide-y divide-line-subtle">
            {q.isLoading && (
              <tr>
                <td colSpan={9} className="px-4 py-6 text-center text-sm text-ink-muted">
                  Loading pools…
                </td>
              </tr>
            )}
            {!q.isLoading && rows.length === 0 && (
              <tr>
                <td colSpan={9} className="px-4 py-6 text-center text-sm text-ink-muted">
                  No Blend pools have emitted auction events yet.
                </td>
              </tr>
            )}
            {rows.map((p) => {
              const poolId = p.pool ?? '';
              const meta = BLEND_POOL_META[poolId];
              return (
                <tr key={poolId} className="hover:bg-surface-muted">
                  <Td>
                    <span className="inline-block rounded-sm bg-up-subtle px-1.5 py-0.5 text-[11px] font-medium uppercase tracking-wider text-up-strong">
                      {p.protocol}
                    </span>
                  </Td>
                  <Td>
                    <div className="space-y-0.5">
                      <Link
                        href={`/lending/${poolId}`}
                        className="block font-mono text-[11px] hover:text-brand-600"
                        title={poolId}
                      >
                        {poolId.slice(0, 6)}…{poolId.slice(-6)}
                      </Link>
                      {/* Curated label where we have one; else a generic
                          "Blend pool" tag so newer/unmapped pools are still
                          identified rather than shown as a bare hash (audit
                          2026-06-19). We don't invent pool names. */}
                      <div className="text-[9px] uppercase tracking-wide text-ink-muted">
                        {meta?.label ?? 'Blend pool'}
                      </div>
                    </div>
                  </Td>
                  <Td>
                    {meta?.deployedAt ? (
                      <div className="space-y-0.5">
                        <div className="font-mono text-[11px] text-ink-body">
                          {meta.deployedAt}
                        </div>
                        {meta.initiator && (
                          <div
                            className="font-mono text-[9px] text-ink-muted"
                            title={meta.initiator}
                          >
                            by {meta.initiator.slice(0, 4)}…{meta.initiator.slice(-4)}
                          </div>
                        )}
                      </div>
                    ) : (
                      <span className="text-ink-faint">—</span>
                    )}
                  </Td>
                  <Td align="right">
                    <span className="font-mono tabular-nums text-ink-body">
                      {(p.auctions_24h ?? 0).toLocaleString()}
                    </span>
                  </Td>
                  <Td align="right">
                    <span className="font-mono tabular-nums text-ink-body">
                      {(p.auctions_total ?? 0).toLocaleString()}
                    </span>
                  </Td>
                  <PoolRealStats pool={p.pool ?? ''} />
                  <Td align="right">
                    <span className="font-mono tabular-nums text-ink-body">
                      {(p.unique_users_30d ?? 0).toLocaleString()}
                    </span>
                  </Td>
                  <Td align="right">
                    <span className="font-mono text-xs text-ink-muted">
                      {formatRelative(p.last_seen)}
                    </span>
                  </Td>
                </tr>
              );
            })}
          </tbody>
        </table>
      </div>
    </Panel>
  );
}

function Th({ children, align }: { children: React.ReactNode; align?: 'left' | 'right' }) {
  return (
    <th
      scope="col"
      className={`px-4 py-2 ${align === 'right' ? 'text-right' : 'text-left'}`}
    >
      {children}
    </th>
  );
}

function Td({ children, align }: { children: React.ReactNode; align?: 'left' | 'right' }) {
  return (
    <td className={`px-4 py-2 ${align === 'right' ? 'text-right' : 'text-left'}`}>{children}</td>
  );
}
