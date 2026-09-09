import Link from 'next/link';
import { ArrowRight, Activity } from 'lucide-react';

import { ButtonLink, Container } from '@/components/ui';
import { CURRENT_NETWORK } from '@/lib/networks';
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
  // Pricing-derived home panels (XLM/USD hero chart, fiat-rate strip, USD
  // "top markets"/"top movers") have no data on the lean test nets — they
  // render empty grids / retry-storm the pricing endpoints. Hide them there;
  // the chain-native panels below stay.
  const pricing = CURRENT_NETWORK.pricing;
  // "Top assets" needs the network to HAVE issued assets. That was an
  // `id === 'futurenet'` special case living on the homepage alone (#328),
  // so the nav still offered the same empty /assets + /sdex surfaces; the
  // fact is now a NetworkInfo capability every surface reads.
  // (Recent trades also seeds pairs from the empty /v1/markets on testnet, so
  // gate it on pricing; the contracts/ledgers/accounts surfaces stay on both.)
  const hasAssets = CURRENT_NETWORK.hasAssets;
  return (
    <Container className="space-y-12 py-10 sm:py-14">
      <header className="max-w-3xl space-y-5">
        <p className="border-line bg-surface text-ink-muted inline-flex items-center gap-2 rounded-full border px-3 py-1 text-xs font-medium">
          <span className="bg-up h-1.5 w-1.5 rounded-full" />
          Independent · open · public-tier free
        </p>
        <h1 className="text-display-sm text-ink md:text-display font-semibold">
          {pricing
            ? 'The protocol explorer for the Stellar network.'
            : `The Stellar ${CURRENT_NETWORK.label} Explorer`}
        </h1>
        <p className="text-ink-muted max-w-2xl text-lg leading-relaxed">
          {pricing ? (
            <>
              Every contract, every event, and every trade across Stellar
              protocols — CEXes, on-chain DEXes, and lending — served as
              verified per-protocol data plus a single VWAP price through a
              public REST API, alongside live world fiat rates. Every panel
              below shows the exact API call that produced it.
            </>
          ) : (
            <>
              Every ledger, transaction, account, asset, and Soroban contract on
              Stellar {CURRENT_NETWORK.label} — complete, verified, per-protocol
              on-chain data through a public REST API. Every panel below shows
              the exact API call that produced it.
            </>
          )}
        </p>
        <div className="flex flex-wrap items-center gap-3 pt-1">
          <ButtonLink href="/assets" size="lg">
            Browse assets
            <ArrowRight className="h-4 w-4" />
          </ButtonLink>
          <ButtonLink href="/pricing" variant="secondary" size="lg">
            Pricing
          </ButtonLink>
          <ButtonLink
            href="https://docs.stellarindex.io"
            variant="secondary"
            size="lg"
          >
            API docs
          </ButtonLink>
          <Link
            href="/methodology"
            className="text-ink-muted hover:text-brand-600 px-2 text-sm font-medium transition-colors"
          >
            How it works →
          </Link>
        </div>
      </header>

      <HomeNetworkStrip />

      {pricing && <HomeHeroChart />}

      <section className="grid grid-cols-1 gap-4 lg:grid-cols-3">
        <NetworkLivePanel />
        <SystemHealthLivePanel />
        <Link
          href="/diagnostics"
          className="group rounded-card border-line bg-surface shadow-card hover:border-line-strong hover:shadow-elevated flex h-full flex-col justify-between border p-5 transition-all"
        >
          <div>
            <p className="text-ink-muted flex items-center gap-1.5 text-[11px] font-medium tracking-wider uppercase">
              <Activity className="text-ink-faint h-3.5 w-3.5" />
              Diagnostics
            </p>
            <p className="text-h3 text-ink mt-2 font-semibold">
              Watch the indexer tick.
            </p>
            <p className="text-ink-muted mt-1 text-sm">
              Per-source ingest cursors, refreshed every 15 seconds — see every
              backfill chunk advance in real time.
            </p>
          </div>
          <p className="text-brand-600 mt-4 inline-flex items-center gap-1 text-sm font-medium">
            Open diagnostics{' '}
            <ArrowRight className="h-3.5 w-3.5 transition-transform group-hover:translate-x-0.5" />
          </p>
        </Link>
      </section>

      {hasAssets && <HomeTopAssets />}

      {pricing && <HomeCurrencies />}

      {pricing && <HomeTopMarkets />}

      {pricing && <HomeTopMovers />}

      {pricing && <HomeRecentTrades />}

      <HomeRecentChanges />

      <HomeBlogStrip />

      {/* LC-060: the flagship API product, presented as a product —
          plans, keys, and where to start. */}
      <section className="rounded-card border-line bg-surface shadow-card border p-6 sm:p-8">
        <div className="grid grid-cols-1 gap-8 lg:grid-cols-2 lg:items-center">
          <div className="space-y-4">
            <p className="text-brand-600 text-xs font-medium tracking-wider uppercase">
              Stellar Index API
            </p>
            <h2 className="text-h2 text-ink font-semibold">
              One verified price for every Stellar pair.
            </h2>
            <p className="text-ink-muted text-[15px] leading-relaxed">
              The API behind this explorer — the same data, machine-readable.
              VWAP, TWAP, and OHLC computed from every CEX, DEX, and oracle we
              index, served over REST + SSE with deterministic closed-bucket
              semantics, alongside the ledger, contract, asset, supply and
              history endpoints. Anonymous reads are free forever at 6,000
              requests a minute per IP; an API key is a per-key budget of 1,000
              a minute — yours alone rather than shared with every client on
              your IP — with staff-set partner limits above that.
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
                className="text-ink-muted hover:text-brand-600 px-2 text-sm font-medium transition-colors"
              >
                Quickstart →
              </Link>
            </div>
          </div>
          <dl className="divide-line border-line divide-y rounded-lg border">
            {[
              ['GET /v1/price', 'Latest closed-bucket VWAP for any pair.'],
              [
                'GET /v1/price/tip',
                'Rolling live price, sub-minute freshness.',
              ],
              [
                'GET /v1/ohlc',
                'Candles — daily bars from 2018; coverage not yet continuous.',
              ],
              ['GET /v1/price/stream', 'Server-Sent Events push feed.'],
            ].map(([ep, desc]) => (
              <div
                key={ep}
                className="flex flex-col gap-1 px-4 py-2.5 sm:flex-row sm:items-baseline sm:gap-4"
              >
                <dt className="text-ink font-mono text-[13px] sm:w-44 sm:shrink-0">
                  {ep}
                </dt>
                <dd className="text-ink-muted text-sm">{desc}</dd>
              </div>
            ))}
          </dl>
        </div>
      </section>

      <section className="space-y-4">
        <div className="space-y-1">
          <h2 className="text-h2 text-ink font-semibold">Try the API</h2>
          <p className="text-ink-muted text-[15px]">
            The free public tier needs no key — pick an example and paste it
            straight into a terminal. An{' '}
            <Link
              href="/pricing"
              className="text-brand-600 font-medium hover:underline"
            >
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
