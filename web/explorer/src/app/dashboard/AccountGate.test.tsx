import { describe, it, expect, vi } from 'vitest';
import { render, screen } from '@testing-library/react';

vi.mock('@/api/hooks', async () => {
  const actual = await vi.importActual<typeof import('@/api/hooks')>('@/api/hooks');
  return { ...actual, useMe: vi.fn() };
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

    expect(screen.getByText(/Couldn.t verify your sign-in/)).toBeInTheDocument();
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
