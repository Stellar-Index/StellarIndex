import type { Metadata } from 'next';
import Link from 'next/link';
import { ExternalLink } from 'lucide-react';

import { CopyableSnippet } from '../widgets/CopyableSnippet';

export const metadata: Metadata = {
  title: 'Go SDK',
  description:
    'Official Go SDK for the Stellar Index API. Idiomatic typed client, SemVer-stable surface, paste-ready examples for every common pattern.',
  alternates: { canonical: '/sdk' },
};

const INSTALL = `go get github.com/Stellar-Index/StellarIndex/pkg/client`;

const QUICKSTART = `package main

import (
    "context"
    "fmt"
    "github.com/Stellar-Index/StellarIndex/pkg/client"
)

func main() {
    c := client.New(client.Options{
        BaseURL: "https://api.stellarindex.io",
        APIKey:  "sip_…", // optional; anonymous works at the public rate-limit
    })

    p, err := c.Price(context.Background(), client.PriceQuery{
        Asset: "native",
        Quote: "fiat:USD",
    })
    if err != nil {
        panic(err)
    }
    fmt.Printf("XLM/USD = %s (%s, observed %s)\\n",
        p.Data.Price, p.Data.PriceType, p.Data.ObservedAt)
}`;

const PATTERNS: { title: string; blurb: string; code: string }[] = [
  {
    title: 'Batch lookup — up to 1000 assets per call',
    blurb:
      'Single round trip; the wire shape preserves the input order. Use this when feeding a watchlist or rendering a portfolio strip.',
    code: `prices, err := c.PriceBatch(ctx, client.PriceBatchQuery{
    Assets: []string{
        "native",
        "USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN",
        "AQUA-GBNZILSTVQZ4R7IKQDGHYGY2QXL5QOFJYQMXPKWRRM5PAV7Y4M67AQUA",
    },
    Quote: "fiat:USD",
})
if err != nil {
    return err
}
for _, p := range prices.Data {
    fmt.Printf("%-10s %s\\n", p.AssetID, p.Price)
}`,
  },
  {
    title: 'Trade history — recent trades for a pair',
    blurb:
      'Cursor-paginated. For a one-shot recent-trades panel, take the first page; for a backfill or aggregator, follow Pagination.Next until empty.',
    code: `h, err := c.History(ctx, client.HistoryRangeQuery{
    Base:  "native",
    Quote: "USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN",
    Limit: 50,
})
if err != nil {
    return err
}
for _, t := range h.Data {
    fmt.Printf("%s  %s/%s  %s @ %s\\n",
        t.Timestamp.Format(time.RFC3339), t.BaseAsset, t.QuoteAsset,
        t.BaseAmount, t.Price)
}`,
  },
  {
    title: 'Closed-bucket SSE stream',
    blurb:
      'Per ADR-0018 the API only emits closed buckets — every event is final. SSE sits deliberately outside the typed SDK surface (request/response only) — consume the stream with net/http directly; cancel via the parent ctx.',
    code: `req, err := http.NewRequestWithContext(ctx, http.MethodGet,
    "https://api.stellarindex.io/v1/price/stream?asset=native&quote=fiat:USD", nil)
if err != nil {
    return err
}
req.Header.Set("Accept", "text/event-stream")

resp, err := http.DefaultClient.Do(req)
if err != nil {
    return err
}
defer resp.Body.Close()

sc := bufio.NewScanner(resp.Body)
for sc.Scan() {
    if data, ok := strings.CutPrefix(sc.Text(), "data: "); ok {
        fmt.Println(data) // one closed VWAP bucket per event, JSON
    }
}`,
  },
  {
    title: 'OHLC bar — single-window summary',
    blurb:
      'For per-asset cards or sparkline backing data. Pair with /v1/chart for multi-bar series; OHLC is the one-bar variant.',
    code: `o, err := c.OHLC(ctx, client.OHLCQuery{
    Base:  "native",
    Quote: "fiat:USD",
    // From/To zero → the server defaults to the last hour,
    // clamped to a closed-bucket boundary (ADR-0015).
})
if err != nil {
    return err
}
fmt.Printf("O=%s H=%s L=%s C=%s vol=%s\\n",
    o.Data.Open, o.Data.High, o.Data.Low, o.Data.Close, o.Data.QuoteVolume)`,
  },
  {
    title: 'Error handling — *APIError wraps problem+json',
    blurb:
      "HTTP errors from the server come through as `*client.APIError` carrying the problem-document fields (type / title / status / detail). Network / parse errors come through wrapped via fmt.Errorf — distinguish with errors.As.",
    code: `_, err := c.Price(ctx, client.PriceQuery{Asset: "garbage"})
if err != nil {
    var apiErr *client.APIError
    if errors.As(err, &apiErr) {
        switch apiErr.Status {
        case 404:
            // pair not yet observed — render "no price"
        case 400:
            // bad asset id — fix call site
        default:
            log.Printf("api error: %d %s", apiErr.Status, apiErr.Detail)
        }
    } else {
        log.Printf("transport error: %v", err)
    }
}`,
  },
];

