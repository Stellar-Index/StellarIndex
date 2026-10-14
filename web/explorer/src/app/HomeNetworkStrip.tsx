'use client';

import Link from 'next/link';
import type { ReactNode } from 'react';

import { asExample } from '@/api/client';
import {
  isOnChainSource,
  useNetworkStats,
  useSourcesEnvelope,
} from '@/api/hooks';
import { RollingNumber } from '@/components/primitives/RollingNumber';
import { RequestReveal } from '@/components/reveal/RequestReveal';
import { Stat } from '@/components/ui';
import { CURRENT_NETWORK } from '@/lib/networks';
import { cn } from '@/lib/cn';
import {
  formatCompact,
  formatCompactUnits,
  sumDecimalStrings,
} from '@/lib/format';
import { isFrameStale, useLedgerStream, useLiveClock } from '@/lib/live/hooks';

/**
 * HomeNetworkStrip — the row of five network tiles under the landing finder.
 *
 * 24h volume is the DEX-subclass sum of /v1/sources (Stellar on-chain only;
 * the /v1/network/stats total is dominated by CEX feeds that never touch the
 * chain). Markets, assets and sources come from /v1/network/stats. The ledger
 * tip follows the SSE ledger stream while fresh and falls back to the polled
 * stat. Every tile dates itself from the server's `as_of` (or the frame's
 * `ingested_at`), never from the client's fetch time, and says so when the
 * envelope is flagged stale.
 */
export function HomeNetworkStrip() {
  const stats = useNetworkStats();
  const sources = useSourcesEnvelope(undefined, true);
  const frame = useLedgerStream();
  const clock = useLiveClock();

  const net = stats.data?.data;
  const statsAt = stats.data?.as_of;
  const statsStale = stats.data?.flags?.stale === true;
  const sourceRows = sources.data?.data ?? [];
  const sourcesAt = sources.data?.as_of;
  const sourcesStale = sources.data?.flags?.stale === true;

  const dexSources = sourceRows.filter((s) => s.subclass === 'dex');
  const stellarVolume = sumDecimalStrings(
    dexSources.map((s) => s.volume_24h_usd),
  );
  // Any flagged source makes the sum a lower bound (excluded unpriced trades).
  const volumeLowerBound = dexSources.some((s) => s.volume_lower_bound);
  const volume =
    stellarVolume != null && Number(stellarVolume) > 0 ? stellarVolume : null;
  const activeMarkets = net?.markets_count_24h ?? null;
  const assetsIndexed = net?.assets_indexed ?? null;
  const exchangeSources = net?.exchange_sources ?? null;
  // The lean test nets run no off-chain feeds, so exchange_sources would be a
  // leaked mainnet count there; show the real on-chain source count instead.
  const onChainSources = sourceRows.filter(isOnChainSource).length;

  const streamLive =
    frame != null && !isFrameStale(clock, frame.receivedAt, 30_000);
  const tipLedger = streamLive
    ? frame.data.latest_ledger
    : (net?.latest_ledger ?? null);
  const tipAt = streamLive ? frame.data.ingested_at : statsAt;

  const pricing = CURRENT_NETWORK.pricing;
  return (
    <section aria-labelledby="home-network-heading" className="space-y-3">
      <div className="flex items-center justify-between">
        <h2
          id="home-network-heading"
          className="text-h3 text-ink font-semibold"
        >
          {CURRENT_NETWORK.label}
        </h2>
        <RequestReveal
          example={asExample('/v1/network/stats')}
          position="inline"
        />
      </div>
      <div
        className={cn(
          'grid grid-cols-2 gap-3 sm:gap-4',
          pricing ? 'md:grid-cols-3 lg:grid-cols-5' : 'md:grid-cols-3',
        )}
      >
        {pricing && (
          <Cell
            label="24h volume"
            value={
              volume != null
                ? `${volumeLowerBound ? '≥ ' : ''}$${formatCompactUnits(volume)}`
                : '—'
            }
            sub={volumeLowerBound ? 'on-chain, lower bound' : 'on-chain'}
            at={sourcesAt}
            stale={sourcesStale}
            href="/markets"
          />
        )}
        {pricing && (
          <Cell
            label="Active markets"
            value={
              activeMarkets != null
                ? activeMarkets.toLocaleString('en-US')
                : '—'
            }
            sub="traded in 24h"
            at={statsAt}
            stale={statsStale}
            href="/markets"
          />
        )}
        <Cell
          label="Assets indexed"
          value={assetsIndexed != null ? formatCompact(assetsIndexed) : '—'}
          sub="classic assets"
          at={statsAt}
          stale={statsStale}
          href="/assets"
        />
        <Cell
          label="Sources online"
          value={
            pricing
              ? exchangeSources != null
                ? `${exchangeSources}`
                : '—'
              : sources.data
                ? `${onChainSources}`
                : '—'
          }
          sub={pricing ? 'exchange feeds' : 'on-chain sources'}
          at={pricing ? statsAt : sourcesAt}
          stale={pricing ? statsStale : sourcesStale}
          href="/sources"
        />
        <Cell
          label="Ledger tip"
          value={
            tipLedger != null ? (
              <RollingNumber value={String(tipLedger)} />
            ) : (
              '—'
            )
          }
          sub={streamLive ? 'live' : 'ingest cursor'}
          at={tipAt}
          stale={!streamLive && statsStale}
          href="/ledgers"
          className={pricing ? 'col-span-2 md:col-span-1' : undefined}
        />
      </div>
    </section>
  );
}

function asOf(iso: string | undefined): string | null {
  if (!iso) return null;
  const t = Date.parse(iso);
  if (Number.isNaN(t)) return null;
  return `as of ${new Date(t).toISOString().slice(11, 19)} UTC`;
}

function Cell({
  label,
  value,
  sub,
  at,
  stale,
  href,
  className,
}: {
  label: string;
  value: ReactNode;
  sub: string;
  /** Server `as_of` (or frame `ingested_at`) the value was computed at. */
  at: string | undefined;
  stale: boolean;
  href: string;
  className?: string;
}) {
  const stamp = asOf(at);
  return (
    <Link
      href={href}
      className={cn(
        'border-line bg-surface hover:border-line-strong rounded-card block border p-4 transition-colors',
        className,
      )}
    >
      <Stat
        label={label}
        value={
          <span
            className="block truncate font-mono"
            title={typeof value === 'string' ? value : undefined}
          >
            {value}
          </span>
        }
        sub={
          <>
            <span className="block">
              {sub}
              {stale && <span className="text-warn-500"> · stale</span>}
            </span>
            {stamp && <span className="text-ink-faint block">{stamp}</span>}
          </>
        }
      />
    </Link>
  );
}
