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

// Hermetic: SidebarNav's Status dot polls /v1/status on mount and useMe
// hits /v1/me — never let a component test reach the network.
beforeEach(() => {
  vi.spyOn(globalThis, 'fetch').mockRejectedValue(
    new TypeError('no network in tests'),
  );
});

// These guards keep the two pages REACHABLE (the sidebar rail — the ONLY
// site-wide chrome; nav/Footer.tsx is unmounted — plus the signup consent
// line) and keep every stated figure pinned to the code that enforces it.
function expectFinal(body: string) {
  expect(body).not.toMatch(/TO BE CONFIRMED|DRAFT|\bdraft\b/i);
  expect(body).toMatch(/Last updated: \d{4}-\d{2}-\d{2}(?!\d)/);
  expect(body).toMatch(/Policy history ?2026-09-30 — first published/);
}

// JSX collapses source line breaks to single spaces; prettier may re-wrap
// a paragraph, so match on a whitespace-normalised body.
function bodyText() {
  return (document.body.textContent ?? '').replace(/\s+/g, ' ');
}

const ENTITY =
  /Loop Finance Ltd \(company no\. 16862033\), a company registered in England and Wales, trading as Stellar Index/;
const OFFICE =
  /Unit 9 Vinnetrow Business Centre, Vinnetrow Road, Runcton, Chichester, England, PO20 1QH/;

