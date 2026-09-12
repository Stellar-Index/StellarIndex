import type { Metadata } from 'next';
import Link from 'next/link';

export const metadata: Metadata = {
  title: 'Methodology — how Stellar Index computes prices',
  description:
    'Source classes, VWAP weighting, stablecoin proxy, freeze policy, closed-bucket contract, latency targets. The full methodology behind every price Stellar Index serves.',
  alternates: { canonical: '/methodology' },
};

export default function MethodologyPage() {
  return (
    <div className="mx-auto max-w-4xl space-y-10 px-6 py-10">
      <header className="space-y-3">
        <h1 className="text-3xl font-semibold tracking-tight">Methodology</h1>
        <p className="text-ink-body text-base">
          How every price Stellar Index serves is computed, from raw on-chain
          event to the final aggregate. Each section links to the underlying ADR
          for the full rationale, alternatives considered, and consequences.
        </p>
      </header>

      <TableOfContents />

      <Section
        id="sources"
        title="Source classes"
        subtitle="What gets included in the VWAP — and what doesn't"
      >
        <p>
          Every venue we ingest from is tagged with one of seven source{' '}
          <em>classes</em>. The class determines whether a venue contributes
          price observations to the aggregate or is reported alongside as
          context. The first four carry a price; the last three are indexed for
          flow and state, and never reach the aggregate.
        </p>
        <DefList
          rows={[
            {
              term: 'exchange',
              def: 'Real trading venues — DEXes (Soroswap, Phoenix, Aquarius, Comet, sdex), CEXes (Coinbase, Binance, Kraken, Bitstamp), FX vendors. These are the only sources that contribute to the VWAP. Subdivided into dex / cex / fx for grouping.',
            },
            {
              term: 'aggregator',
              def: 'Third parties (CoinGecko, CoinMarketCap) that already aggregate the same upstream venues. Including them in our VWAP would double-count. We surface their numbers separately for divergence checks.',
            },
            {
              term: 'oracle',
              def: 'Reflector, Band, Redstone, Chainlink. Each runs its own methodology — adding their output to our VWAP would impose theirs on top of ours. We surface them as parallel readings + use them for cross-checks.',
            },
            {
              term: 'authority_sanity',
              def: 'A small set of Stellar-blessed reference points (anchor home-domains, canonical fiat rates) used as sanity bounds, not price input. Catches catastrophic drift.',
            },
            {
              term: 'lending',
              def: 'On-chain lending protocols (Blend, SoroCredit). Their events are directional state changes — supply, borrow, liquidation auction, bad debt — taken on top of some other oracle’s price rather than new price observations. Surfaced as a secondary validation and protocol-health surface.',
            },
            {
              term: 'router',
              def: 'Soroban DEX routers and aggregator vaults (soroswap-router, defindex, upshift). They emit no independent trades — they invoke the contracts that do — so counting them would double-count the underlying pool’s swap. Indexed for per-transaction attribution (which router drove this swap) and requested-vs-realised path.',
            },
            {
              term: 'bridge',
              def: 'Cross-chain transfer protocols (Circle CCTP, Rozo). These move tokens between chains rather than exchanging them at a price: a deposit_for_burn on Stellar plus a mint_and_withdraw elsewhere is one logical transfer, not a two-leg trade. Surfaced for cross-chain flow attribution and USDC supply accounting.',
            },
          ]}
        />
        <Aside>
          The full per-venue registry — including{' '}
          <code className="bg-surface-subtle rounded-sm px-1 py-0.5 text-xs">
            include_in_vwap
          </code>
          ,{' '}
          <code className="bg-surface-subtle rounded-sm px-1 py-0.5 text-xs">
            paid
          </code>
          ,{' '}
          <code className="bg-surface-subtle rounded-sm px-1 py-0.5 text-xs">
            backfill_safe
          </code>
          , and 24h trade counts — is at{' '}
          <Link href="/sources" className="text-brand-600 hover:underline">
            /sources
          </Link>
          .
        </Aside>
      </Section>

      <Section
        id="vwap"
        title="VWAP weighting"
        subtitle="Volume-weighted average across all eligible exchange-class trades"
      >
        <p>For an asset pair (BASE/QUOTE) over a window, the VWAP is:</p>
        <Formula>
          VWAP = Σ(price<sub>i</sub> × volume<sub>i</sub>) / Σ(volume
          <sub>i</sub>)
        </Formula>
        <p>
          where each trade i is from a source with class ={' '}
          <code className="bg-surface-subtle rounded-sm px-1 py-0.5 text-xs">
            exchange
          </code>
          . No per-venue weighting tier or boost — the weight is the
          trade&apos;s quote-side volume, period. A million dollars of XLM/USD
          trading at $0.12 on Coinbase counts the same as a million dollars of
          XLM/USD trading at $0.12 on Soroswap.
        </p>
        <p>
          <strong>The VWAP is not outlier-filtered by default.</strong>{' '}
          <code className="bg-surface-subtle rounded-sm px-1 py-0.5 text-xs">
            /v1/vwap
          </code>{' '}
          and{' '}
          <code className="bg-surface-subtle rounded-sm px-1 py-0.5 text-xs">
            /v1/twap
          </code>{' '}
          default{' '}
          <code className="bg-surface-subtle rounded-sm px-1 py-0.5 text-xs">
            outlier_sigma
          </code>{' '}
          to 0. Only{' '}
          <code className="bg-surface-subtle rounded-sm px-1 py-0.5 text-xs">
            /v1/ohlc
          </code>{' '}
          filters by default, because its High and Low have no statistical
          robustness at all — one dust trade pins them. Pass{' '}
          <code className="bg-surface-subtle rounded-sm px-1 py-0.5 text-xs">
            outlier_sigma
          </code>{' '}
          explicitly on the other two if you need filtering.
        </p>
        <p>
          Do not read volume-weighting as an outlier defence. On a sparse window
          a single print carries the whole result, and because VWAP is Σquote /
          Σbase an actor supplying the quote asset dominates the numerator for
          free. Time-weighting is weaker still — TWAP has no volume term.
        </p>
        <p>
          Separately from filtering, anomaly detection can refuse to publish a
          bucket outright (<ADRRef id="0019" />
          ); see{' '}
          <a href="#freeze" className="text-brand-600 hover:underline">
            freeze policy
          </a>{' '}
          below.
        </p>
        <Aside>
          The authoritative per-endpoint defaults are served, not written here:
          see{' '}
          <code className="bg-surface-subtle rounded-sm px-1 py-0.5 text-xs">
            aggregation.outlier_filter
          </code>{' '}
          on{' '}
          <code className="bg-surface-subtle rounded-sm px-1 py-0.5 text-xs">
            /v1/methodology
          </code>
          .
        </Aside>
      </Section>

      <Section
        id="stablecoin-proxy"
        title="Stablecoin → fiat proxy"
        subtitle="Why XLM/USDC contributes to the XLM/USD rate"
      >
        <p>
          On-chain trade venues quote against stablecoins (USDC, USDT, EURC) far
          more often than against raw fiat. To surface a useful XLM/USD rate, we
          proxy stablecoins to their pegged fiat at the aggregator layer, not at
          ingest. Specifically:
        </p>
        <ul className="ml-5 list-disc space-y-1">
          <li>
            Ingest stores the <strong>real pair</strong> as observed (XLM/USDC,
            XLM/USDT, XLM/PYUSD, etc.).
          </li>
          <li>
            The aggregator maps operator-declared pegged stablecoins to their
            fiat at VWAP compute time (<ADRRef id="0026" />
            ). Which stablecoins those are is deployment configuration, not a
            fixed list — this deployment&apos;s live mapping is served as{' '}
            <code className="bg-surface-subtle rounded-sm px-1 py-0.5 text-xs">
              aggregation.stablecoin_fiat_proxy
            </code>{' '}
            on{' '}
            <code className="bg-surface-subtle rounded-sm px-1 py-0.5 text-xs">
              /v1/methodology
            </code>
            . A peg is declared by its full{' '}
            <code className="bg-surface-subtle rounded-sm px-1 py-0.5 text-xs">
              CODE-ISSUER
            </code>{' '}
            identity, never by code alone.
          </li>
          <li>
            <strong>
              Eager normalisation at ingest would hide a depeg event.
            </strong>{' '}
            Late binding keeps the data honest — when a stablecoin loses its
            peg, the divergence from real fiat shows up in the historical
            record.
          </li>
        </ul>
      </Section>

      <Section
        id="freeze"
        title="Freeze policy"
        subtitle="When the API stops serving a price"
      >
        <p>
          Some failures shouldn&apos;t be smoothed over with a fresh number the
          aggregate can no longer stand behind. For those, the API keeps serving
          the <strong>last known-good value</strong> but stamps it with{' '}
          <code className="bg-surface-subtle rounded-sm px-1 py-0.5 text-xs">
            flags.frozen=true
          </code>{' '}
          so consumers know not to act on it — rather than silently returning a
          misleading live rate. Freeze triggers (
          <ADRRef id="0019" />
          ):
        </p>
        <DefList
          rows={[
            {
              term: 'Outlier storm',
              def: 'More than 50% of trades in the window were filtered as statistical outliers. Indicates upstream-data noise that the aggregate cannot trust.',
            },
            {
              term: 'Source-class collapse',
              def: 'All exchange-class sources for a pair drop out simultaneously. Common cause: vendor outage taking out CEX feeds, leaving only DEX trades whose volume is too thin for a confident VWAP.',
            },
            {
              term: 'Cross-oracle divergence',
              def: 'Our VWAP and ≥2 independent oracles disagree by more than the configured tolerance for the asset class. Catches cases where our ingest has gone wrong without catching the failure ourselves.',
            },
            {
              term: 'Operator-triggered',
              def: 'On-call can freeze a pair manually during incident response — surfaced on the status page.',
            },
          ]}
        />
        <Aside>
          Active freezes are reported in real time on{' '}
          <a href="/status" className="text-brand-600 hover:underline">
            the status page
          </a>
          , and historically as Atom syndication via{' '}
          <code className="bg-surface-subtle rounded-sm px-1 py-0.5 text-xs">
            /v1/incidents.atom
          </code>
          .
        </Aside>
      </Section>

      <Section
        id="closed-bucket"
        title="Closed-bucket-only contract"
        subtitle="Why any region serves the same number at the same wall-clock time"
      >
        <p>
          The aggregator computes prices in fixed time buckets (1m, 5m, 15m, 1h,
          1d). The API only ever serves
          <strong> closed</strong> buckets — the in-progress bucket is invisible
          until it rolls over.
        </p>
        <p>
          This is the load-bearing invariant behind cross-region consistency.
          One region serves the API today; the three-region active/active
          topology is ratified in ADR-0050 and scheduled after v1.0. Each region
          will ingest independently, with slightly different latency profiles,
          so the guarantee has to hold without any coordination between them:
          because a region only ever serves closed buckets, the value any of
          them returns for a given timestamp is identical to the byte. No
          eventually-consistent reconciliation, no last-writer-wins, no
          stale-cache footgun. The one cost is a bucket-width of latency at the
          very tip — the 1-minute bucket isn&apos;t visible until ~5–10 seconds
          after the minute ends.
        </p>
        <p>
          Tip-price callers who can&apos;t tolerate that latency use the
          separate
          <code className="bg-surface-subtle rounded-sm px-1 py-0.5 text-xs">
            /v1/price/tip
          </code>{' '}
          endpoint, which serves the rolling-window in-progress aggregate
          explicitly flagged as such (
          <ADRRef id="0018" />
          ).
        </p>
      </Section>

      <Section
        id="latency"
        title="Latency targets"
        subtitle="What we measure ourselves against"
      >
        <p>
          The serving SLOs (
          <ADRRef id="0009" />
          ):
        </p>
        {/* table-wave:allowlist prose — SLO summary table (operator
            decision D5 2026-08-24): prose-page table, stays hand-rolled;
            the A2-01 data-table wave must not sweep it. */}
        <table className="divide-line w-full divide-y text-sm">
          <thead>
            <tr className="text-ink-muted text-left text-xs tracking-wider uppercase">
              <th className="py-2">Percentile</th>
              <th className="py-2">Target</th>
              <th className="py-2">Measured live</th>
            </tr>
          </thead>
          <tbody className="divide-line-subtle divide-y">
            <tr>
              <td className="py-2 font-mono">p50</td>
              <td className="py-2">&lt; 50 ms</td>
              <td className="text-ink-muted py-2">
                <a href="/status" className="text-brand-600 hover:underline">
                  the status page
                </a>
              </td>
            </tr>
            <tr>
              <td className="py-2 font-mono">p95</td>
              <td className="py-2">&lt; 200 ms</td>
              <td className="text-ink-muted py-2">live</td>
            </tr>
            <tr>
              <td className="py-2 font-mono">p99</td>
              <td className="py-2">&lt; 500 ms</td>
              <td className="text-ink-muted py-2">live</td>
            </tr>
          </tbody>
        </table>
        <p>
          End-to-end freshness target: a trade landing on Stellar mainnet at
          ledger close T is queryable via the API by T+30 seconds in the typical
          case (longer at bucket-roll boundaries). Each component&apos;s slice
          of the budget — Galexie → indexer → aggregator → API → CDN — is
          enumerated in the ADR.
        </p>
      </Section>

      <Section
        id="precision"
        title="Numerical precision"
        subtitle="Why every amount is a string"
      >
        <p>
          Soroban stores token quantities as <strong>i128 / u128</strong> — two
          64-bit words. At the standard 7-decimal precision, any amount above
          ~922 billion tokens overflows int64. So we never truncate (
          <ADRRef id="0003" />
          ):
        </p>
        <ul className="ml-5 list-disc space-y-1">
          <li>
            <code className="bg-surface-subtle rounded-sm px-1 py-0.5 text-xs">
              *big.Int
            </code>{' '}
            in Go.
          </li>
          <li>
            <code className="bg-surface-subtle rounded-sm px-1 py-0.5 text-xs">
              NUMERIC
            </code>{' '}
            column in TimescaleDB.
          </li>
          <li>
            <strong>Strings on the wire.</strong> JSON numbers are IEEE-754
            doubles; precision loss kicks in above 2<sup>53</sup>, well below
            the i128 range. Treating amounts as numbers silently corrupts every
            value above ~9 quadrillion tokens.
          </li>
        </ul>
      </Section>

      <Section id="audit" title="Why every decision is documented">
        <p>
          Stellar already has Horizon. The reason a second pricing stack adds
          value is methodology — what gets included, how we handle edge cases,
          what triggers a freeze. None of that is useful behind a closed door.
        </p>
        <p>
          Every load-bearing decision has an Architecture Decision Record at{' '}
          <Link href="/research" className="text-brand-600 hover:underline">
            /research
          </Link>
          . Every alert has a runbook. Every Soroban contract is audited per
          WASM-version before backfill is permitted. Every incident gets a
          public postmortem on{' '}
          <a href="/status" className="text-brand-600 hover:underline">
            the status page
          </a>
          .
        </p>
      </Section>
    </div>
  );
}

