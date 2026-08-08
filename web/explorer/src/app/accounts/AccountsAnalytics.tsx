'use client';

import { useQuery } from '@tanstack/react-query';

import { Panel } from '@/components/reveal';
import { Stat } from '@/components/ui';
import { HBarList, type HBarItem } from '@/components/charts/Bars';
import { AssetLink } from '@/components/AssetLink';
import { apiGet, asExample } from '@/api/client';
import { formatCompact } from '@/lib/format';
import { type Envelope, stroopsToXlm } from '../explorer-shared';

// Mirrors api/v1 explorer.AccountsStatsView (GET /v1/accounts/stats).
// Stroops values arrive as strings (ADR-0003).
interface AccountsStatsResp {
  totals: {
    accounts: number;
    trustlines: number;
    trustline_holding_accounts: number;
    xlm_held_stroops: string;
  };
  balances: {
    avg_stroops: string;
    median_stroops: string;
    p90_stroops: string;
    p99_stroops: string;
  };
  concentration: { top100_xlm_stroops: string; top100_share_pct: number };
  wealth_histogram: { bucket: number; accounts: number; xlm_stroops: string }[];
  trustline_histogram: { bucket: string; accounts: number }[];
  top_held_assets?: { asset: string; holders: number }[];
  computed_at: string;
}

// wealthBucketLabel renders the log10 bucket as its XLM range.
function wealthBucketLabel(bucket: number): string {
  if (bucket <= -1) return '< 1 XLM';
  if (bucket >= 10) return '≥ 10B XLM';
  const lo = 10 ** bucket;
  const hi = 10 ** (bucket + 1);
  return `${formatCompact(lo)}–${formatCompact(hi)} XLM`;
}

/**
 * AccountsAnalytics — the /accounts hub's insight strip (operator
 * request 2026-08-08): network totals, XLM balance statistics, wealth
 * distribution, trustlines-per-account bands, and the most-held assets.
 * All from one precomputed rollup snapshot (30-min cycle) — the panel's
 * data cost is a handful of keyed reads.
 */
export function AccountsAnalytics() {
  const q = useQuery<AccountsStatsResp>({
    queryKey: ['/v1/accounts/stats'],
    queryFn: async () => {
      const env = await apiGet<Envelope<AccountsStatsResp>>('/v1/accounts/stats');
      return env.data;
    },
    retry: false,
    refetchInterval: 10 * 60 * 1000,
  });

  const s = q.data;
  if (q.isLoading) {
    return (
      <Panel title="Network accounts" source={asExample('/v1/accounts/stats')} bodyClassName="text-sm text-ink-muted">
        Loading account analytics…
      </Panel>
    );
  }
  if (q.isError || !s) {
    return (
      <Panel title="Network accounts" source={asExample('/v1/accounts/stats')} bodyClassName="text-sm text-ink-muted">
        Account analytics are warming (the rollup runs every 30 minutes) — the
        wealth directory below is unaffected.
      </Panel>
    );
  }

  const wealthItems: HBarItem[] = s.wealth_histogram.map((b) => ({
    label: wealthBucketLabel(b.bucket),
    value: b.accounts,
    display: formatCompact(b.accounts),
    annotation: `${formatCompact(Number(stroopsToXlm(b.xlm_stroops).replace(/,/g, '')))} XLM held`,
    title: `${b.accounts.toLocaleString('en-US')} accounts`,
  }));
  const trustlineItems: HBarItem[] = s.trustline_histogram.map((b) => ({
    label: b.bucket === '0' ? 'no trustlines' : `${b.bucket} trustlines`,
    value: b.accounts,
    display: formatCompact(b.accounts),
  }));

  return (
    <>
      <Panel title="Network accounts" source={asExample('/v1/accounts/stats')} bodyClassName="space-y-5">
        <dl className="grid grid-cols-2 gap-x-6 gap-y-4 sm:grid-cols-3 lg:grid-cols-6">
          <Stat label="Funded accounts" value={formatCompact(s.totals.accounts)} />
          <Stat label="Trustlines" value={formatCompact(s.totals.trustlines)} />
          <Stat
            label="XLM held by accounts"
            value={`${formatCompact(Number(stroopsToXlm(s.totals.xlm_held_stroops).replace(/,/g, '')))} XLM`}
          />
          <Stat label="Median balance" value={`${stroopsToXlm(s.balances.median_stroops)} XLM`} />
          <Stat label="p99 balance" value={`${stroopsToXlm(s.balances.p99_stroops)} XLM`} />
          <Stat
            label="Top-100 hold"
            value={`${s.concentration.top100_share_pct.toFixed(2)}%`}
           
          />
        </dl>
        <p className="text-[11px] text-ink-muted">
          Snapshot recomputed every 30 minutes from the captured ledger state
          (last cycle {s.computed_at}). Balance statistics cover funded
          accounts&apos; native XLM.
        </p>
      </Panel>

      <div className="grid gap-6 lg:grid-cols-2">
        <Panel title="Wealth distribution" source={asExample('/v1/accounts/stats')} bodyClassName="space-y-2">
          <p className="text-xs text-ink-muted">
            Accounts by native balance, log-scale bands. Most accounts are
            small; the annotation shows each band&apos;s total XLM.
          </p>
          <HBarList items={wealthItems} ariaLabel="Accounts by XLM balance band" />
        </Panel>
        <Panel title="Trustlines per account" source={asExample('/v1/accounts/stats')} bodyClassName="space-y-2">
          <p className="text-xs text-ink-muted">
            How many assets accounts opt into holding —{' '}
            {formatCompact(s.totals.trustline_holding_accounts)} accounts hold at
            least one trustline.
          </p>
          <HBarList items={trustlineItems} ariaLabel="Accounts by trustline count band" />
        </Panel>
      </div>

      {s.top_held_assets && s.top_held_assets.length > 0 && (
        <Panel title="Most held assets" source={asExample('/v1/accounts/stats')} bodyClassName="space-y-2">
          <p className="text-xs text-ink-muted">
            Assets by number of positive-balance holders — click through for
            each asset&apos;s holder board.
          </p>
          <HBarList
            items={s.top_held_assets.map((a) => ({
              label: a.asset.split('-')[0],
              value: a.holders,
              display: formatCompact(a.holders),
              title: a.asset,
            }))}
            ariaLabel="Most held assets by holder count"
          />
          <div className="flex flex-wrap gap-x-4 gap-y-1 text-xs">
            {s.top_held_assets.map((a) => (
              <AssetLink key={a.asset} canonical={a.asset} />
            ))}
          </div>
        </Panel>
      )}
    </>
  );
}
