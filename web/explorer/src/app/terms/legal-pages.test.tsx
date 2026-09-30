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
}

describe('legal pages', () => {
  it('terms is final and governed by the law of England and Wales', () => {
    render(<TermsPage />);
    expect(screen.getByRole('heading', { level: 1 })).toHaveTextContent(
      'Terms of Service',
    );
    expect(screen.queryByRole('note')).toBeNull();
    const body = document.body.textContent ?? '';
    expectFinal(body);
    expect(body).toMatch(
      /governed by the laws of England and Wales, and the courts of England and Wales have exclusive jurisdiction/,
    );
    expect(body).toMatch(/greater of GBP 100/);
    expect(body).toMatch(/operated by Stellar Index/);
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
    const body = document.body.textContent ?? '';
    expectFinal(body);
    // Retention figures mirror internal/magiclinkreaper (15 min TTL,
    // 48 h sweep) and dashboardauth.SessionTTL (30 days).
    expect(screen.getByText(/Valid for 15 minutes/)).toHaveTextContent(
      /48 hours/,
    );
    expect(screen.getByText(/lasts up to 30 days/)).toHaveTextContent(
      /deleted automatically 90 days after it expires/,
    );
    // logincodereaper 48 h; retentionreaper webhook deliveries 30 d;
    // usage_daily 12-month policy (migration 0167).
    expect(screen.getByText(/Records of repeated failed/)).toHaveTextContent(
      /48 hours after the lockout ends/,
    );
    expect(screen.getByText(/The delivery log/)).toHaveTextContent(
      /30 days after each delivery/,
    );
    expect(screen.getByText(/Daily per-endpoint counts/)).toHaveTextContent(
      /after 12 months/,
    );
    // Log store 30 d (Loki 720h), journal 14 d (journald MaxRetentionSec).
    expect(body).toMatch(
      /up to 30 days in our log store and up to 14\s+days in the host system journal/,
    );
    // Nothing reaps or archives audit_log; erasure pseudonymises it.
    expect(
      screen.getByText(/Nothing deletes or archives audit-log entries/),
    ).toHaveTextContent(/kept indefinitely.*pseudonymis/);
    expect(
      screen.getByText(/Kept indefinitely while the account/),
    ).toBeInTheDocument();
    expect(body).not.toMatch(/then archived/);
    expect(body).toMatch(/Hetzner in\s+Falkenstein, Germany/);
    expect(body).toMatch(/UK GDPR as the lead framework/);
    // Five cookies, set by internal/api/v1/dashboardauth.
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
