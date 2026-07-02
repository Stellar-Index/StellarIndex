import Link from 'next/link';
import { ArrowRight, Activity } from 'lucide-react';

import { ButtonLink, Container } from '@/components/ui';
import { HomeBlogStrip } from './HomeBlogStrip';
import { HomeCurrencies } from './HomeCurrencies';
import { HomeHeroChart } from './HomeHeroChart';
import { NetworkLivePanel, SystemHealthLivePanel } from './HomeLivePanels';
import { HomeNetworkStrip } from './HomeNetworkStrip';
import { HomeRecentChanges } from './HomeRecentChanges';
import { HomeRecentTrades } from './HomeRecentTrades';
import { HomeTopAssets } from './HomeTopAssets';
import { HomeTopMarkets } from './HomeTopMarkets';
import { HomeTopMovers } from './HomeTopMovers';
import { HomeTryAPI } from './HomeTryAPI';

export default function HomePage() {
  return (
    <Container className="space-y-12 py-10 sm:py-14">
      <header className="max-w-3xl space-y-5">
        <p className="inline-flex items-center gap-2 rounded-full border border-line bg-surface px-3 py-1 text-xs font-medium text-ink-muted">
          <span className="h-1.5 w-1.5 rounded-full bg-up" />
          Independent · open · public-tier free
        </p>
        <h1 className="text-display-sm font-semibold text-ink md:text-display">
          The protocol explorer for the Stellar network.
        </h1>
        <p className="max-w-2xl text-lg leading-relaxed text-ink-muted">
          Every contract, every event, and every trade across Stellar
          protocols — CEXes, on-chain DEXes, and lending — served as verified
          per-protocol data plus a single VWAP price through a public REST
          API, alongside live world fiat rates. Every panel below shows the
          exact API call that produced it.
        </p>
        <div className="flex flex-wrap items-center gap-3 pt-1">
          <ButtonLink href="/assets" size="lg">
            Browse assets
            <ArrowRight className="h-4 w-4" />
          </ButtonLink>
          <ButtonLink href="/pricing" variant="secondary" size="lg">
            Pricing API
          </ButtonLink>
          <ButtonLink href="https://docs.stellarindex.io" variant="secondary" size="lg">
            API docs
          </ButtonLink>
          <Link
            href="/methodology"
            className="px-2 text-sm font-medium text-ink-muted transition-colors hover:text-brand-600"
          >
            How it works →
          </Link>
        </div>
      </header>

      <HomeNetworkStrip />

      <HomeHeroChart />

      <section className="grid grid-cols-1 gap-4 lg:grid-cols-3">
        <NetworkLivePanel />
        <SystemHealthLivePanel />
        <Link
          href="/diagnostics"
          className="group flex h-full flex-col justify-between rounded-card border border-line bg-surface p-5 shadow-card transition-all hover:border-line-strong hover:shadow-elevated"
        >
          <div>
            <p className="flex items-center gap-1.5 text-[11px] font-medium uppercase tracking-wider text-ink-muted">
              <Activity className="h-3.5 w-3.5 text-ink-faint" />
              Diagnostics
            </p>
            <p className="mt-2 text-h3 font-semibold text-ink">
              Watch the indexer tick.
            </p>
            <p className="mt-1 text-sm text-ink-muted">
              Per-source ingest cursors, refreshed every 15 seconds — see
              every backfill chunk advance in real time.
            </p>
          </div>
          <p className="mt-4 inline-flex items-center gap-1 text-sm font-medium text-brand-600">
            Open diagnostics{' '}
            <ArrowRight className="h-3.5 w-3.5 transition-transform group-hover:translate-x-0.5" />
          </p>
        </Link>
      </section>

      <HomeTopAssets />

      <HomeCurrencies />

      <HomeTopMarkets />

      <HomeTopMovers />

      <HomeRecentTrades />

      <HomeRecentChanges />

      <HomeBlogStrip />

      {/* LC-060: the flagship pricing-API product, presented as a product —
          plans, keys, and where to start. */}
      <section className="rounded-card border border-line bg-surface p-6 shadow-card sm:p-8">
        <div className="grid grid-cols-1 gap-8 lg:grid-cols-2 lg:items-center">
          <div className="space-y-4">
            <p className="text-xs font-medium uppercase tracking-wider text-brand-600">
              Pricing API
            </p>
            <h2 className="text-h2 font-semibold text-ink">
              One verified price for every Stellar pair.
            </h2>
            <p className="text-[15px] leading-relaxed text-ink-muted">
              The flagship product behind this explorer: VWAP, TWAP, and OHLC
              computed from every CEX, DEX, and oracle we index, served over
              REST + SSE with deterministic closed-bucket semantics. Anonymous
              reads are free forever; an API key raises your rate limit from
              60 to 1,000+ requests a minute, with tiers and SLAs beyond that.
            </p>
            <div className="flex flex-wrap items-center gap-3 pt-1">
              <ButtonLink href="/signup">
                Get an API key
                <ArrowRight className="h-4 w-4" />
              </ButtonLink>
              <ButtonLink href="/pricing" variant="secondary">
                Plans &amp; rate limits
              </ButtonLink>
              <Link
                href="/docs"
                className="px-2 text-sm font-medium text-ink-muted transition-colors hover:text-brand-600"
              >
                Quickstart →
              </Link>
            </div>
          </div>
          <dl className="divide-y divide-line rounded-lg border border-line">
            {[
              ['GET /v1/price', 'Latest closed-bucket VWAP for any pair.'],
              ['GET /v1/price/tip', 'Rolling live price, sub-minute freshness.'],
              ['GET /v1/ohlc', 'Candles — daily history back to 2015.'],
              ['GET /v1/price/stream', 'Server-Sent Events push feed.'],
            ].map(([ep, desc]) => (
              <div
                key={ep}
                className="flex flex-col gap-1 px-4 py-2.5 sm:flex-row sm:items-baseline sm:gap-4"
              >
                <dt className="font-mono text-[13px] text-ink sm:w-44 sm:shrink-0">
                  {ep}
                </dt>
                <dd className="text-sm text-ink-muted">{desc}</dd>
              </div>
            ))}
          </dl>
        </div>
      </section>

      <section className="space-y-4">
        <div className="space-y-1">
          <h2 className="text-h2 font-semibold text-ink">Try the API</h2>
          <p className="text-[15px] text-ink-muted">
            The free public tier needs no key — pick an example and paste it
            straight into a terminal. An{' '}
            <Link href="/pricing" className="font-medium text-brand-600 hover:underline">
              API key
            </Link>{' '}
            lifts the rate limit when you outgrow it.
          </p>
        </div>
        <HomeTryAPI />
      </section>
    </Container>
  );
}
