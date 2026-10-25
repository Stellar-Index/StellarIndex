import { afterEach, describe, expect, it, vi } from 'vitest';
import { act, cleanup, render } from '@testing-library/react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';

import { API_BASE_URL } from '@/api/client';
import AccountsPage from '@/app/accounts/page';
import { AccountView } from '@/app/accounts/AccountView';
import { AssetsTable } from '@/app/assets/AssetsTable';
import ContractsPage from '@/app/contracts/page';
import LedgersPage from '@/app/ledgers/page';
import LendingPage from '@/app/lending/page';
import HomePage from '@/app/page';
import ProtocolsPage from '@/app/protocols/page';
import TransactionsPage from '@/app/transactions/page';
import { MarketsTable } from '@/app/markets/MarketsTable';
import { ConsoleShell } from '@/components/nav/ConsoleShell';

import { PRELOAD_GLOBAL, PRELOAD_ROUTES, preloadScript } from './manifest';

const G = 'GAQ7XD7LJIRBLW5KTB7TDBDZ4MPYS2OHPELHX7NNP3MVOL22YBA7EPGF';
const ORIGIN = new URL(API_BASE_URL).origin;

// Every fetch stays pending, so what is captured is exactly the first wave: the
// calls a page makes before any response arrives.
async function firstWave(ui: React.ReactNode, now: number): Promise<string[]> {
  vi.setSystemTime(now);
  const seen: string[] = [];
  vi.stubGlobal(
    'fetch',
    vi.fn((input: RequestInfo | URL) => {
      seen.push(String(input instanceof Request ? input.url : input));
      return new Promise(() => {});
    }),
  );
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  render(<QueryClientProvider client={client}>{ui}</QueryClientProvider>);
  for (let i = 0; i < 5; i++)
    await act(async () => {
      await new Promise((r) => setTimeout(r, 0));
    });
  cleanup();
  client.clear();
  vi.unstubAllGlobals();
  return seen.map((u) => u.replace(ORIGIN, '')).sort();
}

// Two clocks an hour apart: a URL carrying the time (a `from` window) changes
// between them and must not be preloaded.
async function stableFirstWave(ui: React.ReactNode): Promise<string[]> {
  const a = await firstWave(ui, Date.UTC(2026, 9, 23, 12));
  const b = await firstWave(ui, Date.UTC(2026, 9, 23, 13));
  expect(a).toEqual(b);
  return a;
}

function routeUrls(path: string, id = ''): string[] {
  const r = PRELOAD_ROUTES.find((x) => x.path.test(path));
  if (!r) throw new Error(`no preload route for ${path}`);
  return r.urls.map((u) => u.split('{id}').join(id)).sort();
}

afterEach(() => {
  vi.useRealTimers();
  document.head
    .querySelectorAll('link[rel="preload"]')
    .forEach((l) => l.remove());
  window.history.replaceState(null, '', '/');
});

describe('preload lists match what each page fetches first', () => {
  it('console shell', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    expect(
      await stableFirstWave(
        <ConsoleShell>
          <div />
        </ConsoleShell>,
      ),
    ).toEqual([...PRELOAD_GLOBAL].sort());
  });

  it('/markets/', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    expect(await stableFirstWave(<MarketsTable />)).toEqual(
      routeUrls('/markets/'),
    );
  });

  // A page may also fetch a shell global; the shell's preload covers it.
  it.each<[string, () => React.ReactNode]>([
    ['/', () => <HomePage />],
    ['/lending/', () => <LendingPage />],
    ['/assets/', () => <AssetsTable verifiedSlugs={[]} />],
    ['/contracts/', () => <ContractsPage />],
    ['/ledgers/', () => <LedgersPage />],
    ['/transactions/', () => <TransactionsPage />],
    ['/protocols/', () => <ProtocolsPage />],
    ['/accounts/', () => <AccountsPage />],
  ])('%s', async (path, ui) => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    const wave = await stableFirstWave(ui());
    expect(wave.filter((u) => !PRELOAD_GLOBAL.includes(u))).toEqual(
      routeUrls(path),
    );
  });

  it('/accounts/{G}', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    expect(await stableFirstWave(<AccountView id={G} />)).toEqual(
      routeUrls(`/accounts/${G}`, G),
    );
  });
});

function preloadsAt(url: string): string[] {
  window.history.replaceState(null, '', url);
  new Function(preloadScript(API_BASE_URL))();
  return [
    ...document.head.querySelectorAll<HTMLLinkElement>('link[rel="preload"]'),
  ]
    .map((l) => {
      expect(l.as).toBe('fetch');
      expect(l.crossOrigin).toBe('anonymous');
      return l.href.replace(ORIGIN, '');
    })
    .sort();
}

describe('preloadScript', () => {
  it('cannot close its own script tag', () => {
    const js = preloadScript(API_BASE_URL);
    expect(js).not.toMatch(/<\/|<!--/);
  });

  it('preloads the globals and the account list for a valid account', () => {
    expect(preloadsAt(`/accounts/${G}`)).toEqual(
      [...PRELOAD_GLOBAL, ...routeUrls(`/accounts/${G}`, G)].sort(),
    );
  });

  it('preloads only the globals for an invalid account id', () => {
    expect(preloadsAt('/accounts/not-an-account')).toEqual(
      [...PRELOAD_GLOBAL].sort(),
    );
  });

  it('preloads no route list when the URL carries a query', () => {
    expect(preloadsAt('/markets/?sort=price')).toEqual(
      [...PRELOAD_GLOBAL].sort(),
    );
  });

  it('preloads nothing outside the console shell', () => {
    expect(preloadsAt('/embed/price/native')).toEqual([]);
  });

  it('preloads the globals on a page with no route list', () => {
    expect(preloadsAt('/ledgers/123/')).toEqual([...PRELOAD_GLOBAL].sort());
  });

  it('matches the home list on / only', () => {
    expect(preloadsAt('/')).toEqual(
      [...PRELOAD_GLOBAL, ...routeUrls('/')].sort(),
    );
  });
});