describe('legal pages', () => {
  it('terms is final and governed by the law of England and Wales', () => {
    render(<TermsPage />);
    expect(screen.getByRole('heading', { level: 1 })).toHaveTextContent(
      'Terms of Service',
    );
    expect(screen.queryByRole('note')).toBeNull();
    const body = bodyText();
    expectFinal(body);
    expect(body).toMatch(
      /governed by the laws of England and Wales, and the courts of England and Wales have exclusive jurisdiction/,
    );
    expect(body).toMatch(/greater of GBP 100/);
    expect(body).toMatch(ENTITY);
    expect(body).toMatch(OFFICE);
    // Both budgets are keyed on the account (middleware.authenticatedRateLimitKey,
    // UsageKeyForSubject; platform.Tier.MaxMonthlyQuota = 1,000,000 for free).
    expect(body).toMatch(
      /All keys on an account share one rate limit \(currently 1,000 requests per minute\) and one monthly request quota \(currently 1,000,000 requests\)/,
    );
    expect(body).not.toMatch(/per-key/);
    // Anonymous limit: stellarindex.toml.j2 anon_rate_limit_per_min, /64 keying.
    expect(body).toMatch(
      /6,000 requests per minute; IPv6 callers share one limit per \/64 block/,
    );
    // Venue feeds: no "tier-restricted or paid feeds" claim — nothing enforces it.
    expect(body).toMatch(
      /check them before redistributing \/v1\/observations data/,
    );
    expect(body).not.toMatch(/tier-restricted or paid feeds/);
    // Erasure needs the slug typed back (dashboardauth/account.go).
    expect(body).toMatch(/you type the account slug back to confirm/);
    // No bulk sender exists; notice is the page + policy history.
    expect(body).toMatch(
      /where we hold a verified email address for the account, sent to it\. The changelog is not the notice/,
    );
    expect(body).not.toMatch(/14 days before/);
    // SECURITY.md's disclosure commitments, verbatim.
    expect(body).toMatch(
      /acknowledge receipt within 72 hours, provide an initial assessment within 7 days, and fix HIGH \/ CRITICAL issues within 30 days/,
    );
    expect(
      screen
        .getAllByRole('link', { name: 'privacy policy' })
        .map((a) => a.getAttribute('href'))
        .sort(),
    ).toEqual(['/privacy', '/privacy#rights']);
  });

  it('privacy states the retention the code enforces and the contact mailbox', () => {
    render(<PrivacyPage />);
    expect(screen.getByRole('heading', { level: 1 })).toHaveTextContent(
      'Privacy Policy',
    );
    expect(screen.queryByRole('note')).toBeNull();
    const body = bodyText();
    expectFinal(body);
    expect(body).toMatch(ENTITY);
    expect(body).toMatch(OFFICE);
    expect(body).toMatch(/acknowledge every message within 72 hours/);
    expect(body).toMatch(
      /We have not appointed an EU representative under Article 27 GDPR; we rely on the Article 27\(2\) exemption/,
    );
    // Retention figures mirror internal/magiclinkreaper (15 min TTL,
    // 48 h sweep) and dashboardauth.SessionTTL (30 days).
    expect(screen.getByText(/Valid for 15 minutes/)).toHaveTextContent(
      /48 hours/,
    );
    expect(screen.getByText(/lasts up to 30 days/)).toHaveTextContent(
      /deleted automatically 90 days after it expires/,
    );
    // logincodereaper 48 h; webhook_store.SweepFinishedDeliveries deletes
    // 30 d after created_at only once no retry is pending; usage_daily
    // 12-month policy (migration 0167).
    expect(screen.getByText(/Records of repeated failed/)).toHaveTextContent(
      /48 hours after the lockout ends/,
    );
    expect(screen.getByText(/The delivery log/)).toHaveTextContent(
      /30 days after it was queued, once no retry is pending/,
    );
    expect(body).not.toMatch(/30 days after each delivery/);
    expect(screen.getByText(/Daily per-endpoint counts/)).toHaveTextContent(
      /after 12 months/,
    );
    // Loki 720h, journald MaxRetentionSec=14d, logrotate weekly x rotate 10.
    expect(body).toMatch(
      /up to 30 days in our log store \(Loki\) and up to 14 days in the host system journal\. Application and database log files on the host are rotated weekly and kept for about ten weeks/,
    );
    expect(body).toMatch(/server logs, which age out after about ten weeks/);
    expect(body).not.toMatch(/10 weeks/);
    // Nothing reaps or archives audit_log; erasure pseudonymises it.
    expect(
      screen.getByText(/Nothing deletes or archives audit-log entries/),
    ).toHaveTextContent(/kept indefinitely.*pseudonymis/);
    expect(
      screen.getByText(/Kept indefinitely while the account/),
    ).toBeInTheDocument();
    expect(body).not.toMatch(/then archived/);
    expect(body).toMatch(
      /hosted by Hetzner in Germany \(mainnet\) and Finland \(test networks\)/,
    );
    // Regulator wording: never the EU one-stop-shop "lead authority".
    expect(body).toMatch(
      /The UK regulator is the Information Commissioner.s Office \(ICO\)\. If you are in the EEA you may also complain to your national supervisory authority/,
    );
    expect(body).not.toMatch(/lead authority|lead framework/);
    // Storage: only the one-shot sessionStorage reload flag
    // (assets/[slug]/AssetClientFallback.tsx); nothing writes localStorage.
    expect(body).toMatch(/It sets one sessionStorage flag/);
    expect(body).not.toMatch(/stores some display preferences/);
    // api.* is not behind Cloudflare, so CF-IPCountry is never populated.
    expect(body).toMatch(
      /a country code where the request carries one \(empty in production\)/,
    );
    expect(body).not.toMatch(/CF-IPCountry|our CDN attaches/);
    // Client error beacon (functions/client-errors.js) and issuer icon hosts.
    expect(body).toMatch(/sends an error report/);
    expect(body).toMatch(/your browser fetches it from that host directly/);
    expect(body).toMatch(
      /the names of the parameters it used, and the values of a fixed list of enumerated or numeric ones \(for example limit and order_by\); any other parameter is reduced to its name, so free-text and identifying values are never logged/,
    );
    // Processors with country + safeguard (dns-email-perimeter.md).
    expect(body).toMatch(
      /Amazon SES in the us-east-1 \(United States\) region/,
    );
    expect(body).toMatch(/Google Workspace/);
    expect(body).toMatch(
      /ask us for a copy of the clauses that apply to your data/,
    );
    // Lawful bases: named legitimate interest; no 6(1)(c) record today;
    // Art. 13(2)(e) and Art. 21 stated explicitly.
    expect(body).toMatch(
      /keeping the Service secure and available, and enforcing fair use \(Art\. 6\(1\)\(f\)\)/,
    );
    expect(body).toMatch(
      /None today: we keep no record because a law requires it/,
    );
    expect(body).toMatch(
      /An email address is needed to sign in to the dashboard; without one you can still use anonymous reads/,
    );
    expect(body).toMatch(
      /right to object, on grounds relating to your particular situation/,
    );
    // Unsalted email hash is personal data (signup.go).
    expect(body).toMatch(/An unsalted hash of an email is still personal data/);
    // Export is the export.go field list, not "everything we hold"; slug confirm.
    expect(body).toMatch(
      /the account, its members, sessions, passkeys, API keys, webhooks, price alerts, invitations, usage counts and audit log/,
    );
    expect(body).not.toMatch(/everything we hold/);
    expect(body).toMatch(/you type the account slug back to confirm/);
    // Backups: repo1 is unencrypted on r1 (pgbackrest-encryption.md), only
    // the off-site repo2 copy is AES-256; ZFS 7 d; restore hedge
    // (account-erasure.md is a manual runbook step).
    expect(body).toMatch(
      /sits on the database host and is not separately encrypted\. Local snapshots are kept for 7 days/,
    );
    expect(body).toMatch(
      /The off-site copy is AES-256 encrypted and expires on a rolling schedule of a few weeks/,
    );
    expect(body).not.toMatch(/Backups are encrypted/);
    expect(body).toMatch(
      /if a restore predates your erasure request, tell us and we will erase again/,
    );
    expect(body).not.toMatch(/not promise to expire/);
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
    expect(body).toMatch(/Anonymous browsing sets none of ours/);
    expect(body).toMatch(/__cf_bm/);
    expect(body).toMatch(/The changelog is not the notice/);
    const mailto = screen.getAllByRole('link', {
      name: 'security@stellarindex.io',
    });
    expect(mailto.length).toBeGreaterThan(0);
    expect(mailto[0]).toHaveAttribute(
      'href',
      'mailto:security@stellarindex.io',
    );
    expect(body).not.toMatch(/privacy@/);
  });

  it('the mounted sidebar rail links to both legal pages', () => {
    const client = new QueryClient({
      defaultOptions: { queries: { retry: false } },
    });
    render(
      <QueryClientProvider client={client}>
        <SidebarNav />
      </QueryClientProvider>,
    );
    expect(
      screen.getByRole('link', { name: 'Terms of service' }),
    ).toHaveAttribute('href', '/terms');
    expect(
      screen.getByRole('link', { name: 'Privacy policy' }),
    ).toHaveAttribute('href', '/privacy');
  });

  it('signup carries the consent line linking both pages', () => {
    render(<SignupPage />);
    expect(
      screen.getByRole('link', { name: 'terms of service' }),
    ).toHaveAttribute('href', '/terms');
    expect(
      screen.getByRole('link', { name: 'privacy policy' }),
    ).toHaveAttribute('href', '/privacy');
  });
});
