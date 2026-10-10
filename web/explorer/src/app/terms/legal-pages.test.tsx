import { describe, it, expect, vi, beforeEach } from 'vitest';
import { render, screen } from '@testing-library/react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';

import { SidebarNav } from '@/components/nav/Sidebar';

import TermsPage from './page';
import PrivacyPage from '../privacy/page';
import SignupPage from '../signup/page';

vi.mock('next/navigation', () => ({
  usePathname: () => '/',
}));

// Hermetic: SidebarNav polls /v1/status and /v1/me on mount.
beforeEach(() => {
  vi.spyOn(globalThis, 'fetch').mockRejectedValue(
    new TypeError('no network in tests'),
  );
});

// Keeps both pages reachable (sidebar rail, signup consent line) and every
// stated figure pinned to the code that enforces it.
function expectFinal(body: string) {
  expect(body).not.toMatch(/TO BE CONFIRMED|DRAFT|\bdraft\b/i);
  expect(body).toMatch(/Last updated: \d{4}-\d{2}-\d{2}(?!\d)/);
  expect(body).toMatch(/Policy history ?2026-09-30 — first published/);
}

// Whitespace-normalised: prettier may re-wrap JSX paragraphs.
function bodyText() {
  return (document.body.textContent ?? '').replace(/\s+/g, ' ');
}

const ENTITY =
  /Loop Finance Ltd \(company no\. 16862033\), a company registered in England and Wales, trading as Stellar Index/;
const OFFICE =
  /Unit 9 Vinnetrow Business Centre, Vinnetrow Road, Runcton, Chichester, England, PO20 1QH/;

function expectBody(body: string, matches: RegExp[], absent: RegExp[]) {
  for (const re of matches) expect(body).toMatch(re);
  for (const re of absent) expect(body).not.toMatch(re);
}

function withClient(ui: React.ReactElement) {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  render(<QueryClientProvider client={client}>{ui}</QueryClientProvider>);
}

function expectLinks(links: [name: string, href: string][]) {
  for (const [name, href] of links) {
    expect(screen.getByRole('link', { name })).toHaveAttribute('href', href);
  }
}

function openPage(page: React.ReactElement, h1: string) {
  render(page);
  expect(screen.getByRole('heading', { level: 1 })).toHaveTextContent(h1);
  expect(screen.queryByRole('note')).toBeNull();
  const body = bodyText();
  expectFinal(body);
  return body;
}

