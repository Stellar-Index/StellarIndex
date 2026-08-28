import { describe, it, expect, vi } from 'vitest';
import { render, screen } from '@testing-library/react';

import { Footer } from '@/components/nav/Footer';

import TermsPage from './page';
import PrivacyPage from '../privacy/page';
import SignupPage from '../signup/page';

vi.mock('next/navigation', () => ({
  usePathname: () => '/',
}));

// Legal surface (DRAFT, 2026-08-28): the pages exist so that an account
// holder can find the terms they agreed to and a data subject can find
// what is retained. These guards keep the two pages REACHABLE (footer +
// signup consent line) and keep the draft honest: the visible
// jurisdiction placeholder and the draft banner must not disappear
// until the operator's legal review replaces them in the same commit.
describe('legal pages', () => {
  it('terms renders the visible jurisdiction placeholder and draft banner', () => {
    render(<TermsPage />);
    expect(screen.getByRole('heading', { level: 1 })).toHaveTextContent(
      'Terms of Service',
    );
    expect(screen.getByRole('note')).toHaveTextContent(/draft/i);
    expect(
      screen.getAllByText(/JURISDICTION — ASH TO CONFIRM/).length,
    ).toBeGreaterThan(0);
    expect(
      screen.getByRole('link', { name: 'privacy policy' }),
    ).toHaveAttribute('href', '/privacy');
  });

  it('privacy states the retention the code enforces and the contact mailbox', () => {
    render(<PrivacyPage />);
    expect(screen.getByRole('heading', { level: 1 })).toHaveTextContent(
      'Privacy Policy',
    );
    expect(screen.getByRole('note')).toHaveTextContent(/draft/i);
    // Retention figures mirror internal/magiclinkreaper (15 min TTL,
    // 48 h sweep) and dashboardauth.SessionTTL (30 days).
    expect(screen.getByText(/Valid for 15 minutes/)).toHaveTextContent(
      /48 hours/,
    );
    expect(screen.getByText(/lasts up to 30 days/)).toBeInTheDocument();
    const mailto = screen.getAllByRole('link', {
      name: 'security@stellarindex.io',
    });
    expect(mailto.length).toBeGreaterThan(0);
    expect(mailto[0]).toHaveAttribute(
      'href',
      'mailto:security@stellarindex.io',
    );
  });

  it('footer and signup link to both legal pages', () => {
    render(<Footer />);
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
