import { describe, expect, it } from 'vitest';

import { exportedHtmlPath, routesFromLiteral, staticFrame } from './nav-shell';

const page = (main: string) =>
  `<html><body><nav>Ledgers Transactions</nav><main>${main}</main></body></html>`;

describe('staticFrame', () => {
  it('reads the h1 count and visible text out of <main>', () => {
    const f = staticFrame(
      page('<h1>Transactions</h1><p>Newest ledger first.</p>'),
    );
    expect(f).toEqual({
      h1Count: 1,
      text: 'Transactions Newest ledger first.',
    });
  });

  it('scores an unresolved Suspense boundary as empty, not as content', () => {
    // What `<Suspense fallback={null}>` bakes under output:'export': the
    // page's whole body is a pending-boundary marker plus its inline
    // bootstrap script. Neither is content a crawler or a no-JS reader
    // ever sees, so neither may count toward the frame.
    const f = staticFrame(
      page(
        '<!--$?--><template id="B:0"></template><!--/$-->' +
          '<script>self.__next_f.push([1,"x"])</script>',
      ),
    );
    expect(f).toEqual({ h1Count: 0, text: '' });
  });

  it('ignores chrome outside <main>, and reports a page with no <main>', () => {
    expect(staticFrame(page('<h1>Assets</h1>'))?.text).toBe('Assets');
    expect(staticFrame('<html><body><h1>Embed</h1></body></html>')).toBeNull();
  });
});

describe('routesFromLiteral', () => {
  const source = `
const NAV: NavGroup[] = [
  {
    title: 'Stellar',
    items: [
      { href: '/ledgers', label: 'Ledgers', icon: Layers },
      { href: '/external/assets', label: 'Assets', icon: Globe },
      { href: '/ledgers', label: 'Ledgers again', icon: Layers },
      { href: 'https://docs.stellarindex.io', label: 'API Docs', external: true },
    ],
  },
];

const ACCOUNT_GROUP: NavGroup = {
  items: [{ href: '/dashboard', label: 'Dashboard', icon: LayoutDashboard }],
};
`;

  it("returns the rail's own site-relative routes, skipping external links", () => {
    expect(routesFromLiteral(source, 'NAV')).toEqual([
      '/ledgers',
      '/external/assets',
    ]);
  });

  it('stops at the named literal — the signed-in groups are not the rail', () => {
    expect(routesFromLiteral(source, 'NAV')).not.toContain('/dashboard');
  });

  it('returns nothing when the literal it parses is gone', () => {
    // The caller asserts a floor on the route count precisely because
    // this is how a renamed constant would otherwise pass vacuously.
    expect(routesFromLiteral('const RAIL = [];', 'NAV')).toEqual([]);
    expect(routesFromLiteral(source, 'DEEPER')).toEqual([]);
  });
});

describe('exportedHtmlPath', () => {
  it('maps a route onto the file the static export emits for it', () => {
    expect(exportedHtmlPath('/transactions')).toBe('transactions/index.html');
    expect(exportedHtmlPath('/external/assets')).toBe(
      'external/assets/index.html',
    );
  });
});
