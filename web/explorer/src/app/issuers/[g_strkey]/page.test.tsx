import { describe, it, expect, vi } from 'vitest';
import { render, screen } from '@testing-library/react';

vi.mock('@/lib/buildFetch', async () => {
  const actual =
    await vi.importActual<typeof import('@/lib/buildFetch')>(
      '@/lib/buildFetch',
    );
  return { ...actual, buildFetchData: vi.fn(), failBuild: vi.fn() };
});

import { buildFetchData } from '@/lib/buildFetch';
import IssuerDetailPage, { generateMetadata } from './page';
import { CURRENT_NETWORK } from '@/lib/networks';

const G = 'GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN';

function mockIssuer(detail: Record<string, unknown>) {
  vi.mocked(buildFetchData).mockImplementation(async (path: string) => {
    if (path.startsWith('/v1/issuers/')) return detail as never;
    return [] as never; // /v1/assets?issuer= price join
  });
}

async function renderPage() {
  render(await IssuerDetailPage({ params: Promise.resolve({ g_strkey: G }) }));
}
/** The value under a Stat label (label span → label row → value div). */
function stat(label: string) {
  return screen.getByText(label, { selector: 'span' }).closest('div')
    ?.nextElementSibling?.textContent;
}

// Frontend-honesty sweep: /v1/issuers/{g} soft-fails its per-asset
// fan-out on error OR deadline (internal/api/v1/issuers.go), so `assets`
// arrives absent. `?? 0` baked "Assets 0" / "Total observations 0" /
// "Issued assets (0)" into the static export — three headline claims
// that the issuer has issued nothing and never traded.
describe('IssuerDetailPage issued-asset tiles', () => {
  it('renders — for the asset tiles when `assets` is absent', async () => {
    mockIssuer({ g_strkey: G, home_domain: 'circle.com' });
    await renderPage();

    expect(stat('Assets issued')).toBe('—');
    expect(stat('Observations')).toBe('—');
    expect(screen.getByText('Not read yet')).toBeInTheDocument();
    // Panel heading carries no fabricated count, and the body abstains.
    expect(screen.getByText('Issued assets')).toBeInTheDocument();
    expect(
      screen.getByText(/Issued-asset list unavailable/),
    ).toBeInTheDocument();
    expect(
      screen.queryByText(/No issued assets observed/),
    ).not.toBeInTheDocument();
  });

  it('renders real counts (including a genuine 0) when `assets` is served', async () => {
    mockIssuer({ g_strkey: G, home_domain: 'circle.com', assets: [] });
    await renderPage();

    expect(stat('Assets issued')).toBe('0');
    expect(stat('Observations')).toBe('0');
    expect(screen.getByText('Issued assets (0)')).toBeInTheDocument();
    expect(screen.getByText(/No issued assets observed/)).toBeInTheDocument();
    expect(
      screen.queryByText(/Issued-asset list unavailable/),
    ).not.toBeInTheDocument();
  });

  it('sums observations across the served assets', async () => {
    mockIssuer({
      g_strkey: G,
      home_domain: 'circle.com',
      assets: [
        {
          asset_id: `USDC-${G}`,
          code: 'USDC',
          slug: 'usdc',
          first_seen_ledger: 100,
          last_seen_ledger: 200,
          observation_count: 40,
        },
        {
          asset_id: `EURC-${G}`,
          code: 'EURC',
          slug: 'eurc',
          first_seen_ledger: 100,
          last_seen_ledger: 200,
          observation_count: 2,
        },
      ],
    });
    await renderPage();

    expect(stat('Assets issued')).toBe('2');
    expect(stat('Observations')).toBe('42');
    expect(screen.getByText('Issued assets (2)')).toBeInTheDocument();
  });
});

// Money: the 24h volume tile sums decimal strings exactly. Float addition
// reaches 999.995 here and rounds to "$1K"; the exact total is 999.99499….
describe('IssuerDetailPage 24h volume tile', () => {
  const asset = (code: string) => ({
    asset_id: `${code}-${G}`,
    code,
    slug: code.toLowerCase(),
    first_seen_ledger: 100,
    last_seen_ledger: 200,
    observation_count: 1,
  });

  function mockWithVolumes(volumes: string[]) {
    const codes = volumes.map((_, i) => `A${i}`);
    vi.mocked(buildFetchData).mockImplementation(async (path: string) => {
      if (path.startsWith('/v1/issuers/'))
        return { g_strkey: G, assets: codes.map(asset) } as never;
      return codes.map((c, i) => ({
        asset_id: `${c}-${G}`,
        volume_24h_usd: volumes[i],
      })) as never;
    });
  }

  it.runIf(CURRENT_NETWORK.pricing)('sums volumes exactly', async () => {
    mockWithVolumes(['999.994999999999998', '0.000000000000001']);
    await renderPage();
    expect(stat('24h volume')).toBe('$999.99');
  });

  it.runIf(CURRENT_NETWORK.pricing).each([
    ['not a decimal', 'NaN'],
    ['negative', '-6000'],
  ])('abstains when one row is %s', async (_, bad) => {
    mockWithVolumes(['5000', bad]);
    await renderPage();
    expect(stat('24h volume')).toBe('—');
  });
});

