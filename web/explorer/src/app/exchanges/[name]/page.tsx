import type { Metadata } from 'next';
import Link from 'next/link';
import { notFound } from 'next/navigation';
import { ArrowLeft, ExternalLink } from 'lucide-react';

import { Panel } from '@/components/reveal';
import { asExample } from '@/api/client';
import { SourceSparkline } from '@/components/SourceSparkline';
import { PairsTable } from './PairsTable';

const API_BASE_URL =
  process.env.NEXT_PUBLIC_API_BASE_URL ?? 'https://api.ratesengine.net';

const isCIStub =
  API_BASE_URL.includes('.invalid') || API_BASE_URL.includes('local-stub');

const BUILD_FETCH_TIMEOUT_MS = 8_000;

const CEX_INFO: Record<
  string,
  { name: string; type: string; homepage: string; docsUrl: string; blurb: string }
> = {
  binance: {
    name: 'Binance',
    type: 'CEX — REST + WebSocket spot tickers',
    homepage: 'https://www.binance.com',
    docsUrl: 'https://github.com/binance/binance-spot-api-docs',
    blurb:
      'Spot trading pairs against XLM. We poll Binance ticker streams for trade events; usd_volume is computed Phase-1-style from USD-pegged quotes (USDT, BUSD, USDC).',
  },
  coinbase: {
    name: 'Coinbase',
    type: 'CEX — Advanced Trade WebSocket',
    homepage: 'https://www.coinbase.com',
    docsUrl: 'https://docs.cloud.coinbase.com/advanced-trade-api',
    blurb:
      'XLM spot pairs from Coinbase Advanced Trade — direct USD quote for usd_volume populates with no FX leg. The market-data feed dropped 0-quote-amount canonical-validator violations after the fix in PR #49.',
  },
  kraken: {
    name: 'Kraken',
    type: 'CEX — public WebSocket trades',
    homepage: 'https://www.kraken.com',
    docsUrl: 'https://docs.kraken.com/websockets',
    blurb:
      'Kraken spot pairs against USD and EUR. Forex factor (X2.5) snaps EUR pairs into USD-equivalent volume.',
  },
  bitstamp: {
    name: 'Bitstamp',
    type: 'CEX — public WebSocket trades',
    homepage: 'https://www.bitstamp.net',
    docsUrl: 'https://www.bitstamp.net/websocket/v2/',
    blurb:
      'Long-running USD-quoted XLM pairs. Smaller volume share than Binance/Coinbase but contributes to the cross-CEX VWAP weighting.',
  },
};

type Params = Promise<{ name: string }>;

export function generateStaticParams() {
  return Object.keys(CEX_INFO).map((name) => ({ name }));
}

export async function generateMetadata({
  params,
}: {
  params: Params;
}): Promise<Metadata> {
  const { name } = await params;
  const info = CEX_INFO[name];
  if (!info) return { title: 'Exchange not found' };
  return {
    title: `${info.name} — every pair, live`,
    description: `All ${info.name} pairs observed in the last 14 days, with per-pair 24h trade count + last trade. Source: /v1/markets?source=${name}.`,
  };
}

interface VolumeBucket {
  hour: string;
  volume_usd: string;
}

interface SourceStats {
  trade_count_24h?: number;
  volume_24h_usd?: string;
  markets_count_24h?: number;
  volume_history_24h?: VolumeBucket[];
}

async function fetchSourceStats(name: string): Promise<SourceStats | null> {
  if (isCIStub) return null;
  try {
    const res = await fetch(`${API_BASE_URL}/v1/sources?include=stats,sparkline`, {
      signal: AbortSignal.timeout(BUILD_FETCH_TIMEOUT_MS),
    });
    if (!res.ok) return null;
    const env = (await res.json()) as { data?: Array<{ name: string } & SourceStats> };
    return env.data?.find((r) => r.name === name) ?? null;
  } catch {
    return null;
  }
}

