import type { Metadata } from 'next';
import Link from 'next/link';
import { Container, PageHeader } from '@/components/ui/Page';

// States only what the code and ADRs promise (ADR-0049: anonymous reads,
// free self-service accounts, staff-set partner limits, NO payment surface),
// so it deliberately contains no billing, refund, or subscription terms.
// Each factual paragraph names the file it mirrors in a `source:` comment;
// correct the text when that file changes.

// The registered entity; both legal pages read these.
export const LEGAL_ENTITY = 'Loop Finance Ltd';
export const LEGAL_ENTITY_DETAILS =
  'Loop Finance Ltd (company no. 16862033), a company registered in England and Wales, trading as Stellar Index';
export const REGISTERED_OFFICE =
  'Unit 9 Vinnetrow Business Centre, Vinnetrow Road, Runcton, Chichester, England, PO20 1QH';

export const metadata: Metadata = {
  title: 'Terms of Service — Stellar Index',
  description:
    'Terms governing use of the Stellar Index explorer and public API: service description, API keys and rate limits, acceptable use, data warranty disclaimer, accounts and termination.',
  alternates: { canonical: '/terms' },
};

const LAST_UPDATED = '2026-09-30';

const POLICY_HISTORY = [{ date: '2026-09-30', note: 'first published' }];

