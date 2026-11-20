'use client';

import { useQuery } from '@tanstack/react-query';

import { Panel } from '@/components/reveal';
import { AssetLink } from '@/components/AssetLink';
import { InlineMeter } from '@/components/charts/Bars';
import { Button } from '@/components/ui';
import { Fact, FactList } from '@/components/ui/Facts';
import { apiGet, asExample } from '@/api/client';
import type { paths } from '@/api/types';
import {
  formatCompact,
  formatCompactUnits,
  formatReadable,
  ratioPct,
  sumDecimalStrings,
} from '@/lib/format';
import { scaledUnits } from '../../explorer-shared';
import { fixedPct } from '../poolConfig';
import {
  amountText,
  BACKSTOP_ASSET_LABELS,
  carryForward,
  isHistoryBuilding,
  tierLabel,
} from '../lendingInsights';
import { HistoryChart } from './HistoryChart';

type Resp =
  paths['/lending/pools/{pool}/backstop']['get']['responses'][200]['content']['application/json'];
type HistResp =
  paths['/lending/pools/{pool}/backstop/history']['get']['responses'][200]['content']['application/json'];
type Tier = NonNullable<Resp['data']>['tiers'][number];

// The API gives no decimals for backstop tokens and shares; they use the
// 7-decimal Soroban token scale.
const BACKSTOP_DECIMALS = 7;

const UNAVAILABLE: Record<string, string> = {
  blend_v1_unsupported:
    'This is a Blend V1 pool. Its backstop is stored in a way we do not read yet.',
  pool_lineage_unknown:
    'We could not tell which protocol version this pool runs, so its backstop is not shown.',
  pool_instance_unavailable:
    "The pool's contract data is not available yet, so its backstop is not shown.",
};

function BackstopHistory({ pool }: { pool: string }) {
  const q = useQuery({
    queryKey: ['/v1/lending/pools/{pool}/backstop/history', pool, '90d'],
    retry: false,
    staleTime: 60_000,
    queryFn: async () => {
      const env = await apiGet<HistResp>(
        `/v1/lending/pools/${encodeURIComponent(pool)}/backstop/history`,
        { interval: '1d', window: '90d' },
      );
      return env.data;
    },
  });
  if (q.isLoading)
    return <p className="text-ink-muted text-sm">Loading history…</p>;
  if (q.isError)
    return isHistoryBuilding(q.error) ? (
      <p className="text-ink-muted text-sm">
        History is still being built for this backstop. Check back soon.
      </p>
    ) : (
      <p className="text-ink-muted text-sm">
        Backstop history is unavailable right now.
        <Button
          variant="secondary"
          size="sm"
          className="ml-3"
          onClick={() => void q.refetch()}
        >
          Retry
        </Button>
      </p>
    );
  const d = q.data;
  if (!d) return null;
  return (
    <div
      className={
        d.tiers.length > 2
          ? 'grid gap-4 lg:grid-cols-3'
          : d.tiers.length === 2
            ? 'grid gap-4 lg:grid-cols-2'
            : 'space-y-4'
      }
    >
      {d.tiers.map((t) => {
        const label = tierLabel(t.tier) ?? 'Backstop';
        const series = carryForward(
          t.points.map((p) => {
            const n =
              p.tokens == null ? NaN : scaledUnits(p.tokens, BACKSTOP_DECIMALS);
            return {
              t: p.t,
              value: p.removed || !Number.isFinite(n) ? null : n,
            };
          }),
          '1d',
          d.from,
          d.history_through_time && d.history_through_time < d.to
            ? d.history_through_time
            : d.to,
        );
        return (
          <div key={t.tier ?? 'single'} className="space-y-1.5">
            <h3 className="text-ink text-sm font-medium">
              {label} size, last 90 days
            </h3>
            {t.before_first_state && (
              <p className="text-ink-muted text-xs">
                No recorded balance before the first point shown.
              </p>
            )}
            {t.points.length === 0 ? (
              <p className="text-ink-muted text-sm">
                No balance changes recorded in this range.
              </p>
            ) : (
              <HistoryChart
                points={series}
                label={`${label} tokens`}
                formatValue={formatCompact}
              />
            )}
          </div>
        );
      })}
      {d.history_through_time && (
        <p className="text-ink-muted text-xs lg:col-span-full">
          Data through {d.history_through_time.slice(0, 16).replace('T', ' ')}{' '}
          UTC. Each value holds until the next change.
        </p>
      )}
    </div>
  );
}

// `tokens_usd` ships in a later API release; read it when the response has it.
type TierUsd = Tier & { tokens_usd?: string | null };

const assetKey = (t: Tier) => t.backstop_token ?? t.backstop_asset ?? '';

function AssetName({ tier }: { tier: Tier }) {
  const named =
    tier.backstop_asset && BACKSTOP_ASSET_LABELS[tier.backstop_asset];
  if (named)
    return <span title={tier.backstop_token ?? undefined}>{named}</span>;
  if (tier.backstop_token) return <AssetLink canonical={tier.backstop_token} />;
  return <>{tier.backstop_asset ?? '—'}</>;
}