const TOC = [
  { id: 'sources', label: 'Source classes' },
  { id: 'vwap', label: 'VWAP weighting' },
  { id: 'stablecoin-proxy', label: 'Stablecoin → fiat proxy' },
  { id: 'freeze', label: 'Freeze policy' },
  { id: 'closed-bucket', label: 'Closed-bucket contract' },
  { id: 'latency', label: 'Latency targets' },
  { id: 'precision', label: 'Numerical precision' },
  { id: 'audit', label: 'Why every decision is documented' },
];

function TableOfContents() {
  return (
    <nav className="border-line bg-surface rounded-xl border p-4">
      <h2 className="text-ink-muted mb-2 text-xs font-semibold tracking-wider uppercase">
        Contents
      </h2>
      <ol className="space-y-1 text-sm">
        {TOC.map((t, i) => (
          <li key={t.id}>
            <a href={`#${t.id}`} className="text-ink-body hover:text-brand-600">
              {i + 1}. {t.label}
            </a>
          </li>
        ))}
      </ol>
    </nav>
  );
}

function Section({
  id,
  title,
  subtitle,
  children,
}: {
  id: string;
  title: string;
  subtitle?: string;
  children: React.ReactNode;
}) {
  return (
    <section id={id} className="scroll-mt-24 space-y-4">
      <header className="space-y-1">
        <h2 className="text-2xl font-semibold tracking-tight">
          <a
            href={`#${id}`}
            className="hover:text-brand-600"
            aria-label={`Anchor to ${title}`}
          >
            {title}
          </a>
        </h2>
        {subtitle && <p className="text-ink-muted text-sm">{subtitle}</p>}
      </header>
      <div className="text-ink-body space-y-3 text-sm leading-6">
        {children}
      </div>
    </section>
  );
}

function DefList({ rows }: { rows: { term: string; def: string }[] }) {
  return (
    <dl className="space-y-3">
      {rows.map((r) => (
        <div
          key={r.term}
          className="grid grid-cols-1 gap-1 sm:grid-cols-[10rem_1fr] sm:gap-3"
        >
          <dt className="text-brand-600 font-mono text-xs font-semibold">
            {r.term}
          </dt>
          <dd>{r.def}</dd>
        </div>
      ))}
    </dl>
  );
}

function Formula({ children }: { children: React.ReactNode }) {
  return (
    <div className="border-line bg-surface-muted rounded-md border px-4 py-3 text-center font-mono text-sm">
      {children}
    </div>
  );
}

function Aside({ children }: { children: React.ReactNode }) {
  return (
    <p className="border-brand-500 bg-brand-50 text-ink-body rounded-md border-l-2 px-3 py-2 text-xs">
      {children}
    </p>
  );
}

function ADRRef({ id }: { id: string }) {
  return (
    <Link
      href={`/research/adr/${id}`}
      className="text-brand-600 font-medium hover:underline"
    >
      ADR-{id}
    </Link>
  );
}
