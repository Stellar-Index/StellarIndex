import type { Metadata } from 'next';
import { describe, expect, it } from 'vitest';

import { metadata as account } from './accounts/[g]/page';
import { generateMetadata as asset } from './assets/[slug]/page';
import { metadata as contract } from './contracts/[id]/page';
import { generateMetadata as externalAsset } from './external/assets/[slug]/page';
import { metadata as creator } from './insights/creators/[address]/page';
import { metadata as sponsor } from './insights/sponsors/[address]/page';
import { generateMetadata as issuer } from './issuers/[g_strkey]/page';
import { metadata as ledger } from './ledgers/[seq]/page';
import { generateMetadata as lendingPool } from './lending/[pool]/page';
import { generateMetadata as pair } from './markets/[pair]/page';
import { generateMetadata as source } from './sources/[name]/page';
import { metadata as transaction } from './transactions/[hash]/page';

// Each of these documents is served for every id under its route. Next
// merges metadata per top-level key, so a key the page omits inherits the
// root layout's homepage value — canonical '/', og:title, twitter:title.
const SHELLS: [string, () => Promise<Metadata>][] = [
  ['/accounts/[g]', async () => account],
  ['/contracts/[id]', async () => contract],
  ['/insights/creators/[address]', async () => creator],
  ['/insights/sponsors/[address]', async () => sponsor],
  ['/ledgers/[seq]', async () => ledger],
  ['/transactions/[hash]', async () => transaction],
  [
    '/assets/shell',
    () => asset({ params: Promise.resolve({ slug: 'shell' }) }),
  ],
  [
    '/external/assets/shell',
    () => externalAsset({ params: Promise.resolve({ slug: 'shell' }) }),
  ],
  [
    '/issuers/shell',
    () => issuer({ params: Promise.resolve({ g_strkey: 'shell' }) }),
  ],
  [
    '/lending/shell',
    () => lendingPool({ params: Promise.resolve({ pool: 'shell' }) }),
  ],
  [
    '/markets/shell',
    () => pair({ params: Promise.resolve({ pair: 'shell' }) }),
  ],
  [
    '/sources/shell',
    () => source({ params: Promise.resolve({ name: 'shell' }) }),
  ],
];

describe('long-tail shell metadata', () => {
  it.each(SHELLS)(
    '%s overrides every homepage-inherited tag',
    async (_, get) => {
      const m = await get();
      expect(m.robots).toEqual({ index: false, follow: true });
      expect(m.alternates).toBeDefined();
      expect(m.alternates?.canonical).toBeUndefined();

      const title = String(m.title);
      expect(m.openGraph?.title).toContain(title);
      expect(m.openGraph).not.toHaveProperty('url');
      expect(m.twitter?.title).toContain(title);
    },
  );
});