export default async function ExchangeDetailPage({
  params,
}: {
  params: Params;
}) {
  const { name } = await params;
  const info = CEX_INFO[name];
  if (!info) notFound();

  const stats = await fetchSourceStats(name);
  const trades = stats?.trade_count_24h ?? 0;
  const volume = stats?.volume_24h_usd ? Number(stats.volume_24h_usd) : 0;
  const markets = stats?.markets_count_24h ?? 0;

  return (
    <div className="mx-auto max-w-7xl space-y-6 px-6 py-8">
      <Link
        href="/exchanges"
        className="inline-flex items-center gap-1.5 text-sm text-slate-600 hover:text-brand-600 dark:text-slate-400"
      >
        <ArrowLeft className="h-3.5 w-3.5" />
        All exchanges
      </Link>

      <header className="space-y-2 border-b border-slate-200 pb-4 dark:border-slate-800">
        <div className="flex flex-wrap items-baseline gap-3">
          <h1 className="text-3xl font-semibold tracking-tight">{info.name}</h1>
          <span className="rounded bg-slate-100 px-1.5 py-0.5 text-[10px] uppercase tracking-wider text-slate-600 dark:bg-slate-800 dark:text-slate-400">
            {info.type}
          </span>
        </div>
        <p className="max-w-3xl text-sm text-slate-600 dark:text-slate-400">{info.blurb}</p>
      </header>

      <Panel
        title="24h activity"
        hint={`Live from /v1/sources?include=stats,sparkline (source=${name})`}
        source={asExample('/v1/sources', { include: 'stats,sparkline' })}
      >
        <div className="grid grid-cols-2 gap-3 sm:grid-cols-3">
          <Stat label="24h volume" value={volume > 0 ? `$${formatCompact(volume)}` : '—'} />
          <Stat label="24h trades" value={trades > 0 ? formatCompact(trades) : '—'} />
          <Stat label="24h pairs" value={markets > 0 ? markets.toLocaleString() : '—'} />
        </div>
        {stats?.volume_history_24h && stats.volume_history_24h.length > 0 && (
          <div className="mt-4 border-t border-slate-200 pt-3 dark:border-slate-800">
            <div className="text-[10px] uppercase tracking-wider text-slate-500">
              Volume by hour
            </div>
            <div className="mt-2">
              <SourceSparkline buckets={stats.volume_history_24h} width={400} height={48} />
            </div>
          </div>
        )}
      </Panel>

      <PairsTable source={name} exchangeName={info.name} />

      <div className="flex flex-wrap gap-3 text-xs">
        <Link
          href={`/sources/${name}`}
          className="inline-flex items-center gap-1 text-slate-500 hover:text-brand-600"
        >
          Source registry detail →
        </Link>
        <a
          href={info.homepage}
          target="_blank"
          rel="noreferrer noopener"
          className="inline-flex items-center gap-1 text-slate-500 hover:underline"
        >
          {info.name} homepage
          <ExternalLink className="h-3 w-3" />
        </a>
        <a
          href={info.docsUrl}
          target="_blank"
          rel="noreferrer noopener"
          className="inline-flex items-center gap-1 text-slate-500 hover:underline"
        >
          API docs
          <ExternalLink className="h-3 w-3" />
        </a>
      </div>
    </div>
  );
}

function Stat({ label, value }: { label: string; value: string }) {
  return (
    <div>
      <div className="text-[10px] uppercase tracking-wider text-slate-500">{label}</div>
      <div className="mt-1 text-2xl font-semibold tabular-nums">{value}</div>
    </div>
  );
}

function formatCompact(n: number): string {
  if (n >= 1e9) return `${(n / 1e9).toFixed(2)}B`;
  if (n >= 1e6) return `${(n / 1e6).toFixed(2)}M`;
  if (n >= 1e3) return `${(n / 1e3).toFixed(1)}K`;
  return n.toLocaleString();
}
