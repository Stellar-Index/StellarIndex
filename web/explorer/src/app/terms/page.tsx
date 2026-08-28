import type { Metadata } from 'next';
import Link from 'next/link';

// DRAFT — NEEDS ASH LEGAL REVIEW (2026-08-28).
//
// Why this page exists: the explorer + API had accounts, API keys, a
// magic-link sign-in and a public data feed, but no Terms of Service
// anywhere — the launch plan's D10 privacy sign-off (v1-launch-plan.md
// §D10) assumed a legal surface that was never written. This is a
// first draft assembled from what the code and ADRs actually promise
// (ADR-0049: anonymous reads, free self-service accounts, staff-set
// partner limits, NO payment surface), so it deliberately contains no
// billing, refund, or subscription terms — inventing those here would
// contradict ADR-0049.
//
// Every statement a lawyer must confirm is tagged `ASH-REVIEW` in a JSX
// comment immediately above it. (JSX cannot carry an HTML `<!-- -->`
// comment; `{/* ASH-REVIEW */}` is the equivalent and is stripped from
// the rendered HTML.) The governing-law placeholder is rendered
// VISIBLY so the draft cannot be mistaken for a final document.
//
// Structure mirrors /sla (Section / DefList / Aside) so it inherits the
// same prose-page styling; do not add a data table here.

export const metadata: Metadata = {
  title: 'Terms of Service — Stellar Index',
  description:
    'Terms governing use of the Stellar Index explorer and public API: service description, API keys and rate limits, acceptable use, data warranty disclaimer, accounts and termination.',
  alternates: { canonical: '/terms' },
};

// LAST_UPDATED is the draft date, not an effective date. Set the real
// effective date when the operator signs the text off.
const LAST_UPDATED = '2026-08-28 (DRAFT)';