export default function TermsPage() {
  return (
    <Container className="space-y-10 py-10 [&>*]:max-w-4xl">
      <header className="space-y-3">
        <PageHeader
          breadcrumbs={[
            { label: 'Home', href: '/' },
            { label: 'Terms of Service' },
          ]}
          title="Terms of Service"
          description={
            <>
              These terms govern your use of the Stellar Index explorer at
              stellarindex.io and the Stellar Index API at api.stellarindex.io
              (together, the &ldquo;Service&rdquo;), operated by{' '}
              {LEGAL_ENTITY_DETAILS} (the &ldquo;Operator&rdquo;,
              &ldquo;we&rdquo;, &ldquo;us&rdquo;). By using the Service —
              anonymously, or through an account or API key — you agree to them.
              If you do not agree, do not use the Service.
            </>
          }
        />
        <p className="text-ink-muted text-xs">Last updated: {LAST_UPDATED}</p>
      </header>

      <TableOfContents />

      <Section
        id="service"
        title="1. The Service"
        subtitle="What Stellar Index is, and is not"
      >
        <p>
          The Service is operated by {LEGAL_ENTITY_DETAILS}. Registered office:{' '}
          {REGISTERED_OFFICE}.
        </p>
        {/* source: docs/architecture/ingest-pipeline.md, web/explorer/src/app/methodology */}
        <p>
          Stellar Index is a market-data explorer and API for the Stellar
          network. It ingests public ledger data and public market-data feeds
          from third-party venues, derives prices, volumes, supply and related
          metrics, and serves them through a web interface, embeddable widgets,
          and a REST API. How each figure is computed is published on the{' '}
          <Link href="/methodology" className="text-brand-600 hover:underline">
            methodology
          </Link>{' '}
          page; the per-source health verdict is live on{' '}
          <Link href="/diagnostics" className="text-brand-600 hover:underline">
            diagnostics
          </Link>
          .
        </p>
        <p>
          The Service provides <strong>information only</strong>. Nothing on the
          Service is investment, financial, legal, or tax advice, an offer or
          solicitation to buy or sell any asset, or a recommendation of any
          issuer, venue, or asset. Market data can be delayed, incomplete, or
          wrong; you are solely responsible for any decision you make on the
          basis of it.
        </p>
        {/* source: LICENSE */}
        <p>
          The source code of the Service is published under the Apache-2.0
          licence. These terms govern the <em>hosted</em> Service only; the
          licence governs the code.
        </p>
      </Section>

      <Section
        id="access-tiers"
        title="2. Access tiers, accounts and API keys"
        subtitle="Anonymous, free, and staff-set partner limits"
      >
        {/* source: docs/adr/0049-anonymous-access-and-passkey-auth.md, configs/ansible/roles/archival-node/templates/stellarindex.toml.j2, internal/api/v1/middleware/ratelimit.go, internal/api/v1/middleware/usage.go, internal/platform/account.go */}
        <p>
          The Service is free to use. There is no paid plan and no payment
          surface. Access is offered at three levels:
        </p>
        <DefList
          rows={[
            {
              term: 'Anonymous',
              // Production enforces anon_rate_limit_per_min = 6000 (ansible
              // stellarindex.toml.j2); the code default is 60. IPv6 is keyed
              // on the /64 prefix (ratelimit.go remoteIPPrefixFor).
              def: 'Every public endpoint may be read without an account or key, rate-limited per IP address (currently 6,000 requests per minute; IPv6 callers share one limit per /64 block).',
            },
            {
              term: 'Free account',
              // Both budgets are keyed on the ACCOUNT (authenticatedRateLimitKey
              // and UsageKeyForSubject derive "acct:<slug>"), so every key an
              // account holds draws on one 1,000/min bucket and one monthly
              // quota (Tier.MaxMonthlyQuota: 1,000,000 for free).
              def: 'An account (created by magic-link sign-in, or by POST /v1/register, which also issues a first key) lets you mint API keys and see usage analytics. All keys on an account share one rate limit (currently 1,000 requests per minute) and one monthly request quota (currently 1,000,000 requests); minting more keys does not multiply either.',
            },
            {
              term: 'Partner',
              def: 'Higher, staff-set limits for wallets, exchanges and redistributors with heavy fan-out, granted on request and at our discretion. There is nothing to buy; partner limits carry no separate contract unless we agree one with you in writing.',
            },
          ]}
        />
        <p>
          The current limits are published on the{' '}
          <Link href="/pricing" className="text-brand-600 hover:underline">
            pricing
          </Link>{' '}
          page and returned in rate-limit response headers; the numbers above
          are indicative and may change under section 8.
        </p>
        {/* source: internal/api/v1/dashboardauth/handlers.go, internal/api/v1/signup.go */}
        <p>
          Signing in to the dashboard requires a working email address; an
          account created with POST /v1/register needs none, but without one we
          cannot contact you or verify a request from you. You are responsible
          for everything done with your account and your API keys. Keep keys
          secret: do not commit them to public repositories or ship them in
          client-side code. If a key is exposed, revoke it from your{' '}
          <Link href="/dashboard" className="text-brand-600 hover:underline">
            account
          </Link>{' '}
          and mint a new one. We may revoke a key we reasonably believe is
          compromised.
        </p>
        <p>
          You must be at least 18 years old, or the age of majority where you
          live, and able to enter a binding contract, to create an account.
        </p>
      </Section>

      <Section
        id="acceptable-use"
        title="3. Acceptable use"
        subtitle="Rate limits are the contract, not a suggestion"
      >
        <p>You agree not to:</p>
        <ul className="list-disc space-y-1 pl-5">
          <li>
            circumvent, probe, or overload rate limits or quotas — including by
            rotating IP addresses, minting multiple accounts or keys to multiply
            a limit, or retrying rejected (HTTP 429) requests without backing
            off;
          </li>
          <li>
            scrape or bulk-download the explorer web pages or widgets as a
            substitute for the API, or crawl the Service in a way that degrades
            it for others; use the API and the published endpoints for
            programmatic access;
          </li>
          <li>
            attempt to gain unauthorised access to any account, key, session, or
            system component, or interfere with the Service or its
            infrastructure;
          </li>
          <li>
            misrepresent Stellar Index data as your own primary source, or
            present the Service&rsquo;s figures without the staleness and
            confidence signals the API attaches to them, where doing so would
            mislead your users;
          </li>
          <li>
            use the Service to violate any law, or in connection with
            sanctions-evasion, fraud, or market manipulation;
          </li>
          <li>
            resell, sublicense, or redistribute the Service itself (as opposed
            to data you have lawfully obtained from it under section 4) without
            a written partner agreement.
          </li>
        </ul>
        {/* source: internal/api/v1/middleware/ratelimit.go, docs/adr/0049-anonymous-access-and-passkey-auth.md */}
        <Aside>
          Rate-limited (HTTP 429) responses carry a Retry-After header.
          Honouring it is the whole of what &ldquo;backing off&rdquo; means
          here. Sustained abuse is handled by rate limits and key revocation,
          per ADR-0049 — there are no chargebacks to fall back on, so limits are
          enforced technically.
        </Aside>
      </Section>

      <Section
        id="data-licence"
        title="4. Your use of the data"
        subtitle="Attribution, redistribution, and upstream venues"
      >
        <p>
          Subject to these terms, we grant you a non-exclusive,
          non-transferable, revocable licence to use data returned by the
          Service in your own applications, analyses, and publications,
          including displaying it to your users. When you display Stellar Index
          data publicly, attribute it to Stellar Index with a link to
          stellarindex.io where practical.
        </p>
        {/* source: docs/contributing/procedures/add-cex-connector.md, internal/api/v1/observations.go */}
        <p>
          Some data is derived from third-party venues&rsquo; market-data feeds.
          We use venues&rsquo; public market feeds. A venue&rsquo;s own terms
          may limit your reuse of raw per-venue observations; check them before
          redistributing <code>/v1/observations</code> data. We do not grant you
          any right in the underlying venue data beyond what those venues make
          public, and your use of any third-party data remains subject to that
          venue&rsquo;s own terms.
        </p>
        <p>
          Ledger data is public information on the Stellar network. We claim no
          ownership of it; we claim rights only in the derived metrics, the API,
          the explorer, and the compilation.
        </p>
      </Section>

      <Section
        id="no-warranty"
        title="5. No warranty"
        subtitle="Market data is served as-is"
      >
        <p className="uppercase">
          The Service and all data are provided &ldquo;as is&rdquo; and
          &ldquo;as available&rdquo;, without warranty of any kind, express or
          implied, including any warranty of accuracy, completeness, timeliness,
          merchantability, fitness for a particular purpose, or
          non-infringement. We do not warrant that the Service will be
          uninterrupted, error-free, or free of harmful components, or that any
          price, volume, supply, or other figure is correct.
        </p>
        {/* source: web/explorer/src/app/sla/page.tsx */}
        <p>
          The published{' '}
          <Link href="/sla" className="text-brand-600 hover:underline">
            service-level targets
          </Link>{' '}
          are engineering objectives we measure ourselves against. Unless we
          have agreed a written partner agreement with you that says otherwise,
          they are not a contractual guarantee and no credit, refund, or remedy
          attaches to missing them.
        </p>
      </Section>

      <Section
        id="liability"
        title="6. Limitation of liability"
        subtitle="What we are not responsible for"
      >
        <p>
          To the fullest extent permitted by law, the Operator and its
          operators, contributors, and suppliers will not be liable for any
          indirect, incidental, special, consequential, or punitive damages, or
          for any loss of profits, revenue, data, trading gains, or goodwill,
          arising out of or relating to the Service or these terms — including
          loss caused by inaccurate, delayed, or unavailable data — however
          caused and under any theory of liability, even if advised of the
          possibility.
        </p>
        <p>
          Our total aggregate liability to you for all claims relating to the
          Service is limited to the greater of GBP 100 (one hundred pounds
          sterling) and the total fees you paid us for the Service in the 12
          months before the event giving rise to the claim.
        </p>
        <p>
          Nothing in these terms excludes or limits liability that cannot be
          excluded or limited by law, including for death or personal injury
          caused by negligence, or for fraud.
        </p>
      </Section>

      <Section
        id="termination"
        title="7. Suspension and termination"
        subtitle="Yours and ours"
      >
        {/* source: internal/api/v1/dashboardauth/account.go, internal/platform/postgresstore/account_erasure.go */}
        <p>
          You may stop using the Service at any time and may revoke your API
          keys from your account. An account owner can close the account
          entirely with <code>DELETE /v1/dashboard/account</code> while signed
          in to the dashboard — you type the account slug back to confirm (the
          explorer has no button for it yet) — or by emailing{' '}
          <a
            href="mailto:security@stellarindex.io"
            className="text-brand-600 hover:underline"
          >
            security@stellarindex.io
          </a>{' '}
          from the address on the account. Closing an account erases it
          immediately and cannot be undone; the{' '}
          <Link
            href="/privacy#rights"
            className="text-brand-600 hover:underline"
          >
            privacy policy
          </Link>{' '}
          lists what is kept.
        </p>
        <p>
          We may suspend or revoke keys, or suspend or close accounts,
          immediately and without notice where we reasonably believe you have
          breached section 3, where required by law, or where necessary to
          protect the Service or other users. We may also close inactive
          accounts or discontinue the Service or any endpoint on reasonable
          notice. Sections 4 through 6 and 9 survive termination.
        </p>
      </Section>

      <Section
        id="changes"
        title="8. Changes to the Service and to these terms"
        subtitle="How you will find out"
      >
        {/* source: web/explorer/src/app/changelog.atom/route.ts */}
        <p>
          The Service is pre-v1 and changes frequently. Breaking changes to the
          API are announced in the{' '}
          <Link href="/changelog" className="text-brand-600 hover:underline">
            changelog
          </Link>{' '}
          and its Atom feed; we aim to version endpoints rather than break them
          in place, but we do not guarantee backwards compatibility before v1.
        </p>
        {/* source: internal/notify/templates.go (no bulk sender exists; /v1/register accounts may hold no email) */}
        <p>
          We may revise these terms. Material changes will be posted on this
          page with a new &ldquo;last updated&rdquo; date and a row in the
          policy history below, and, where we hold a verified email address for
          the account, sent to it. The changelog is not the notice. A change
          required by law or addressing a security issue may take effect
          immediately. Continued use of the Service after a change takes effect
          is acceptance of it.
        </p>
      </Section>

      <Section
        id="general"
        title="9. Governing law and general terms"
        subtitle="England and Wales"
      >
        <p>
          These terms, and any dispute or claim arising out of or in connection
          with them or the Service, are governed by the laws of England and
          Wales, and the courts of England and Wales have exclusive jurisdiction
          over any such dispute, without prejudice to any mandatory
          consumer-protection rights you have where you live.
        </p>
        <p>
          These terms, together with the{' '}
          <Link href="/privacy" className="text-brand-600 hover:underline">
            privacy policy
          </Link>
          , are the entire agreement between you and us about the Service. If
          any provision is found unenforceable, the rest remains in effect. Our
          failure to enforce a provision is not a waiver of it. You may not
          assign these terms; we may assign them to a successor operator of the
          Service on notice.
        </p>
        {/* source: SECURITY.md, web/explorer/src/app/contact/page.tsx */}
        <p>
          Questions about these terms:{' '}
          <a
            href="mailto:security@stellarindex.io"
            className="text-brand-600 hover:underline"
          >
            security@stellarindex.io
          </a>
          . Security disclosures go to the same address under the policy on the{' '}
          <Link href="/contact" className="text-brand-600 hover:underline">
            contact
          </Link>{' '}
          page: we acknowledge receipt within 72 hours, provide an initial
          assessment within 7 days, and fix HIGH / CRITICAL issues within 30
          days.
        </p>
      </Section>

      <PolicyHistory />
    </Container>
  );
}

