import { describe, expect, it, vi } from 'vitest';
import { render, screen } from '@testing-library/react';

import { BackfillSummary } from './BackfillSummary';

const useCursors = vi.hoisted(() => vi.fn());
vi.mock('@/api/hooks', () => ({ useCursors }));

describe('BackfillSummary', () => {
  it('shows an error, not "Loading…", when the cursors read fails', () => {
    useCursors.mockReturnValue({
      data: undefined,
      isLoading: false,
      isError: true,
    });
    render(<BackfillSummary />);

    expect(screen.getByRole('alert').textContent).toMatch(/Failed to load/);
    expect(screen.queryByText(/Loading/)).toBeNull();
  });

  it('still shows the loading state while the first read is in flight', () => {
    useCursors.mockReturnValue({
      data: undefined,
      isLoading: true,
      isError: false,
    });
    render(<BackfillSummary />);

    expect(screen.getByText(/Loading backfill summary/)).toBeTruthy();
  });
});
