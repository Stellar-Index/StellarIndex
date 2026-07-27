import type { Metadata } from 'next';
import Link from 'next/link';

export const metadata: Metadata = {
  title: 'Service level — Stellar Index API targets and error budget',
  description:
    'The four SLA targets Stellar Index holds itself to — p95/p99 latency, availability, and price freshness — how each is measured, the monthly error budget they imply, and what is explicitly excluded.',
  alternates: { canonical: '/sla' },
};

export default function SLAPage() {
  return (
    <div className="mx-auto max-w-4xl space-y-10 px-6 py-10">
      <header className="space-y-3">
        <h1 className="text-3xl font-semibold tracking-tight">Service level</h1>
        <p className="text-base text-ink-body">
          Four targets bind the Stellar Index API. Each is measured
          continuously by an independent probe that runs against the
          public edge — the same path your requests take — and each
          measurement is published as a metric, not asserted in prose.
          This page states the targets, how they are measured, and
          what they deliberately do not cover.
        </p>
      </header>

      <TableOfContents />

      <Section
        id="targets"
        title="The targets"
        subtitle="Measured on the public edge, continuously"
      >
        <div className="overflow-x-auto">
          <table className="w-full text-sm">
            <thead>
              <tr className="border-b border-line text-left text-xs uppercase tracking-wider text-ink-muted">
                <th className="py-2 pr-4 font-semibold">Target</th>
                <th className="py-2 pr-4 font-semibold">Objective</th>
                <th className="py-2 font-semibold">Measured as</th>
              </tr>
            </thead>
            <tbody className="divide-y divide-line">
              <tr>
                <td className="py-2 pr-4 font-mono text-xs text-brand-600">p95 latency</td>
                <td className="py-2 pr-4">&le; 200 ms</td>
                <td className="py-2">
                  Server-side response time per endpoint, 95th percentile
                </td>
              </tr>
              <tr>
                <td className="py-2 pr-4 font-mono text-xs text-brand-600">p99 latency</td>
                <td className="py-2 pr-4">&le; 500 ms</td>
                <td className="py-2">
                  As above, 99th percentile — the tail that users actually feel
                </td>
              </tr>
              <tr>
                <td className="py-2 pr-4 font-mono text-xs text-brand-600">Availability</td>
                <td className="py-2 pr-4">&ge; 99.9 %</td>
                <td className="py-2">
                  Share of probe requests answered 2xx, per calendar month
                </td>
              </tr>
              <tr>
                <td className="py-2 pr-4 font-mono text-xs text-brand-600">Price freshness</td>
                <td className="py-2 pr-4">&le; 30 s</td>
                <td className="py-2">
                  Age of <code className="rounded-sm bg-surface-subtle px-1 py-0.5 text-xs">observed_at</code>{' '}
                  on the price endpoint at read time
                </td>
              </tr>
            </tbody>
          </table>
        </div>
        <Aside>
          Freshness is the target most services quietly omit. A price
          endpoint that answers in 5 ms with a four-hour-old number is
          fast and useless, so staleness is bound explicitly rather
          than left to the latency figure to imply.
        </Aside>
      </Section>

      <Section
        id="error-budget"
        title="Error budget"
        subtitle="What 99.9 % actually permits"
      >
        <p>
          A 99.9 % monthly availability objective is an allowance of
          roughly <strong>43 minutes 12 seconds</strong> of unavailability
          in a 30-day month. That is the budget. It is spent by
          incidents, by planned maintenance that takes the API down, and
          by failed deploys alike — the budget does not distinguish
          between causes, because your integration does not either.
        </p>
        <DefList
          rows={[
            {
              term: 'Budget spent',
              def: 'When an incident closes, its unavailable minutes are debited. A month that exhausts the budget triggers a freeze on non-essential change until the following month.',
            },
            {
              term: 'Budget intact',
              def: 'Normal change cadence. Deploys are gated on the post-deploy verification battery, not on the budget.',
            },
          ]}
        />
      </Section>

      <Section
        id="measurement"
        title="How it is measured"
        subtitle="An independent probe, not self-reporting"
      >
        <p>
          A dedicated probe binary samples the public endpoints on a
          schedule and records per-endpoint p50/p95/p99 latency,
          availability, and — for the price endpoint — freshness
          computed from the response&apos;s own{' '}
          <code className="rounded-sm bg-surface-subtle px-1 py-0.5 text-xs">observed_at</code>{' '}
          timestamp. It writes results as metrics, so a target breach
          is visible as a number rather than discovered from a support
          ticket.
        </p>
        <p>
          The probe runs against the same public edge external clients
          use. It is deliberately not wired into the application it
          measures: a service that grades its own homework can be
          healthy and wrong at the same time.
        </p>
      </Section>

      <Section
        id="exclusions"
        title="What this does not cover"
        subtitle="Stated plainly, because an unstated exclusion is a broken promise"
      >
        <DefList
          rows={[
            {
              term: 'Upstream outages',
              def: 'When the Stellar network itself halts or an upstream venue stops publishing, we serve the last verified data and mark it stale. Freshness targets are measured against our pipeline, not the network’s liveness.',
            },
            {
              term: 'Historical backfills',
              def: 'Bulk historical queries and re-derivations are batch operations with no latency objective. The targets bind the serving path.',
            },
            {
              term: 'Single-region posture',
              def: 'The service runs from one region today. That is a documented, accepted risk with a tested restore path — not a redundancy claim.',
            },
          ]}
        />
        <Aside>
          Coverage and completeness are a separate, stronger guarantee
          than availability, and they are reported independently — see{' '}
          <Link href="/diagnostics" className="text-brand-600 hover:underline">
            diagnostics
          </Link>{' '}
          for the live per-source verdict and{' '}
          <Link href="/methodology" className="text-brand-600 hover:underline">
            methodology
          </Link>{' '}
          for how prices are computed.
        </Aside>
      </Section>

      <Section
        id="status"
        title="Live status"
        subtitle="Current state, not a claim about it"
      >
        <p>
          The{' '}
          <a
            href="https://status.stellarindex.io"
            className="text-brand-600 hover:underline"
          >
            status page
          </a>{' '}
          carries current availability and any open incident. Past
          incidents are published with their timeline and root cause
          rather than summarised away.
        </p>
      </Section>
    </div>
  );
}

const TOC = [
  { id: 'targets', label: 'The targets' },
  { id: 'error-budget', label: 'Error budget' },
  { id: 'measurement', label: 'How it is measured' },
  { id: 'exclusions', label: 'What this does not cover' },
  { id: 'status', label: 'Live status' },
];

function TableOfContents() {
  return (
    <nav className="rounded-xl border border-line bg-surface p-4">
      <h2 className="mb-2 text-xs font-semibold uppercase tracking-wider text-ink-muted">
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
    <section id={id} className="space-y-4 scroll-mt-24">
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
        {subtitle && <p className="text-sm text-ink-muted">{subtitle}</p>}
      </header>
      <div className="space-y-3 text-sm leading-6 text-ink-body">{children}</div>
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
          <dt className="font-mono text-xs font-semibold text-brand-600">
            {r.term}
          </dt>
          <dd>{r.def}</dd>
        </div>
      ))}
    </dl>
  );
}

function Aside({ children }: { children: React.ReactNode }) {
  return (
    <p className="rounded-md border-l-2 border-brand-500 bg-brand-50 px-3 py-2 text-xs text-ink-body">
      {children}
    </p>
  );
}
