import { describe, it, expect, vi } from 'vitest';
import { render, screen } from '@testing-library/react';

const replace = vi.fn();

vi.mock('@/api/hooks', async () => {
  const actual =
    await vi.importActual<typeof import('@/api/hooks')>('@/api/hooks');
  return { ...actual, useMe: vi.fn() };
});

vi.mock('next/navigation', async () => {
  const actual =
    await vi.importActual<typeof import('next/navigation')>('next/navigation');
  return { ...actual, useRouter: () => ({ ...actual.useRouter(), replace }) };
});

import { useMe } from '@/api/hooks';
import { AccountGate } from './AccountGate';

// error handling / auth availability: AccountGate used to have only two
// terminal states — loading skeleton, or signed-in — so an ERRORED auth
// probe (timeout, network failure, 5xx) fell through the same branch as
// "not signed in" and silently redirected to /signin, potentially
// bouncing a legitimately signed-in visitor whose probe merely failed to
// reach the server. It must now show a retry escape hatch instead.
describe('AccountGate', () => {
  it('shows a retry escape hatch on an errored probe, and does NOT redirect to /signin', () => {
    const refetch = vi.fn();
    vi.mocked(useMe).mockReturnValue({
      isLoading: false,
      isError: true,
      data: undefined,
      refetch,
    } as unknown as ReturnType<typeof useMe>);

    render(<AccountGate>{() => <div>dashboard content</div>}</AccountGate>);

    expect(
      screen.getByText(/Couldn.t verify your sign-in/),
    ).toBeInTheDocument();
    expect(screen.queryByText('dashboard content')).not.toBeInTheDocument();
  });

  it('still renders children when signed in', () => {
    vi.mocked(useMe).mockReturnValue({
      isLoading: false,
      isError: false,
      data: { user: { email: 'a@b.com' } },
      refetch: vi.fn(),
    } as unknown as ReturnType<typeof useMe>);

    render(<AccountGate>{() => <div>dashboard content</div>}</AccountGate>);
    expect(screen.getByText('dashboard content')).toBeInTheDocument();
  });
});

// The session-hint gate turns `useMe` OFF for a visitor with no hint, so
// AccountGate now has to cope with a query that will never run. TanStack
// reports that as pending + fetch-idle, i.e. `isLoading === false`. If
// the gate ever keyed its hold on `isPending` instead, every /dashboard/*
// page would sit on the skeleton forever for an anonymous visitor rather
// than bouncing them to /signin.
describe('AccountGate with a disabled probe', () => {
  const disabledQuery = {
    isLoading: false,
    isPending: true,
    fetchStatus: 'idle' as const,
    isError: false,
    data: undefined,
    refetch: vi.fn(),
  };

  it('redirects to /signin instead of hanging on the skeleton', () => {
    vi.mocked(useMe).mockReturnValue(
      disabledQuery as unknown as ReturnType<typeof useMe>,
    );

    render(<AccountGate>{() => <div>dashboard content</div>}</AccountGate>);

    expect(replace).toHaveBeenCalledWith('/signin');
    expect(screen.queryByText('dashboard content')).not.toBeInTheDocument();
    // No retry escape hatch either — an un-run probe is not a failed one.
    expect(
      screen.queryByText(/Couldn.t verify your sign-in/),
    ).not.toBeInTheDocument();
  });

  it('does not redirect while the probe is genuinely still loading', () => {
    vi.mocked(useMe).mockReturnValue({
      ...disabledQuery,
      isLoading: true,
      fetchStatus: 'fetching' as const,
    } as unknown as ReturnType<typeof useMe>);

    render(<AccountGate>{() => <div>dashboard content</div>}</AccountGate>);

    expect(replace).not.toHaveBeenCalled();
    expect(screen.queryByText('dashboard content')).not.toBeInTheDocument();
  });
});
