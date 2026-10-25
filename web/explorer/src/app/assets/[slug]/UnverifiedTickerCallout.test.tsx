import { describe, it, expect } from 'vitest';
import { render, screen } from '@testing-library/react';

import { UnverifiedTickerCallout } from './UnverifiedTickerCallout';

describe('UnverifiedTickerCallout', () => {
  it('links the verified asset when it is issued on Stellar', () => {
    render(
      <UnverifiedTickerCallout
        symbol="USDC"
        warning={{
          verified_slug: 'usdc',
          verified_asset_id:
            'USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN',
          verified_name: 'USD Coin',
          note: 'Exercise caution.',
        }}
      />,
    );
    expect(screen.getByRole('link', { name: 'USD Coin' })).toHaveAttribute(
      'href',
      expect.stringMatching(/^\/assets\/usdc\/?$/),
    );
  });

  // XRP has no Stellar issuance: /assets/xrp renders "Asset not found".
  it('links nothing when no Stellar issuance exists', () => {
    render(
      <UnverifiedTickerCallout
        symbol="XRP"
        warning={{
          verified_slug: 'xrp',
          verified_asset_id: '',
          verified_name: 'XRP',
          note: 'Any asset trading under this code on Stellar is not XRP.',
        }}
      />,
    );
    expect(screen.queryByRole('link')).toBeNull();
    expect(screen.getByRole('alert')).toHaveTextContent(/is not XRP\.$/);
  });
});