export default function TermsPage() {
  return (
    <div className="mx-auto max-w-4xl space-y-10 px-6 py-10">
      <header className="space-y-3">
        <p className="text-brand-600 font-mono text-xs tracking-widest uppercase">
          Legal
        </p>
        <h1 className="text-3xl font-semibold tracking-tight">
          Terms of Service
        </h1>
        <p className="text-ink-body text-base">
          These terms govern your use of the Stellar Index explorer at
          stellarindex.io and the Stellar Index API at api.stellarindex.io
          (together, the &ldquo;Service&rdquo;). By using the Service —
          anonymously, or through an account or API key — you agree to them. If
          you do not agree, do not use the Service.
        </p>
        <p className="text-ink-muted text-xs">Last updated: {LAST_UPDATED}</p>
      </header>

      <DraftBanner />

      <TableOfContents />

      <Section
        id="service"
        title="1. The Service"
        subtitle="What Stellar Index is, and is not"
      >
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
        {/* ASH-REVIEW: "not financial advice" disclaimer wording. */}
        <p>
          The Service provides <strong>information only</strong>. Nothing on the
          Service is investment, financial, legal, or tax advice, an offer or
          solicitation to buy or sell any asset, or a recommendation of any
          issuer, venue, or asset. Market data can be delayed, incomplete, or
          wrong; you are solely responsible for any decision you make on the
          basis of it.
        </p>
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
        <p>
          The Service is free to use. There is no paid plan and no payment
          surface. Access is offered at three levels:
        </p>
        <DefList
          rows={[
            {
              term: 'Anonymous',
              // ASH-REVIEW: production enforces anon_rate_limit_per_min = 6000
              // (configs/ansible/roles/archival-node/templates/stellarindex.toml.j2);
              // the configs/example.toml default is 60 and /pricing still says
              // "60 req/min per IP". Stating the figure that prod enforces.
              def: 'Every public endpoint may be read without an account or key, rate-limited per IP address (currently 6,000 requests per minute).',
            },
            {
              term: 'Free account',
              // ASH-REVIEW: self-service keys carry an explicit per-key limit of
              // 1,000/min (signupDefaultRateLimitPerMin, internal/api/v1/signup.go),
              // which the rate limiter applies as an override in preference to
              // the bucket default key_rate_limit_per_min = 6000 (prod ansible
              // template) — the 6,000 default only applies to keys minted with
              // no explicit limit (ratelimit.go bucketKeyAndOverrideForRequest).
              // 1,000 is therefore the figure a self-service account gets.
              def: 'Creating an account (magic-link sign-in, or POST /v1/register) issues an API key with its own per-key rate limit (currently 1,000 requests per minute for self-service keys) and a monthly request quota, plus usage analytics.',
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
        <p>
          You must provide a working email address to create an account, and you
          are responsible for everything done with your account and your API
          keys. Keep keys secret: do not commit them to public repositories or
          ship them in client-side code where a scoped widget token is available
          instead. If a key is exposed, revoke it from your{' '}
          <Link href="/dashboard" className="text-brand-600 hover:underline">
            account
          </Link>{' '}
          and mint a new one. We may revoke a key we reasonably believe is
          compromised.
        </p>
        {/* ASH-REVIEW: minimum age / capacity to contract clause. */}
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
        <Aside>
          Rate-limited (HTTP 429) responses carry a Retry-After header.
          Honouring it is the whole of what &ldquo;backing off&rdquo; means
          here. Sustained abuse is handled by key revocation and IP blocking,
          per ADR-0049 — there are no chargebacks to fall back on, so limits are
          enforced technically.
        </Aside>
      </Section>

      <Section
        id="data-licence"
        title="4. Your use of the data"
        subtitle="Attribution, redistribution, and upstream venues"
      >
        {/* ASH-REVIEW: the data-licence grant and attribution requirement
            are a product decision (nothing in the repo settles them). The
            draft grants a broad non-exclusive licence with attribution,
            which matches the Apache-2.0 / open-data posture of the
            project. Confirm or narrow. */}
        <p>
          Subject to these terms, we grant you a non-exclusive,
          non-transferable, revocable licence to use data returned by the
          Service in your own applications, analyses, and publications,
          including displaying it to your users. When you display Stellar Index
          data publicly, attribute it to Stellar Index with a link to
          stellarindex.io where practical.
        </p>
        <p>
          Some data is derived from third-party venues&rsquo; public market-data
          feeds. We do not redistribute tier-restricted or paid feeds, and we do
          not grant you any right in the underlying venue data beyond what those
          venues make public. Your use of any third-party data remains subject
          to that venue&rsquo;s own terms.
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
        {/* ASH-REVIEW: warranty disclaimer — jurisdiction-specific
            consumer-protection carve-outs may be required. */}
        <p className="uppercase">
          The Service and all data are provided &ldquo;as is&rdquo; and
          &ldquo;as available&rdquo;, without warranty of any kind, express or
          implied, including any warranty of accuracy, completeness, timeliness,
          merchantability, fitness for a particular purpose, or
          non-infringement. We do not warrant that the Service will be
          uninterrupted, error-free, or free of harmful components, or that any
          price, volume, supply, or other figure is correct.
        </p>
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
        {/* ASH-REVIEW: liability cap. Because the Service is free, the
            draft caps liability at £0 / USD 0 with a nominal floor of 100
            in the governing currency where a zero cap is unenforceable.
            Confirm the number and the currency once jurisdiction is set. */}
        <p>
          To the fullest extent permitted by law, Stellar Index and its
          operators, contributors, and suppliers will not be liable for any
          indirect, incidental, special, consequential, or punitive damages, or
          for any loss of profits, revenue, data, trading gains, or goodwill,
          arising out of or relating to the Service or these terms — including
          loss caused by inaccurate, delayed, or unavailable data — however
          caused and under any theory of liability, even if advised of the
          possibility.
        </p>
        <p>
          Because the Service is provided free of charge, our total aggregate
          liability to you for all claims relating to the Service is limited to{' '}
          <Placeholder>
            100 in the governing currency — ASH TO CONFIRM
          </Placeholder>
          , or the minimum amount that applicable law permits us to limit it to,
          whichever is greater.
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
        <p>
          You may stop using the Service at any time and may revoke your API
          keys from your account. To close your account entirely, email{' '}
          <a
            href="mailto:security@stellarindex.io"
            className="text-brand-600 hover:underline"
          >
            security@stellarindex.io
          </a>{' '}
          from the address on the account.
          {/* ASH-REVIEW: there is no self-service account-close or
              GDPR-erasure flow (PRV-1 dropped it, 2026-08-15); closure is
              a manual operator action. Confirm the mailbox and turnaround. */}
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
        <p>
          The Service is pre-v1 and changes frequently. Breaking changes to the
          API are announced in the{' '}
          <Link href="/changelog" className="text-brand-600 hover:underline">
            changelog
          </Link>{' '}
          and its Atom feed; we aim to version endpoints rather than break them
          in place, but we do not guarantee backwards compatibility before v1.
        </p>
        {/* ASH-REVIEW: notice period for terms changes (draft: 14 days,
            with changelog + email to account holders as the channel). */}
        <p>
          We may revise these terms. Material changes will be posted on this
          page with a new &ldquo;last updated&rdquo; date, noted in the
          changelog, and — for account holders — emailed to the address on the
          account at least 14 days before they take effect, unless the change is
          required by law or addresses a security issue, in which case it may
          take effect immediately. Continued use of the Service after a change
          takes effect is acceptance of it.
        </p>
      </Section>

      <Section
        id="general"
        title="9. Governing law and general terms"
        subtitle="Jurisdiction is not yet set"
      >
        {/* ASH-REVIEW: governing law + venue. The operating entity and
            its jurisdiction are not recorded anywhere in the repo, so the
            placeholder is rendered visibly rather than guessed. */}
        <p>
          These terms are governed by the laws of{' '}
          <Placeholder>JURISDICTION — ASH TO CONFIRM</Placeholder>, and the
          courts of <Placeholder>JURISDICTION — ASH TO CONFIRM</Placeholder>{' '}
          have exclusive jurisdiction over any dispute arising from them,
          without prejudice to any mandatory consumer-protection rights you have
          where you live.
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
        <p>
          Questions about these terms:{' '}
          <a
            href="mailto:security@stellarindex.io"
            className="text-brand-600 hover:underline"
          >
            security@stellarindex.io
          </a>
          {/* TODO(ASH-REVIEW): verify mailbox exists — security@ is the
              only inbox the repo documents (SECURITY.md / contact page);
              a dedicated legal@ mailbox does not exist. */}
          . Security disclosures go to the same address under the policy on the{' '}
          <Link href="/contact" className="text-brand-600 hover:underline">
            contact
          </Link>{' '}
          page.
        </p>
      </Section>
    </div>
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

// DraftBanner is intentionally loud. It must be removed in the same
// commit that sets the real effective date — its presence is what
// stops an unreviewed draft from reading as binding.
function DraftBanner() {
  return (
    <div
      role="note"
      className="border-warn-300 bg-warn-50 text-warn-700 rounded-xl border p-4 text-sm"
    >
      <strong>Draft.</strong> This text has not yet been reviewed by the
      operator&rsquo;s legal counsel and is published for transparency while
      that review is in progress. Bracketed placeholders mark items still to be
      confirmed.
    </div>
  );
}

function Placeholder({ children }: { children: React.ReactNode }) {
  return (
    <span className="bg-warn-50 text-warn-700 rounded-sm px-1 font-mono text-xs">
      [{children}]
    </span>
  );
}

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
