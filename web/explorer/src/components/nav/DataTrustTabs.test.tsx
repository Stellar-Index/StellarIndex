import { render, screen } from '@testing-library/react';
import { describe, expect, it } from 'vitest';

import { DataTrustTabs } from './DataTrustTabs';

describe('DataTrustTabs', () => {
  it('links every data-trust page and marks the current one', () => {
    render(<DataTrustTabs active="/methodology" />);
    for (const name of [
      'Status',
      'Diagnostics',
      'Sources',
      'Methodology',
      'Service level',
    ]) {
      expect(screen.getByRole('link', { name })).toBeInTheDocument();
    }
    expect(screen.getByRole('link', { name: 'Methodology' })).toHaveAttribute(
      'aria-current',
      'page',
    );
    expect(screen.getByRole('link', { name: 'Status' })).not.toHaveAttribute(
      'aria-current',
    );
  });
});