function TierFacts({ tier, many }: { tier: TierUsd; many: boolean }) {
  const label = tierLabel(tier.tier);
  const tokens =
    tier.tokens == null
      ? null
      : formatCompactUnits(tier.tokens, BACKSTOP_DECIMALS);
  const usd = formatReadable(tier.tokens_usd, true);
  return (
    <div className="border-line space-y-2 rounded-xs border p-3">
      <div className="flex flex-wrap items-baseline justify-between gap-2">
        {many && label && (
          <h3 className="text-ink text-sm font-medium">{label}</h3>
        )}
        {tier.take_rate_weight != null && (
          <span className="text-ink-muted text-xs">
            take-rate weight {tier.take_rate_weight}
          </span>
        )}
      </div>
      <div
        className="text-ink font-mono text-xl tabular-nums"
        title={
          tier.tokens != null
            ? `${amountText(tier.tokens, BACKSTOP_DECIMALS)} tokens`
            : undefined
        }
      >
        {usd ?? tokens ?? '—'}
      </div>
      <div className="text-ink-muted text-xs">
        {usd && tokens ? `${tokens} ` : ''}
        <AssetName tier={tier} />
      </div>
      {tier.queued_pct != null && Number.isFinite(tier.queued_pct) && (
        <div
          className="text-ink-muted flex items-center gap-2 text-xs"
          title={
            tier.queued_shares != null
              ? `${amountText(tier.queued_shares, BACKSTOP_DECIMALS)} shares queued`
              : undefined
          }
        >
          <InlineMeter
            frac={tier.queued_pct / 100}
            label={`${tier.queued_pct.toFixed(1)}% of shares queued for withdrawal`}
            color="var(--color-chart-4)"
            className="w-20"
          />
          <span>
            <span className="text-ink font-mono tabular-nums">
              {tier.queued_pct.toFixed(1)}%
            </span>{' '}
            queued for withdrawal
          </span>
        </div>
      )}
    </div>
  );
}

const TIER_COLORS = [
  'var(--color-chart-1)',
  'var(--color-chart-3)',
  'var(--color-chart-4)',
];

/** Relative tier size; drawn only when every tier is the same asset, so the sizes compare. */
function TierShare({ tiers }: { tiers: Tier[] }) {
  if (tiers.length < 2) return null;
  const key = assetKey(tiers[0]);
  if (!key || tiers.some((t) => assetKey(t) !== key || t.tokens == null))
    return null;
  const total = sumDecimalStrings(tiers.map((t) => t.tokens));
  const parts = tiers.map((t, i) => ({
    label: tierLabel(t.tier) ?? `Tier ${i + 1}`,
    pct: ratioPct(t.tokens, total, 1) ?? 0,
    color: TIER_COLORS[i % TIER_COLORS.length],
  }));
  return (
    <div className="space-y-1.5">
      <div
        role="img"
        aria-label={`Tier sizes: ${parts.map((p) => `${p.label} ${p.pct}%`).join(', ')}`}
        className="flex h-2 w-full gap-px overflow-hidden rounded-xs"
      >
        {parts.map((p) => (
          <span
            key={p.label}
            className="min-w-[3px]"
            style={{ width: `${p.pct}%`, backgroundColor: p.color }}
          />
        ))}
      </div>
      <ul className="text-ink-muted flex flex-wrap gap-x-4 text-xs">
        {parts.map((p) => (
          <li key={p.label} className="flex items-center gap-1.5">
            <span
              aria-hidden
              className="inline-block h-2 w-2 rounded-xs"
              style={{ backgroundColor: p.color }}
            />
            {p.label}{' '}
            <span className="text-ink font-mono tabular-nums">{p.pct}%</span>
          </li>
        ))}
      </ul>
    </div>
  );
}

export function BackstopPanel({ pool }: { pool: string }) {
  const q = useQuery({
    queryKey: ['/v1/lending/pools/{pool}/backstop', pool],
    retry: false,
    staleTime: 60_000,
    queryFn: async () => {
      const env = await apiGet<Resp>(
        `/v1/lending/pools/${encodeURIComponent(pool)}/backstop`,
        {},
      );
      return env.data;
    },
  });
  const d = q.data;

  return (
    <Panel
      headingLevel={2}
      title="Backstop"
      hint="The backstop is capital set aside by depositors to absorb the pool's bad debt first, before lenders lose anything. In return, it earns a share of the interest borrowers pay."
      source={asExample(`/v1/lending/pools/${pool}/backstop`, {})}
      bodyClassName="space-y-4"
    >
      {q.isLoading && (
        <p className="text-ink-muted text-sm">Loading backstop…</p>
      )}
      {q.isError && (
        <p className="text-ink-muted text-sm">
          Backstop data is unavailable right now.
          <Button
            variant="secondary"
            size="sm"
            className="ml-3"
            onClick={() => void q.refetch()}
          >
            Retry
          </Button>
        </p>
      )}
      {d && !d.available && (
        <p className="text-ink-muted text-sm">
          {(d.unavailable_reason && UNAVAILABLE[d.unavailable_reason]) ??
            'Backstop data is not available for this pool.'}
        </p>
      )}
      {d?.available && (
        <>
          <FactList columns={3}>
            <Fact label="Take rate" title={d.take_rate ?? undefined}>
              {fixedPct(d.take_rate, d.config_decimals)}
            </Fact>
          </FactList>
          {d.tiers.length === 0 && (
            <p className="text-ink-muted text-sm">
              No backstop balances recorded for this pool.
            </p>
          )}
          <TierShare tiers={d.tiers} />
          <div className="grid gap-3 md:grid-cols-3">
            {d.tiers.map((t) => (
              <TierFacts
                key={t.tier ?? 'single'}
                tier={t}
                many={d.tiers.length > 1}
              />
            ))}
          </div>
          {d.tiers.length > 0 && <BackstopHistory pool={pool} />}
        </>
      )}
    </Panel>
  );
}
