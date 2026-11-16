'use client';

import { useState } from 'react';
import { useQuery } from '@tanstack/react-query';

import { Panel } from '@/components/reveal';
import { AssetLink } from '@/components/AssetLink';
import { Button } from '@/components/ui';
import { Fact, FactList } from '@/components/ui/Facts';
import { apiGet, asExample } from '@/api/client';
import type { paths } from '@/api/types';
import { formatCompact } from '@/lib/format';
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
    <div className="space-y-4">
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
        <p className="text-ink-muted text-xs">
          Data through {d.history_through_time.slice(0, 16).replace('T', ' ')}{' '}
          UTC. Each value holds until the next change.
        </p>
      )}
    </div>
  );
}

function TierFacts({ tier, many }: { tier: Tier; many: boolean }) {
  const label = tierLabel(tier.tier);
  const dash = '—';
  return (
    <div className="space-y-2">
      {many && label && (
        <h3 className="text-ink text-sm font-medium">{label}</h3>
      )}
      <FactList columns={3}>
        <Fact label="Backstop tokens" title={tier.tokens ?? undefined}>
          {tier.tokens == null
            ? dash
            : amountText(tier.tokens, BACKSTOP_DECIMALS)}
        </Fact>
        <Fact label="Shares" title={tier.shares ?? undefined}>
          {tier.shares == null
            ? dash
            : amountText(tier.shares, BACKSTOP_DECIMALS)}
        </Fact>
        <Fact
          label="Queued for withdrawal"
          title={tier.queued_shares ?? undefined}
        >
          {tier.queued_shares == null
            ? dash
            : `${amountText(tier.queued_shares, BACKSTOP_DECIMALS)} shares${
                tier.queued_pct != null
                  ? ` (${tier.queued_pct.toFixed(2)}%)`
                  : ''
              }`}
        </Fact>
        {tier.backstop_asset && (
          <Fact label="Backstop asset">
            {tier.backstop_token ? (
              <AssetLink canonical={tier.backstop_token} />
            ) : (
              (BACKSTOP_ASSET_LABELS[tier.backstop_asset] ??
              tier.backstop_asset)
            )}
          </Fact>
        )}
        {tier.take_rate_weight != null && (
          <Fact label="Take-rate weight">{tier.take_rate_weight}</Fact>
        )}
      </FactList>
    </div>
  );
}

export function BackstopPanel({ pool }: { pool: string }) {
  const [showHistory, setShowHistory] = useState(false);
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
          {d.tiers.map((t) => (
            <TierFacts
              key={t.tier ?? 'single'}
              tier={t}
              many={d.tiers.length > 1}
            />
          ))}
          {d.tiers.length > 0 &&
            (showHistory ? (
              <BackstopHistory pool={pool} />
            ) : (
              <Button
                variant="secondary"
                size="sm"
                onClick={() => setShowHistory(true)}
              >
                Show size over time
              </Button>
            ))}
        </>
      )}
    </Panel>
  );
}
