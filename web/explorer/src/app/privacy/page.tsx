import type { Metadata } from 'next';
import Link from 'next/link';

// DRAFT — NEEDS ASH LEGAL REVIEW (2026-08-28).
//
// Why this page exists: the platform stores real PII (account email,
// full-resolution IP addresses, user agents) and the launch plan's D10
// item was reduced to a "documentation sign-off" (v1-launch-plan.md
// §D10, 2026-08-15) on the basis that the privacy hygiene was done in
// code (PRV-2 magic-link reaper, PRV-3 IP-retention rationale). The
// public-facing half of that sign-off — telling users what is kept and
// for how long — did not exist. This draft states ONLY what the code
// and migrations show is actually collected, and cites the retention
// figure the code enforces for each item; it must be corrected, not
// re-worded, if the code changes:
//
//   - magic_link_tokens (email, requested_ip): 15-min TTL, expired rows
//     swept 48 h later — internal/magiclinkreaper, PRV-2.
//   - login_code_lockouts (email, ip): swept 48 h — internal/logincodereaper.
//   - sessions (ip_first/last_seen, user_agent): 30-day TTL
//     (dashboardauth.SessionTTL). NOTE: there is no session reaper, so
//     expired rows persist until deleted manually — flagged below.
//   - api_keys.last_used_ip / user_agent: for the life of the key;
//     revoked keys are soft-deleted "forever" (migration 0027).
//   - audit_log.ip / user_agent: 12-month online retention (migration 0027).
//   - api_usage_events: NOT WIRED (no writer) — nothing stored, so it is
//     deliberately not described as collected.
//   - anonymous rate-limit counters: per-IP, per-minute, in Redis only.
//   - IPs are kept at FULL resolution on purpose (PRV-3, migration 0027):
//     they back abuse/session-hijack forensics and the inbox-bomb throttle.
//
// Processors named are only those the repo shows: Resend (email,
// internal/notify/resend.go), Cloudflare (Pages hosting of this
// explorer, wrangler.toml; R2 off-site backups per ADR-0050), the
// API/database host (Hetzner, docs/operations/self-hosting.md), and
// GitHub (issues). No analytics, ad, or tracking script is loaded
// (src/app/layout.tsx carries only schema.org JSON-LD).
//
// Every statement a lawyer must confirm is tagged `ASH-REVIEW` in a JSX
// comment immediately above it (JSX cannot carry an HTML `<!-- -->`
// comment; `{/* ASH-REVIEW */}` is the equivalent). Structure mirrors
// /sla and /terms.

export const metadata: Metadata = {
  title: 'Privacy Policy — Stellar Index',
  description:
    'What Stellar Index collects when you browse the explorer or use the API — account email, IP addresses and why they are kept, cookies — the processors involved, retention periods, and your GDPR / UK GDPR rights.',
  alternates: { canonical: '/privacy' },
};

// LAST_UPDATED is the draft date, not an effective date.
const LAST_UPDATED = '2026-08-28 (DRAFT)';

