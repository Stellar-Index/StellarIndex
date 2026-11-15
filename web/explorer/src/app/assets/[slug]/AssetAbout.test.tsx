import { render, screen } from '@testing-library/react';
import { describe, expect, it } from 'vitest';

import { AssetAbout } from './AssetAbout';

const USDC = 'USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN';
const FAKE_USDC =
  'USDC-GBNZILSTVQZ4R7IKQDGHYGY2QXL5QOFJYQMXPKWRRM5PAV7Y4M67AQUA';

describe('AssetAbout', () => {
  it('describes the asset whose id it is curated for', () => {
    render(<AssetAbout assetId={USDC} symbol="USDC" />);
    expect(
      screen.getByText(/issued by Circle Internet Financial/),
    ).toBeTruthy();
  });

  it('describes Stellar Lumens on the native asset only', () => {
    render(<AssetAbout assetId="native" symbol="XLM" />);
    expect(
      screen.getByText(/native asset of the Stellar network/),
    ).toBeTruthy();
  });

  it('says nothing about a look-alike wearing the same ticker', () => {
    const { container } = render(
      <AssetAbout assetId={FAKE_USDC} symbol="USDC" />,
    );
    expect(container.firstChild).toBeNull();
  });

  it('does not match a code, or an inherited object key', () => {
    for (const id of ['USDC', 'XLM', 'toString', 'constructor']) {
      const { container } = render(<AssetAbout assetId={id} symbol={id} />);
      expect(container.firstChild).toBeNull();
    }
  });
});
