import { describe, expect, it } from 'vitest';
import { fireEvent, render, screen } from '@testing-library/react';

import { DataSourceDisclosures } from './DataSourceDisclosures';

describe('DataSourceDisclosures', () => {
  it('opens a dialog carrying the CoinGecko and ECB credits, and closes on Escape', () => {
    render(<DataSourceDisclosures />);
    expect(screen.queryByRole('dialog')).toBeNull();
    fireEvent.click(
      screen.getByRole('button', { name: 'Data Source Disclosures' }),
    );
    const dialog = screen.getByRole('dialog', {
      name: 'Data source disclosures',
    });
    expect(dialog).toHaveTextContent('Price data powered by CoinGecko.');
    expect(dialog).toHaveTextContent(
      'Source: ECB euro foreign exchange reference rates.',
    );
    fireEvent.keyDown(document, { key: 'Escape' });
    expect(screen.queryByRole('dialog')).toBeNull();
  });
});