export default function PrivacyPage() {
  return (
    <div className="mx-auto max-w-4xl space-y-10 px-6 py-10">
      <header className="space-y-3">
        <p className="text-brand-600 font-mono text-xs tracking-widest uppercase">
          Legal
        </p>
        <h1 className="text-3xl font-semibold tracking-tight">
          Privacy Policy
        </h1>
        <p className="text-ink-body text-base">
          This policy explains what personal data Stellar Index collects when
          you use the explorer at stellarindex.io or the API at
          api.stellarindex.io, why, for how long, who processes it, and what
          rights you have. The short version: we run no advertising or analytics
          trackers, we never sell data, and the only personal data we hold is
          what an account and abuse-prevention actually need — an email address,
          and IP addresses kept for bounded periods.
        </p>
        <p className="text-ink-muted text-xs">Last updated: {LAST_UPDATED}</p>
      </header>

      <DraftBanner />

      <TableOfContents />

      <Section
        id="controller"
        title="1. Who is responsible"
        subtitle="The data controller"
      >
        {/* ASH-REVIEW: legal entity name, registered address, and
            whether an EU/UK representative (GDPR Art. 27) is required.
            None of this is recorded in the repo. */}
        <p>
          The data controller for the Service is{' '}
          <Placeholder>OPERATING ENTITY + ADDRESS — ASH TO CONFIRM</Placeholder>
          , operating as Stellar Index. Contact for anything in this policy:{' '}
          <a
            href="mailto:security@stellarindex.io"
            className="text-brand-600 hover:underline"
          >
            security@stellarindex.io
          </a>
          {/* TODO(ASH-REVIEW): verify mailbox exists — security@ is the
              only inbox the repo documents; there is no privacy@ / dpo@. */}
          .
        </p>
      </Section>

      <Section
        id="anonymous"
        title="2. Browsing and anonymous API use"
        subtitle="No account, no tracking"
      >
        <p>
          You can use the whole explorer and read every public API endpoint
          without an account. When you do, we process:
        </p>
        <DefList
          rows={[
            {
              term: 'IP address',
              def: 'Used in memory to enforce the anonymous per-IP rate limit (a rolling one-minute counter). The counter expires with its window; it is not written to a database.',
            },
            {
              term: 'Request logs',
              def: 'Standard server logs (request path, status, timing, user agent, IP) for operating and securing the Service.',
            },
          ]}
        />
        {/* ASH-REVIEW: edge/server access-log retention. The repo does not
            pin a number for HTTP access logs (Cloudflare edge logs +
            API-host journal). State the real figure once confirmed. */}
        <p>
          Server and edge logs are retained for{' '}
          <Placeholder>ACCESS-LOG RETENTION — ASH TO CONFIRM</Placeholder> and
          are used only for operations, security, and abuse investigation.
        </p>
        <p>
          We load <strong>no</strong> third-party analytics, advertising, or
          social-media scripts. The explorer stores some display preferences
          (for example widget settings) in your browser&rsquo;s local storage;
          that data never leaves your device.
        </p>
      </Section>

      <Section
        id="accounts"
        title="3. Accounts, sign-in and API keys"
        subtitle="What an account actually stores"
      >
        <p>
          Sign-in is passwordless: you enter an email address, we send a
          single-use magic link (or a short code), and clicking it creates a
          session. Passkeys (WebAuthn) can be added as a second sign-in method.
          We hold no passwords. For an account we process:
        </p>
        <DefList
          rows={[
            {
              term: 'Email address',
              def: 'Your sign-in identity and the address we send magic links and account notices to. Required.',
            },
            {
              term: 'Display name',
              def: 'Optional, set by you.',
            },
            {
              term: 'Passkey public keys',
              def: 'If you register a passkey, its public key and credential ID. The private key never leaves your device.',
            },
            {
              term: 'API keys',
              def: 'We store a hash of each key, its name, tier and limits, and — each time it is used — the last-seen time, IP address and user agent, so you can spot a leaked key from your dashboard.',
            },
            {
              term: 'Usage counts',
              def: 'Per-key request counts by minute and by month for rate limiting, your quota, and the usage chart in your dashboard. These are counts, not request bodies.',
            },
            {
              term: 'Sessions',
              def: 'For each dashboard session: creation time, last-seen time, the first and most recent IP address, and the user agent.',
            },
            {
              term: 'Audit log',
              def: 'Security-relevant account actions (sign-in, key minted, key revoked, and similar) with the acting IP address and user agent.',
            },
          ]}
        />
        {/* ASH-REVIEW: lawful basis. Draft: Art. 6(1)(b) contract for the
            account itself; Art. 6(1)(f) legitimate interests for the
            security/abuse-prevention IP retention. */}
        <p>
          We process account data because it is necessary to provide the account
          you asked for (GDPR Art. 6(1)(b)), and we keep the security records
          above on the basis of our legitimate interest in preventing abuse and
          protecting your account (Art. 6(1)(f)).
        </p>
      </Section>

      <Section
        id="ip-addresses"
        title="4. Why we keep IP addresses at full resolution"
        subtitle="A deliberate, documented decision"
      >
        <p>
          Several of the records above hold a full IP address rather than a
          truncated or hashed one. That is deliberate. The sign-in endpoint is
          unauthenticated by design — anyone can ask for a magic link to any
          address — so the IP behind each request is the only signal that lets
          us throttle inbox-bombing and detect an attacker minting links for
          someone else&rsquo;s mailbox. Likewise, the IP on a session and on an
          API key&rsquo;s last use is what lets you and us recognise a hijacked
          session or a leaked key. A masked prefix would defeat all three
          controls.
        </p>
        <p>
          In exchange, every IP-bearing record has a bounded lifetime (section
          6), and we do not use IP addresses for anything other than security,
          abuse prevention, and operating the Service. We do not geolocate you
          for advertising, profile you, or link your IP to third-party data.
        </p>
      </Section>

      <Section
        id="processors"
        title="5. Who processes data for us"
        subtitle="Only the providers the Service actually runs on"
      >
        <p>
          We do not sell personal data, and we do not share it with anyone
          except the infrastructure providers below, each acting on our
          instructions, and where the law requires us to.
        </p>
        <DefList
          rows={[
            {
              term: 'Resend',
              def: 'Transactional email delivery. Receives your email address and the magic-link message we send to it.',
            },
            {
              term: 'Cloudflare',
              def: 'Serves this explorer web site from its edge network and provides DNS and DDoS protection, so it sees your IP address and requests in transit. Off-site encrypted backups of the database are also stored on Cloudflare R2.',
            },
            {
              term: 'Hetzner',
              def: 'Hosts the API servers and the database in which account data lives.',
            },
            {
              term: 'GitHub',
              def: 'If you open an issue or discussion, GitHub processes it under its own privacy policy; we do not run a support inbox for general questions.',
            },
          ]}
        />
        {/* ASH-REVIEW: (a) confirm Resend is the live mail provider in
            production (config MAIL_PROVIDER); (b) confirm R2 backups are
            live, not just planned (ADR-0050 / off-site-backup-plan.md);
            (c) data-centre regions for Hetzner + Cloudflare and the
            international-transfer basis (SCCs / UK IDTA / adequacy). */}
        <p>
          Data is stored in{' '}
          <Placeholder>DATA-CENTRE REGION(S) — ASH TO CONFIRM</Placeholder>.
          Where a provider transfers data outside the UK or EEA, the transfer
          relies on{' '}
          <Placeholder>TRANSFER MECHANISM — ASH TO CONFIRM</Placeholder>.
        </p>
        <Aside>
          There is no payment processor. The Service has no paid tier and no
          billing surface (ADR-0049), so we never collect card or bank details.
        </Aside>
      </Section>

      <Section
        id="retention"
        title="6. How long we keep it"
        subtitle="The retention each record is actually held to"
      >
        <DefList
          rows={[
            {
              term: 'Magic-link tokens',
              def: 'Valid for 15 minutes. Expired tokens (email + requesting IP) are deleted automatically 48 hours after expiry; the delay preserves a short forensic window on sign-in floods.',
            },
            {
              term: 'Sign-in lockouts',
              def: 'Records of repeated failed sign-in codes (email + IP) are deleted automatically after 48 hours.',
            },
            {
              term: 'Sessions',
              def: 'A dashboard session lasts up to 30 days, or until you sign out or we revoke it.',
            },
            {
              term: 'API-key last-use',
              def: 'The last-used IP and user agent are overwritten on each use and kept while the key exists. Revoked keys are retained (hash, name, timestamps) so your dashboard can show rotation history.',
            },
            {
              term: 'Audit log',
              def: 'Kept online for 12 months, then archived.',
            },
            {
              term: 'Account and email',
              def: 'Kept while the account is open. Closed accounts are removed on request (section 7).',
            },
          ]}
        />
        {/* ASH-REVIEW: (a) there is no automatic sweep of EXPIRED session
            rows today (no sessionreaper package) — either add one or
            state that expired session records persist until manual
            deletion; (b) audit-log archive destination and its retention
            are not pinned in the repo ("archived to S3 by an offline
            job", migration 0027). */}
      </Section>

      <Section
        id="cookies"
        title="7. Cookies"
        subtitle="Two, both strictly necessary"
      >
        <p>
          The Service sets cookies only for signing in. Because both are
          strictly necessary for a feature you asked for, no consent banner is
          shown.
        </p>
        <DefList
          rows={[
            {
              term: 'Session cookie',
              def: 'Set when you sign in to the dashboard; identifies your session for up to 30 days. HttpOnly, Secure, SameSite.',
            },
            {
              term: 'Login-intent cookie',
              def: 'Set when you request a magic link and cleared when you use it; binds the link to the browser that asked for it so a link cannot be used to sign someone else in.',
            },
          ]}
        />
        <p>
          Anonymous browsing and API use set no cookies at all. Embedded widgets
          set none either.
        </p>
      </Section>

      <Section id="rights" title="8. Your rights" subtitle="GDPR and UK GDPR">
        <p>
          If you are in the UK or the EEA you have the right to access the
          personal data we hold about you, to have it corrected or erased, to
          restrict or object to its processing, to receive it in a portable
          form, and to withdraw any consent you have given. Equivalent rights
          may apply under other laws where you live.
        </p>
        <p>
          To exercise any of them, email{' '}
          <a
            href="mailto:security@stellarindex.io"
            className="text-brand-600 hover:underline"
          >
            security@stellarindex.io
          </a>{' '}
          from the address on your account, so we can verify it is you. We
          respond within one month. Most of what we hold is visible to you
          already in your{' '}
          <Link href="/dashboard" className="text-brand-600 hover:underline">
            dashboard
          </Link>{' '}
          (keys, sessions, usage), and you can revoke keys and sessions there
          yourself.
        </p>
        {/* ASH-REVIEW: erasure is a manual operator process — the
            self-service GDPR Art. 17 flow was DROPPED (PRV-1, 2026-08-15).
            Confirm the manual procedure and its turnaround, and note any
            data we must keep despite an erasure request (audit log for
            abuse cases, under Art. 17(3)). */}
        <p>
          When we erase an account we delete the email address, sessions,
          passkeys, and API keys. We may retain minimal audit-log entries where
          we need them to defend against abuse or legal claims.
        </p>
        {/* ASH-REVIEW: supervisory authority — ICO for UK, or the
            relevant EU DPA, depending on the controller's jurisdiction. */}
        <p>
          You also have the right to complain to a supervisory authority — in
          the UK, the Information Commissioner&rsquo;s Office; in the EEA, the
          authority in your member state.
        </p>
      </Section>

      <Section
        id="changes"
        title="9. Changes to this policy"
        subtitle="How you will find out"
      >
        <p>
          We will post changes here with a new &ldquo;last updated&rdquo; date,
          note material changes in the{' '}
          <Link href="/changelog" className="text-brand-600 hover:underline">
            changelog
          </Link>
          , and email account holders about any change that affects what we
          collect or how long we keep it. This policy is part of the{' '}
          <Link href="/terms" className="text-brand-600 hover:underline">
            terms of service
          </Link>
          .
        </p>
      </Section>
    </div>
  );
}

const TOC = [
  { id: 'controller', label: 'Who is responsible' },
  { id: 'anonymous', label: 'Browsing and anonymous API use' },
  { id: 'accounts', label: 'Accounts, sign-in and API keys' },
  { id: 'ip-addresses', label: 'Why we keep IP addresses at full resolution' },
  { id: 'processors', label: 'Who processes data for us' },
  { id: 'retention', label: 'How long we keep it' },
  { id: 'cookies', label: 'Cookies' },
  { id: 'rights', label: 'Your rights' },
  { id: 'changes', label: 'Changes to this policy' },
];

// DraftBanner is intentionally loud; remove it in the same commit that
// sets the real effective date.
function DraftBanner() {
  return (
    <div
      role="note"
      className="border-warn-300 bg-warn-50 text-warn-700 rounded-xl border p-4 text-sm"
    >
      <strong>Draft.</strong> This policy has not yet been reviewed by the
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
