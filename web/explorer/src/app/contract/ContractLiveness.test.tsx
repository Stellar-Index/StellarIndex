import { render, screen } from '@testing-library/react';
import { describe, expect, it } from 'vitest';

import { ContractLiveness } from './ContractView';

describe('ContractLiveness', () => {
  it('shows Live with the ledgers left to expiry', () => {
    render(
      <ContractLiveness
        exists
        ttl={{ state: 'live', live_until: 1_500, as_of_ledger: 1_000 }}
      />,
    );
    expect(screen.getByText('Live')).toBeTruthy();
    expect(screen.getByText(/500 ledgers to expiry/)).toBeTruthy();
  });

  it('shows Archived without a countdown once the TTL lapsed', () => {
    render(
      <ContractLiveness
        exists
        ttl={{ state: 'archived', live_until: 900, as_of_ledger: 1_000 }}
      />,
    );
    expect(screen.getByText('Archived')).toBeTruthy();
    expect(screen.queryByText(/ledgers to expiry/)).toBeNull();
  });

  it('shows Not captured only when the API says the contract does not exist', () => {
    const { container, rerender } = render(<ContractLiveness exists={false} />);
    expect(screen.getByText('Not captured')).toBeTruthy();
    rerender(<ContractLiveness />);
    expect(container.textContent).toBe('');
  });
});
