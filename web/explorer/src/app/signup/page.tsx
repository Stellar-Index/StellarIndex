import type { Metadata } from 'next';
import Link from 'next/link';

import { API_BASE_URL } from '@/api/client';

import { SignInForm } from '../signin/SignInForm';

export const metadata: Metadata = {
  // Auth form — no SEO value; keep it out of the index (and the sitemap).
  robots: { index: false, follow: false },
  title: 'Create account',
  description:
    'Create your Stellar Index account. Magic-link email auth — no passwords. Accounts are free; a key adds per-key attribution and usage analytics, and higher partner rate limits are staff-set on request.',
};

const TIERS = [
  {
    name: 'Anonymous',
    rateLimit: '6,000 req/min per IP',
    cost: '$0',
    notes: 'Public read of every endpoint. No account, no key, no auth.',
  },
  {
    name: 'Free',
    rateLimit: '1,000 req/min per key',
    cost: '$0',
    notes:
      'Every registered account. A per-key budget and usage analytics, not extra throughput — it is lower than the anonymous per-IP limit by design. Sign in with the form on the right (first sign-in creates the account), or register straight from the terminal: POST /v1/register returns an account + API key in one call.',
    highlight: true,
  },
  {
    name: 'Partner',
    rateLimit: 'Staff-set (up to 100,000 req/min)',
    cost: '$0',
    notes:
      'For wallets, exchanges, and redistributors with heavy fan-out. Nothing to buy — email us with your use-case and we raise your account’s limits.',
  },
];

export default function SignupPage() {
  return (
    // Route-frame record (FEC A1-10, operator decision D4 2026-08-24):
    // max-w-4xl is DELIBERATE — this page carries the tier table + plan
    // copy beside the same SignInForm; /signin stays max-w-md as the bare
    // auth micro-surface. Vertical rhythm (py-12 sm:py-16) is the shared
    // auth-pair rhythm. Allowlisted for the census-2 route-frame tripwire.
    <div className="mx-auto w-full max-w-4xl px-4 py-12 sm:px-6 sm:py-16">
      <header className="mb-10">
        <h1 className="text-ink text-3xl font-bold tracking-tight sm:text-4xl">
          Create your account
        </h1>
        <p className="text-ink-body mt-3 max-w-2xl text-base">
          Magic-link sign-in — no passwords. Once you&apos;re in, mint API keys
          and watch usage under your account. Accounts are free; higher partner
          rate limits are staff-set on request.
        </p>
      </header>

      <section className="border-line bg-surface mb-12 rounded-xl border p-6 shadow-sm sm:p-8">
        <SignInForm mode="signup" />
        <p className="text-ink-muted mt-4 text-xs">
          Already have an account?{' '}
          <Link href="/signin" className="text-brand-600 hover:underline">
            Sign in
          </Link>{' '}
          — same magic-link form, just lands on your existing account.
        </p>
      </section>

      <section className="mb-12">
        <h2 className="text-ink mb-4 text-xl font-semibold">Tiers</h2>
        {/* overflow-x-auto, not -hidden: WCAG 1.4.10 Reflow — the nowrap
            tier headers were CLIPPED at 320px with no way to reach them.
            The radius still clips. */}
        <div className="border-line overflow-x-auto rounded-xl border">
          {/* table-wave:allowlist prose — marketing tier table (operator
              decision D5 2026-08-24): stays a hand-rolled <table>; the
              A2-01 data-table wave (Table primitives) must not sweep it. */}
          <table className="divide-line min-w-full divide-y">
            <thead className="bg-surface-muted">
              <tr>
                <th
                  scope="col"
                  className="text-ink-body px-4 py-3 text-left text-xs font-semibold tracking-wider uppercase"
                >
                  Tier
                </th>
                <th
                  scope="col"
                  className="text-ink-body px-4 py-3 text-left text-xs font-semibold tracking-wider uppercase"
                >
                  Rate limit
                </th>
                <th
                  scope="col"
                  className="text-ink-body px-4 py-3 text-left text-xs font-semibold tracking-wider uppercase"
                >
                  Cost
                </th>
              </tr>
            </thead>
            <tbody className="divide-line bg-surface divide-y">
              {TIERS.map((tier) => (
                <tr
                  key={tier.name}
                  className={tier.highlight ? 'bg-brand-50' : ''}
                >
                  <td className="text-ink px-4 py-3 text-sm font-semibold whitespace-nowrap">
                    {tier.name}
                    {tier.highlight && (
                      <span className="bg-brand-fill ml-2 inline-flex items-center rounded-full px-2 py-0.5 text-xs font-medium text-white">
                        you are here
                      </span>
                    )}
                  </td>
                  <td className="text-ink-body px-4 py-3 text-sm whitespace-nowrap">
                    {tier.rateLimit}
                  </td>
                  <td className="text-ink-body px-4 py-3 text-sm whitespace-nowrap">
                    {tier.cost === 'Contact sales' || tier.cost === 'Custom' ? (
                      <Link
                        href="/contact"
                        className="text-brand-600 hover:underline"
                      >
                        {tier.cost} →
                      </Link>
                    ) : (
                      tier.cost
                    )}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
        <ul className="text-ink-body mt-4 space-y-2 text-sm">
          {TIERS.map((tier) => (
            <li key={tier.name}>
              <strong className="text-ink">{tier.name}.</strong> {tier.notes}
            </li>
          ))}
        </ul>
      </section>

      <section className="border-warn-300 bg-warn-50 text-warn-700 rounded-xl border p-5 text-sm">
        <strong>Anonymous reads work without an account.</strong> If
        you&rsquo;re prototyping, just hit{' '}
        <a href="https://docs.stellarindex.io" className="underline">
          the public endpoints
        </a>{' '}
        directly — the 6,000 req/min per-IP budget covers exploratory scripts
        and low-traffic embeds. Create an account when you want a budget
        attributed to you rather than shared with your IP, plus usage analytics
        — not for extra throughput, since a free key is stamped at 1,000
        req/min. Ask us for a partner limit when you&rsquo;re ready to ship to
        customers at scale.
      </section>

      <p className="text-ink-muted mt-8 text-xs">
        API base URL: <code className="font-mono">{API_BASE_URL}</code>
      </p>
    </div>
  );
}
