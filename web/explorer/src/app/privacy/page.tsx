import type { Metadata } from 'next';
import Link from 'next/link';

import { LEGAL_ENTITY } from '../terms/page';

// States only what the code, migrations and ansible role do; correct it, do
// not reword it, when they change. Each factual paragraph names its source in
// a `source:` comment. api_usage_events has no writer (internal/platform/usage.go),
// so it is not listed.

export const metadata: Metadata = {
  title: 'Privacy Policy — Stellar Index',
  description:
    'What Stellar Index collects when you browse the explorer or use the API — account email, IP addresses and why they are kept, cookies — the processors involved, retention periods, and your GDPR / UK GDPR rights.',
  alternates: { canonical: '/privacy' },
};

const LAST_UPDATED = '2026-09-30';

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
          what an account and abuse-prevention use — an email address, IP
          addresses and browser user agents. Section 6 lists how long each is
          kept; some records have no automatic deletion.
        </p>
        <p className="text-ink-muted text-xs">Last updated: {LAST_UPDATED}</p>
      </header>

      <TableOfContents />

      <Section
        id="controller"
        title="1. Who is responsible"
        subtitle="The data controller"
      >
        {/* source: web/explorer/src/app/contact/page.tsx, SECURITY.md */}
        <p>
          The data controller for the Service is {LEGAL_ENTITY} (the
          &ldquo;Operator&rdquo;, &ldquo;we&rdquo;, &ldquo;us&rdquo;). Contact
          for anything in this policy:{' '}
          <a
            href="mailto:security@stellarindex.io"
            className="text-brand-600 hover:underline"
          >
            security@stellarindex.io
          </a>
          .
        </p>
        {/* source: docs/adr/0015-last-closed-bucket-rate-serving.md */}
        <p>
          The Service&rsquo;s servers and database are hosted by Hetzner in
          Falkenstein, Germany, so your personal data is processed in the EU. We
          apply the UK GDPR as the lead framework, and the EU GDPR applies to
          that processing as well.
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
        {/* source: internal/ratelimit/bucket.go, configs/ansible/roles/archival-node/tasks/10-observability.yml */}
        <DefList
          rows={[
            {
              term: 'IP address',
              def: 'Used to enforce the anonymous per-IP rate limit: a counter keyed by your IP address is held in our cache (Redis) for the current one-minute window and expires within minutes. It is not written to the account database.',
            },
            {
              term: 'Request logs',
              def: 'Standard server logs (request path, status, timing, user agent, IP) for operating and securing the Service.',
            },
          ]}
        />
        {/* source: configs/ansible/roles/archival-node/tasks/15-log-discipline.yml (MaxRetentionSec=14d; Loki 720h), configs/loki/loki.r1.yml, configs/ansible/roles/loki/defaults/main.yml */}
        <p>
          Our server logs, including web-server access logs with client IP
          addresses, are kept for up to 30 days in our log store and up to 14
          days in the host system journal, and are used only for operations,
          security, and abuse investigation.
        </p>
        {/* source: web/explorer/wrangler.toml */}
        <p>
          The explorer web site is served by Cloudflare, which keeps its own
          request logs under its own privacy policy; we do not set their
          retention.
        </p>
        {/* source: web/explorer/src/app/layout.tsx, web/explorer/public/_headers */}
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
        {/* source: internal/api/v1/dashboardauth/handlers.go, internal/api/v1/dashboardauth/passkey.go, internal/api/v1/account.go */}
        <p>
          Sign-in is passwordless: you enter an email address, we send a
          single-use magic link (or a short code), and clicking it creates a
          session. Passkeys (WebAuthn) can be added as a second sign-in method.
          We hold no passwords. An account can also be created from the terminal
          with <code>POST /v1/register</code>, which asks for nothing: a name
          and an email address are both optional, and an address given there is
          stored as a contact address without being verified. For an account we
          process:
        </p>
        {/* source: migrations/0027_platform_v1_schema.up.sql, migrations/0140_webauthn_credentials.up.sql, internal/usage/counter.go, internal/platform/postgresstore/account_erasure.go */}
        <DefList
          rows={[
            {
              term: 'Email address',
              def: 'Your sign-in identity and the address we send magic links and account notices to. Required to sign in to the dashboard; optional for an account created with POST /v1/register.',
            },
            {
              term: 'Sign-in requests',
              def: 'Each magic link we issue is recorded with the email address and the IP address that requested it. To limit abuse we also count requests per IP address and per email address over a rolling hour in our cache (Redis).',
            },
            {
              term: 'Account name',
              def: 'Derived from your email address when the account is created at sign-in, or the name you gave to POST /v1/register.',
            },
            {
              term: 'Passkey public keys',
              def: 'If you register a passkey, its public key and credential ID. The private key never leaves your device.',
            },
            {
              term: 'API keys',
              def: 'We store a hash of each key, its name, tier and limits, and — each time it is used — the last-used time, IP address and user agent. Your dashboard shows the last-used time; the IP address and user agent are included in your data export.',
            },
            {
              term: 'Usage counts',
              def: 'Per-account request counts by day and by endpoint (successful, client-error, server-error and rate-limited) for your quota and the usage chart in your dashboard, plus short-lived per-minute rate-limit counters. These are counts, not request bodies.',
            },
            {
              term: 'Sessions',
              def: "For each dashboard session: creation time, last-seen time, the first and most recent IP address, the user agent, and the country code our CDN attaches to the request (Cloudflare's CF-IPCountry header) when present.",
            },
            {
              term: 'Audit log',
              def: 'Security-relevant account actions (sign-in, key minted, key revoked, and similar) with the acting IP address and user agent.',
            },
            {
              term: 'Webhooks',
              def: 'If you register a webhook: its name, HTTPS URL, event types and signing secret, and a log of each delivery attempt including the payload sent and the response status.',
            },
            {
              term: 'Price alerts',
              def: 'If you create a price alert: the asset pair, condition, threshold and when it last fired.',
            },
          ]}
        />
        {/* source: internal/api/v1/signup.go */}
        <p>
          An API key can also be requested without a dashboard account through{' '}
          <code>POST /v1/signup</code>. For that we store a SHA-256 hash of the
          email address you give, with no expiry, so the same address cannot
          request a second key. Such a key is not part of an account, so the
          export and erasure in section 8 do not cover it; ask us by email.
        </p>
        <p>
          The lawful basis for each purpose (UK GDPR and EU GDPR Art. 6(1)):
        </p>
        <DefList
          rows={[
            {
              term: 'Contract',
              def: 'Your account, its email address and the messages we send to it: necessary to provide the account you asked for (Art. 6(1)(b)).',
            },
            {
              term: 'Legitimate interests',
              def: 'API-key usage records, sign-in and session records, and rate-limit counters: our legitimate interest in keeping the Service secure, preventing abuse and protecting your account (Art. 6(1)(f)).',
            },
            {
              term: 'Legal obligation',
              def: 'The audit log: needed to meet our obligations to secure personal data and to account for breaches (Art. 6(1)(c)), and our legitimate interest in investigating misuse of an account (Art. 6(1)(f)).',
            },
          ]}
        />
      </Section>

      <Section
        id="ip-addresses"
        title="4. Why we keep IP addresses at full resolution"
        subtitle="A deliberate, documented decision"
      >
        {/* source: internal/api/v1/dashboardauth/handlers.go, internal/platform/export.go */}
        <p>
          Several of the records above hold a full IP address rather than a
          truncated or hashed one. That is deliberate. The sign-in endpoint is
          unauthenticated by design — anyone can ask for a magic link to any
          address — so the IP behind each request is the only signal that lets
          us throttle inbox-bombing and detect an attacker minting links for
          someone else&rsquo;s mailbox. Likewise, the IP on a session and on an
          API key&rsquo;s last use is what lets us recognise a hijacked session
          or a leaked key; both are included in your data export. A masked
          prefix would defeat all three controls.
        </p>
        {/* source: internal/retentionreaper/reaper.go, internal/platform/postgresstore/account_erasure.go */}
        <p>
          Some IP-bearing records are deleted automatically and some are not:
          the last-used IP of an API key and the IP addresses on audit-log
          entries are kept until the account is erased (section 6). We do not
          use IP addresses for anything other than security, abuse prevention,
          and operating the Service. We do not geolocate you for advertising,
          profile you, or link your IP to third-party data.
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
        {/* source: internal/notify/resend.go, web/explorer/wrangler.toml, configs/ansible/roles/archival-node/templates/pgbackrest.conf.j2, docs/adr/0015-last-closed-bucket-rate-serving.md */}
        <DefList
          rows={[
            {
              term: 'Resend',
              def: 'Transactional email delivery. Receives your email address and the sign-in, verification and account-security messages we send to it.',
            },
            {
              term: 'Cloudflare',
              def: 'Serves this explorer web site from its edge network, so it sees your IP address and requests to the site in transit.',
            },
            {
              term: 'Off-site backup storage',
              def: 'Off-site copies of the database are encrypted (AES-256) before they leave our servers and are held in S3-compatible object storage separate from the host.',
            },
            {
              term: 'Hetzner',
              def: 'Hosts the API servers and the database in which account data lives, in Falkenstein, Germany.',
            },
            {
              term: 'GitHub',
              def: 'If you open an issue or discussion, GitHub processes it under its own privacy policy; we do not run a support inbox for general questions.',
            },
          ]}
        />
        <p>
          The account database is hosted in Germany. Where a provider above
          processes data outside the UK or EEA, the transfer relies on a UK or
          EU adequacy decision or on the standard contractual clauses in that
          provider&rsquo;s data-processing terms.
        </p>
        {/* source: docs/adr/0049-anonymous-access-and-passkey-auth.md */}
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
        {/* source: internal/magiclinkreaper/reaper.go, internal/logincodereaper/reaper.go, internal/api/v1/dashboardauth/handlers.go (SessionTTL), internal/retentionreaper/reaper.go, migrations/0167_usage_daily_retention.up.sql, migrations/0188_account_erasure.up.sql */}
        <DefList
          rows={[
            {
              term: 'Magic-link tokens',
              def: 'Valid for 15 minutes. Expired tokens (email + requesting IP) are deleted automatically 48 hours after expiry; the delay preserves a short forensic window on sign-in floods.',
            },
            {
              term: 'Sign-in lockouts',
              def: 'Records of repeated failed sign-in codes (email address only) are deleted automatically 48 hours after the lockout ends.',
            },
            {
              term: 'Sessions',
              def: 'A dashboard session lasts up to 30 days, or until you sign out or we revoke it. The session record (including its IP addresses and user agent) is deleted automatically 90 days after it expires or is revoked.',
            },
            {
              term: 'API keys',
              def: 'The last-used IP and user agent are overwritten on each use. Keys, including revoked keys and their last-use details, are kept until the account is erased; nothing deletes them sooner.',
            },
            {
              term: 'Usage counts',
              def: 'Daily per-endpoint counts are deleted automatically after 12 months (in practice up to about 15, as they are dropped in 90-day blocks).',
            },
            {
              term: 'Webhook deliveries',
              def: 'The delivery log, including payloads, is deleted automatically 30 days after each delivery.',
            },
            {
              term: 'Audit log',
              def: 'Nothing deletes or archives audit-log entries: they are kept indefinitely. An account erasure pseudonymises them by stripping the identifying fields (section 8).',
            },
            {
              term: 'Account and email',
              def: 'Kept indefinitely while the account exists; nothing deletes an inactive account. An erasure deletes the account record and pseudonymises the records that are kept (section 8).',
            },
          ]}
        />
      </Section>

      <Section
        id="cookies"
        title="7. Cookies"
        subtitle="Five, all for signing in"
      >
        {/* source: internal/api/v1/dashboardauth/{auth,session_hint,login_intent,passkey}.go */}
        <p>
          The Service sets cookies only for signing in. Because they are
          strictly necessary for a feature you asked for, no consent banner is
          shown.
        </p>
        <DefList
          rows={[
            {
              term: '__Host-stellarindex_session',
              def: 'Set when you sign in to the dashboard; identifies your session and expires with it (up to 30 days). HttpOnly, Secure, SameSite=Lax.',
            },
            {
              term: 'stellarindex_session_present',
              def: 'Set alongside the session cookie with the value "1" and the same lifetime, so this web site can tell that you are signed in without reading the session itself. Readable by scripts on the site; SameSite=Lax.',
            },
            {
              term: '__Host-stellarindex_login_intent',
              def: 'Set when you request a magic link and cleared when you use it; lasts at most 15 minutes. Binds the link to the browser that asked for it so a link cannot be used to sign someone else in. HttpOnly, Secure, SameSite=Lax.',
            },
            {
              term: 'stellarindex_login_device',
              def: 'Set each time you sign in and kept for 400 days. Holds an expiry time and a keyed hash of your email address (not the address itself), so this browser can still request a sign-in link for that address when the per-address sending limit has been used up by someone else. Sent only to the sign-in endpoint; it signs nothing in. HttpOnly.',
            },
            {
              term: '__Host-stellarindex_passkey_ceremony',
              def: 'Set while you register or use a passkey; lasts at most 5 minutes. HttpOnly, Secure, SameSite=Lax.',
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
          respond within one month. Your{' '}
          <Link href="/dashboard" className="text-brand-600 hover:underline">
            dashboard
          </Link>{' '}
          shows your API keys, usage, passkeys, price alerts and webhooks, and
          you can revoke keys there yourself; it has no view of your sessions
          other than signing out.
        </p>
        {/* source: internal/api/v1/dashboardauth/account.go (accountReauthWindow), internal/platform/export.go */}
        <p>
          An account owner can also act directly through the API, signed in to
          the dashboard (an API key cannot do either) and within 10 minutes of
          signing in: <code>GET /v1/dashboard/account/export</code> returns
          everything we hold about the account as a JSON download, and{' '}
          <code>DELETE /v1/dashboard/account</code> erases it. The explorer has
          no button for either yet.
        </p>
        {/* source: internal/platform/postgresstore/account_erasure.go, internal/accounterasure/eraser.go */}
        <p>
          An erasure takes effect immediately; there is no grace period and it
          cannot be undone. It deletes the account, its members&rsquo; sign-in
          records, sessions and passkeys, API keys, webhooks and their delivery
          log, price alerts, pending invitations and sign-in tokens, and the
          account&rsquo;s cached counters. Erasing through the API emails each
          owner a confirmation; an erasure we carry out on an emailed request
          does not.
        </p>
        {/* source: migrations/0188_account_erasure.up.sql (audit_log_erase_metadata), internal/platform/postgresstore/account_erasure.go, docs/operations/runbooks/account-erasure.md */}
        <p>
          Some records are kept after an erasure, pseudonymised so they are no
          longer linked to you. Audit-log entries are kept indefinitely with
          their IP address and user agent removed (entries recording an action
          by our staff keep the staff member&rsquo;s), and with the account
          name, email address, account identifiers, key labels, passkey names
          and suspension reasons stripped from their details; daily usage counts
          are re-labelled with a random identifier and age out after 12 months;
          and a hash of the account name is kept so the name cannot be reused.
          An erasure does not reach database backups and snapshots, which we do
          not promise to expire on any schedule; server logs, which age out
          after up to 30 days; or copies of sent email held by our email
          provider under its own retention. If we restore from a backup, our
          restore procedure re-applies every erasure before the API serves the
          restored data.
        </p>
        <p>
          You also have the right to complain to a supervisory authority: our
          lead authority is the UK Information Commissioner&rsquo;s Office; in
          the EEA you may also complain to the authority in your member state.
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