describe('legal pages', () => {
  it('terms is final and governed by the law of England and Wales', () => {
    const body = openPage(<TermsPage />, 'Terms of Service');
    expectBody(
      body,
      [
        /governed by the laws of England and Wales, and the courts of England and Wales have exclusive jurisdiction/,
        /greater of GBP 100/,
        ENTITY,
        OFFICE,
        // Both budgets are keyed on the account (middleware.authenticatedRateLimitKey,
        // UsageKeyForSubject; platform.Tier.MaxMonthlyQuota = 1,000,000 for free).
        /All keys on an account share one rate limit \(currently 1,000 requests per minute\) and one monthly request quota \(currently 1,000,000 requests\)/,
        // Anonymous limit: stellarindex.toml.j2 anon_rate_limit_per_min, /64 keying.
        /6,000 requests per minute; IPv6 callers share one limit per \/64 block/,
        // Venue feeds: no "tier-restricted or paid feeds" claim — nothing enforces it.
        /check them before redistributing \/v1\/observations data/,
        // Erasure needs the slug typed back (dashboardauth/account.go).
        /you type the account slug back to confirm/,
        // No bulk sender exists; notice is the page + policy history.
        /where we hold a verified email address for the account, sent to it\. The changelog is not the notice/,
        // SECURITY.md's disclosure commitments, verbatim.
        /acknowledge receipt within 72 hours, provide an initial assessment within 7 days, and fix HIGH \/ CRITICAL issues within 30 days/,
      ],
      [/per-key/, /tier-restricted or paid feeds/, /14 days before/],
    );
    expect(
      screen
        .getAllByRole('link', { name: 'privacy policy' })
        .map((a) => a.getAttribute('href'))
        .sort(),
    ).toEqual(['/privacy', '/privacy#rights']);
  });

  it('privacy states the retention the code enforces and the contact mailbox', () => {
    const body = openPage(<PrivacyPage />, 'Privacy Policy');
    expectBody(
      body,
      [
        ENTITY,
        OFFICE,
        /acknowledge every message within 72 hours/,
        /We have not appointed an EU representative under Article 27 GDPR; we rely on the Article 27\(2\) exemption/,
        // Loki 720h, journald MaxRetentionSec=14d, logrotate weekly x rotate 10.
        /up to 30 days in our log store \(Loki\) and up to 14 days in the host system journal\. Application and database log files on the host are rotated weekly and kept for about ten weeks/,
        /server logs, which age out after about ten weeks/,
        /hosted by Hetzner in Germany \(mainnet\) and Finland \(test networks\)/,
        // Regulator wording: never the EU one-stop-shop "lead authority".
        /The UK regulator is the Information Commissioner.s Office \(ICO\)\. If you are in the EEA you may also complain to your national supervisory authority/,
        // Storage: only the one-shot sessionStorage reload flag
        // (assets/[slug]/AssetClientFallback.tsx); nothing writes localStorage.
        /It sets one sessionStorage flag/,
        // api.* is not behind Cloudflare, so CF-IPCountry is never populated.
        /a country code where the request carries one \(empty in production\)/,
        // Client error beacon (functions/client-errors.js) and the issuer icon proxy.
        /sends an error report/,
        /loads them from our own .*\/icon.* endpoint/,
        /does not see your IP address/,
        /the names of the parameters it used, and the values of a fixed list of enumerated or numeric ones \(for example limit and order_by\); any other parameter is reduced to its name, so free-text and identifying values are never logged/,
        // Processors with country + safeguard (dns-email-perimeter.md).
        /Amazon SES in the us-east-1 \(United States\) region/,
        /Google Workspace/,
        /ask us for a copy of the clauses that apply to your data/,
        // Lawful bases: named legitimate interest; no 6(1)(c) record today;
        // Art. 13(2)(e) and Art. 21 stated explicitly.
        /keeping the Service secure and available, and enforcing fair use \(Art\. 6\(1\)\(f\)\)/,
        /None today: we keep no record because a law requires it/,
        /An email address is needed to sign in to the dashboard; without one you can still use anonymous reads/,
        /right to object, on grounds relating to your particular situation/,
        // Unsalted email hash is personal data (signup.go).
        /An unsalted hash of an email is still personal data/,
        // Export is the export.go field list, not "everything we hold"; slug confirm.
        /the account, its members, sessions, passkeys, API keys, webhooks, price alerts, invitations, usage counts and audit log/,
        /you type the account slug back to confirm/,
        // Backups: repo1 is unencrypted on r1 (pgbackrest-encryption.md), only
        // the off-site repo2 copy is AES-256; ZFS 7 d; restore hedge
        // (account-erasure.md is a manual runbook step).
        /sits on the database host and is not separately encrypted\. Local snapshots are kept for 7 days/,
        /The off-site copy is AES-256 encrypted and expires on a rolling schedule of a few weeks/,
        /if a restore predates your erasure request, tell us and we will erase again/,
        /Anonymous browsing sets none of ours/,
        /__cf_bm/,
        /The changelog is not the notice/,
      ],
      [
        /30 days after each delivery/,
        /10 weeks/,
        /then archived/,
        /lead authority|lead framework/,
        /stores some display preferences/,
        /CF-IPCountry|our CDN attaches/,
        /everything we hold/,
        /Backups are encrypted/,
        /not promise to expire/,
        /privacy@/,
      ],
    );
    // Retention figures mirror internal/magiclinkreaper (15 min TTL, 48 h
    // sweep), dashboardauth.SessionTTL (30 d), logincodereaper 48 h,
    // webhook_store.SweepFinishedDeliveries (30 d, only once no retry is
    // pending), usage_daily 12 months (migration 0167); audit_log is never
    // reaped, erasure pseudonymises it.
    for (const [row, text] of [
      [/Valid for 15 minutes/, /48 hours/],
      [/lasts up to 30 days/, /deleted automatically 90 days after it expires/],
      [/Records of repeated failed/, /48 hours after the lockout ends/],
      [
        /The delivery log/,
        /30 days after it was queued, once no retry is pending/,
      ],
      [/Daily per-endpoint counts/, /after 12 months/],
      [
        /Nothing deletes or archives audit-log entries/,
        /kept indefinitely.*pseudonymis/,
      ],
    ] as const) {
      expect(screen.getByText(row)).toHaveTextContent(text);
    }
    expect(
      screen.getByText(/Kept indefinitely while the account/),
    ).toBeInTheDocument();
    // Five cookies, set by internal/api/v1/dashboardauth; CDN challenge cookie.
    for (const name of [
      '__Host-stellarindex_session',
      'stellarindex_session_present',
      '__Host-stellarindex_login_intent',
      'stellarindex_login_device',
      '__Host-stellarindex_passkey_ceremony',
    ]) {
      expect(screen.getByText(name)).toBeInTheDocument();
    }
    const mailto = screen.getAllByRole('link', {
      name: 'security@stellarindex.io',
    });
    expect(mailto.length).toBeGreaterThan(0);
    expect(mailto[0]).toHaveAttribute(
      'href',
      'mailto:security@stellarindex.io',
    );
  });

  it('the mounted sidebar rail links to both legal pages', () => {
    withClient(<SidebarNav />);
    expectLinks([
      ['Terms of service', '/terms'],
      ['Privacy policy', '/privacy'],
    ]);
  });

  it('signup carries the consent line linking both pages', () => {
    render(<SignupPage />);
    expectLinks([
      ['terms of service', '/terms'],
      ['privacy policy', '/privacy'],
    ]);
  });
});