export default function SDKPage() {
  return (
    <div className="mx-auto w-full max-w-4xl px-6 py-12 sm:py-16">
      <header className="mb-10 space-y-3">
        <p className="font-mono text-xs uppercase tracking-widest text-brand-600">
          Go SDK
        </p>
        <h1 className="text-3xl font-semibold tracking-tight sm:text-4xl">
          Idiomatic Go client for the Stellar Index API
        </h1>
        <p className="max-w-2xl text-base text-ink-body">
          Typed, SemVer-stable, no surprises. Anonymous mode for the
          public tier; bearer-token mode for paid tiers and SEP-10
          JWTs. The SDK covers the pricing/read surface — prices,
          history, OHLC, markets, the asset catalogue, and account
          self-service — with ~36 typed methods; SSE streams and the
          explorer read surface are reachable over plain HTTP.
        </p>
      </header>

      <section className="mb-10 space-y-4">
        <h2 className="text-xl font-semibold tracking-tight">Install</h2>
        <p className="text-sm text-ink-body">
          Single dependency. The module path follows the canonical{' '}
          <code className="rounded-sm bg-surface-subtle px-1.5 py-0.5 font-mono text-xs">
            github.com/Stellar-Index/StellarIndex
          </code>{' '}
          repo path.
        </p>
        <div className="overflow-hidden rounded-xl border border-line">
          <CopyableSnippet snippet={INSTALL} />
        </div>
      </section>

      <section className="mb-12 space-y-4">
        <h2 className="text-xl font-semibold tracking-tight">Quick start</h2>
        <p className="text-sm text-ink-body">
          One-asset current-price lookup. Anonymous works at the public
          rate-limit; pass <code className="font-mono text-xs">APIKey</code>{' '}
          to bump to your tier&apos;s budget.
        </p>
        <div className="overflow-hidden rounded-xl border border-line">
          <CopyableSnippet snippet={QUICKSTART} />
        </div>
      </section>

      <section className="mb-12 space-y-6">
        <h2 className="text-xl font-semibold tracking-tight">
          Common patterns
        </h2>
        {PATTERNS.map((p) => (
          <div key={p.title} className="space-y-3">
            <div>
              <h3 className="text-base font-semibold">{p.title}</h3>
              <p className="mt-1 text-sm text-ink-body">
                {p.blurb}
              </p>
            </div>
            <div className="overflow-hidden rounded-xl border border-line">
              <CopyableSnippet snippet={p.code} />
            </div>
          </div>
        ))}
      </section>

      <section className="mb-12 space-y-3">
        <h2 className="text-xl font-semibold tracking-tight">
          Authentication
        </h2>
        <p className="text-sm text-ink-body">
          Three modes mirror the server&apos;s auth middleware:
        </p>
        <dl className="grid grid-cols-1 gap-3 sm:grid-cols-3">
          <Mode
            term="Anonymous"
            def="No APIKey on the client. Rate-limited per IP. Good for prototyping and embedded widgets."
          />
          <Mode
            term="API key"
            def={
              <>
                Set{' '}
                <code className="rounded-sm bg-surface-subtle px-1 py-0.5 font-mono text-[11px]">
                  Options.APIKey
                </code>
                . Sent as{' '}
                <code className="rounded-sm bg-surface-subtle px-1 py-0.5 font-mono text-[11px]">
                  Authorization: Bearer
                </code>{' '}
                on every request. Sign in at{' '}
                <Link href="/signin" className="text-brand-600 hover:underline">
                  /signin
                </Link>{' '}
                (magic-link, no password) and mint a key from{' '}
                <Link href="/dashboard" className="text-brand-600 hover:underline">
                  /account
                </Link>
                .
              </>
            }
          />
          <Mode
            term="SEP-10"
            def="Verified at /v1/auth/sep10/{challenge,token}. Pass the resulting JWT as Options.APIKey; the SDK forwards it verbatim."
          />
        </dl>
      </section>

      <section className="rounded-xl border border-line bg-surface p-5 text-sm">
        <h2 className="text-base font-semibold">Reference</h2>
        <ul className="mt-3 space-y-2 text-ink-body">
          {/* site-audit S5: the pkg.go.dev link 404'd. The module IS
              published (v0.20.x is on proxy.golang.org and `go get` works),
              but pkg.go.dev does not render this large monorepo — repeated
              requests still 404. Point the reference at the GitHub source,
              which renders the package's Go doc comments in-browser and
              always resolves. Swap back to pkg.go.dev if it ever indexes. */}
          <li>
            <a
              href="https://github.com/Stellar-Index/StellarIndex/tree/main/pkg/client"
              target="_blank"
              rel="noreferrer noopener"
              className="inline-flex items-center gap-1 text-brand-600 hover:underline"
            >
              API reference &amp; source (GitHub)
              <ExternalLink className="h-3 w-3" />
            </a>
          </li>
          <li>
            <a
              href="https://docs.stellarindex.io"
              target="_blank"
              rel="noreferrer noopener"
              className="inline-flex items-center gap-1 text-brand-600 hover:underline"
            >
              REST API reference (Scalar)
              <ExternalLink className="h-3 w-3" />
            </a>
          </li>
          <li>
            Other languages? The REST API is plain JSON — generate a
            client for your favourite language from the OpenAPI spec
            at{' '}
            <code className="rounded-sm bg-surface-subtle px-1 py-0.5 font-mono text-[11px]">
              openapi/stellar-index.v1.yaml
            </code>
            . First-party clients beyond Go land as the demand
            surfaces.
          </li>
        </ul>
      </section>
    </div>
  );
}

function Mode({
  term,
  def,
}: {
  term: string;
  def: React.ReactNode;
}) {
  return (
    <div className="rounded-xl border border-line bg-surface p-3">
      <dt className="text-xs font-semibold uppercase tracking-wider text-brand-600">
        {term}
      </dt>
      <dd className="mt-1 text-xs text-ink-body">
        {def}
      </dd>
    </div>
  );
}