const TOC = [
  { id: 'service', label: 'The Service' },
  { id: 'access-tiers', label: 'Access tiers, accounts and API keys' },
  { id: 'acceptable-use', label: 'Acceptable use' },
  { id: 'data-licence', label: 'Your use of the data' },
  { id: 'no-warranty', label: 'No warranty' },
  { id: 'liability', label: 'Limitation of liability' },
  { id: 'termination', label: 'Suspension and termination' },
  { id: 'changes', label: 'Changes to the Service and to these terms' },
  { id: 'general', label: 'Governing law and general terms' },
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

function Aside({ children }: { children: React.ReactNode }) {
  return (
    <p className="border-brand-500 bg-brand-50 text-ink-body rounded-md border-l-2 px-3 py-2 text-xs">
      {children}
    </p>
  );
}

function PolicyHistory() {
  return (
    <section id="history" className="scroll-mt-24 space-y-2">
      <h2 className="text-ink-muted text-xs font-semibold tracking-wider uppercase">
        Policy history
      </h2>
      <ul className="text-ink-body space-y-1 text-sm">
        {POLICY_HISTORY.map((h) => (
          <li key={h.date}>
            <span className="font-mono text-xs">{h.date}</span> — {h.note}
          </li>
        ))}
      </ul>
    </section>
  );
}
