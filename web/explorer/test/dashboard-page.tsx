import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { render } from '@testing-library/react';
import { vi } from 'vitest';

import { useMe } from '@/api/hooks';

// The calling test file must vi.mock('@/api/hooks') with `useMe: vi.fn()`.
export function renderSignedInPage(
  ui: React.ReactElement,
  me: object = { user: { email: 'a@b.com' } },
) {
  vi.mocked(useMe).mockReturnValue({
    isLoading: false,
    isError: false,
    data: me,
    refetch: vi.fn(),
  } as unknown as ReturnType<typeof useMe>);

  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(<QueryClientProvider client={qc}>{ui}</QueryClientProvider>);
}
