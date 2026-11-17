import { readFileSync } from 'node:fs';
import { dirname, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';

import { render, screen } from '@testing-library/react';
import { describe, expect, it } from 'vitest';

import { AssetAbout, CURATED_ASSET_ABOUT } from './AssetAbout';

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

describe('CURATED_ASSET_ABOUT against the verified-currency registry', () => {
  const seed = readFileSync(
    resolve(
      dirname(fileURLToPath(import.meta.url)),
      '../../../../../../internal/currency/data/seed.yaml',
    ),
    'utf8',
  );
  const labels = new Map<string, string>();
  for (const block of seed.split(/\n {2}- ticker:/).slice(1)) {
    const label = /verified_issuer_label:\s*(.+)/.exec(block)?.[1];
    if (!label) continue;
    for (const m of block.matchAll(/asset_id:\s*(\S+)/g)) {
      labels.set(m[1], label.replace(/\s*\(.*$/, '').trim());
    }
  }

  it('keys every non-native entry on a registry asset id and names its issuer', () => {
    for (const [id, text] of Object.entries(CURATED_ASSET_ABOUT)) {
      if (id === 'native') continue;
      const org = labels.get(id);
      expect(org, `${id} is not a seed asset_id`).toBeTruthy();
      expect(text, `${id} must name ${org}`).toContain(org as string);
    }
  });
});