// F086: functions/issuers/[[path]].js serves THIS route's baked static
// HTML verbatim for every issuer beyond the pre-rendered top-100 (a 404
// sub-fetches /issuers/shell/ and returns its body at 200). Whatever
// generateMetadata bakes for g_strkey === 'shell' is therefore what every
// long-tail issuer page declares to a crawler — it must be a generic
// noindex shell, never the real per-issuer metadata built from the
// literal string 'shell'.
// The organisation block is issuer-authored: only a plain https URL to a
// public host and a bare handle are ever linked.
describe('IssuerDetailPage organisation panel', () => {
  const sep1 = (doc: Record<string, string>) => ({
    g_strkey: G,
    org_name: 'Centre',
    assets: [],
    sep1_payload: { Documentation: doc },
  });

  it('links a plain https website and a bare GitHub handle', async () => {
    mockIssuer(
      sep1({ ORG_URL: 'https://www.centre.io', ORG_GITHUB: 'centrehq' }),
    );
    await renderPage();
    expect(
      screen.getByText('https://www.centre.io').closest('a'),
    ).toHaveAttribute('href', 'https://www.centre.io/');
    expect(screen.getByText('centrehq').closest('a')).toHaveAttribute(
      'href',
      'https://github.com/centrehq',
    );
  });

  it.each([
    ['http', 'http://www.centre.io'],
    ['javascript', 'javascript:alert(1)'],
    ['credentials', 'https://user:pw@www.centre.io'],
  ])('renders a %s website as text, never a link', async (_, url) => {
    mockIssuer(sep1({ ORG_URL: url, ORG_GITHUB: 'centre/hq' }));
    await renderPage();
    expect(screen.getByText(url).closest('a')).toBeNull();
    expect(screen.getByText('centre/hq').closest('a')).toBeNull();
  });
});

describe('IssuerDetailPage generateMetadata', () => {
  it('builds real canonical/og:url metadata for a real issuer, indexable', async () => {
    const meta = await generateMetadata({
      params: Promise.resolve({ g_strkey: G }),
    });
    expect(meta.alternates?.canonical).toBe(
      `${CURRENT_NETWORK.explorerUrl}/issuers/${G}`,
    );
    expect(meta.openGraph?.url).toBe(
      `${CURRENT_NETWORK.explorerUrl}/issuers/${G}`,
    );
    expect(meta.robots).toBeUndefined();
  });

  it('serves generic noindex metadata for the runtime-fallback shell, never a shell-URL canonical', async () => {
    const meta = await generateMetadata({
      params: Promise.resolve({ g_strkey: 'shell' }),
    });
    expect(meta.robots).toEqual({ index: false, follow: true });
    // Root layout bakes `alternates: { canonical: '/' }`; metadata merges
    // shallowly per top-level key, so this shell must override the whole
    // key with `{}` rather than omit it — omitting it inherits the root's
    // canonical verbatim (F097).
    expect(meta).toHaveProperty('alternates');
    expect(meta.alternates).toEqual({});
    expect(meta.alternates?.canonical).toBeUndefined();
    expect(meta.openGraph?.url).toBeUndefined();
    expect(meta.title).not.toMatch(/shell/i);
    // An omitted openGraph key would inherit the homepage og:title.
    expect(meta.openGraph?.title).toContain('Issuer');
    expect(meta.twitter?.title).toContain('Issuer');
  });
});

describe('IssuerDetailPage issued-asset rows', () => {
  const asset = (code: string, observation_count: number) => ({
    asset_id: `${code}:${G}`,
    code,
    observation_count,
  });

  it('leads with the traded assets and folds the never-traded tail away', async () => {
    mockIssuer({
      g_strkey: G,
      auth_revocable: true,
      auth_required: false,
      auth_immutable: false,
      auth_clawback: false,
      assets: [asset('AAA', 0), asset('USDC', 41_000_000), asset('ZZZ', 0)],
    });
    await renderPage();
    expect(screen.getByText('Issued assets (3)')).toBeInTheDocument();
    const summary = screen.getByText(/2 more with no trade or trustline/);
    const details = summary.closest('details') as HTMLDetailsElement;
    expect(details).not.toBeNull();
    expect(details.querySelectorAll('tbody tr')).toHaveLength(2);
    // USDC sits in the open table, not behind the fold.
    const usdc = screen.getByText('USDC').closest('tbody') as HTMLElement;
    expect(details.contains(usdc)).toBe(false);
    // Only the set flag is a chip; the rest read as one faint line.
    expect(screen.getByText('Revocable')).toBeInTheDocument();
    expect(
      screen.getByText('Unset: auth required, immutable, clawback'),
    ).toBeInTheDocument();
  });

  it('shows the whole list when nothing has traded', async () => {
    mockIssuer({
      g_strkey: G,
      auth_required: false,
      auth_revocable: false,
      auth_immutable: false,
      auth_clawback: false,
      assets: [asset('AAA', 0), asset('BBB', 0)],
    });
    await renderPage();
    expect(screen.queryByText(/more with no trade/)).not.toBeInTheDocument();
    expect(screen.getAllByRole('row')).toHaveLength(3); // header + 2
    expect(screen.getByText('None set')).toBeInTheDocument();
  });
});
