import { describe, it, expect, vi, beforeEach } from 'vitest';
import { render, screen, waitFor } from '@testing-library/react';

vi.mock('@/api/client', async () => {
  const actual =
    await vi.importActual<typeof import('@/api/client')>('@/api/client');
  return { ...actual, apiGet: vi.fn() };
});

import { apiGet } from '@/api/client';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { AssetScamCallout, LiveAssetScamCallout } from './AssetScamCallout';

// The shared scam banner. Its behaviour is pinned here rather than only
// through the two pages that mount it, so the warning's own rules — which
// flags trigger it, and that a clean asset stays quiet — survive a
// refactor of either page.

type Live =
  | { kind: 'pending' }
  | { kind: 'error' }
  | { kind: 'ok'; envelope: Record<string, unknown> };

// Live data may add a warning but never clear a baked one: the payload has
// no explicit "checked, not flagged" answer (a failed directory lookup also
// omits the tags, and its degraded mark is not on the JSON wire).
const CASES: Array<{
  name: string;
  baked: string[];
  live: Live;
  warns: boolean;
}> = [
  {
    name: 'live pending',
    baked: ['malicious'],
    live: { kind: 'pending' },
    warns: true,
  },
  {
    name: 'live failed',
    baked: ['malicious'],
    live: { kind: 'error' },
    warns: true,
  },
  {
    name: 'live present with no tags',
    baked: ['malicious'],
    live: { kind: 'ok', envelope: { data: {} } },
    warns: true,
  },
  {
    name: 'live stale with no tags',
    baked: ['malicious'],
    live: { kind: 'ok', envelope: { data: {}, flags: { stale: true } } },
    warns: true,
  },
  {
    name: 'live present with explicitly empty tags',
    baked: ['malicious'],
    live: { kind: 'ok', envelope: { data: { issuer_directory_tags: [] } } },
    warns: true,
  },
  {
    name: 'live flags what the build did not',
    baked: [],
    live: {
      kind: 'ok',
      envelope: { data: { issuer_directory_tags: ['unsafe'] } },
    },
    warns: true,
  },
  {
    name: 'stale live read still adds a flag',
    baked: [],
    live: {
      kind: 'ok',
      envelope: {
        data: { issuer_directory_tags: ['unsafe'] },
        flags: { stale: true },
      },
    },
    warns: true,
  },
  {
    name: 'nothing anywhere',
    baked: [],
    live: { kind: 'ok', envelope: { data: {} } },
    warns: false,
  },
];

describe('LiveAssetScamCallout resolution', () => {
  beforeEach(() => {
    vi.mocked(apiGet).mockReset();
  });

  it.each(CASES)('$name', async ({ baked, live, warns }) => {
    const client = new QueryClient({
      defaultOptions: { queries: { retry: false } },
    });
    if (live.kind === 'pending') {
      vi.mocked(apiGet).mockImplementation(() => new Promise(() => {}));
    } else if (live.kind === 'error') {
      vi.mocked(apiGet).mockRejectedValue(new Error('503'));
    } else {
      vi.mocked(apiGet).mockResolvedValue(live.envelope as never);
    }
    const { container } = render(
      <QueryClientProvider client={client}>
        <LiveAssetScamCallout assetID="X-G" directoryTags={baked} />
      </QueryClientProvider>,
    );
    if (live.kind !== 'pending') {
      await waitFor(() =>
        expect(
          client.getQueryState(['/v1/assets/{id}', 'X-G'])?.status,
        ).not.toBe('pending'),
      );
    }
    if (warns) {
      expect(await screen.findByRole('alert')).toBeTruthy();
    } else {
      await waitFor(() => expect(container.firstChild).toBeNull());
    }
  });
});

describe('LiveAssetScamCallout', () => {
  beforeEach(() => {
    vi.mocked(apiGet).mockImplementation(() => new Promise(() => {}));
  });

  it('warns from the live read when the baked page carried no flag', async () => {
    const client = new QueryClient({
      defaultOptions: { queries: { retry: false } },
    });
    client.setQueryData(['/v1/assets/{id}', 'EVIL-G'], {
      data: { issuer_directory_tags: ['malicious'] },
    });
    render(
      <QueryClientProvider client={client}>
        <LiveAssetScamCallout assetID="EVIL-G" directoryTags={[]} />
      </QueryClientProvider>,
    );
    expect(
      await screen.findByText('Flagged by community directory'),
    ).toBeInTheDocument();
  });
});

describe('AssetScamCallout', () => {
  it('warns when the issuer carries a scam-class directory tag', () => {
    render(<AssetScamCallout directoryTags={['unsafe']} />);
    expect(screen.getByRole('alert')).toBeTruthy();
    expect(screen.getByText(/Flagged by community directory/i)).toBeTruthy();
    expect(screen.getByText(/Do not trust this asset/i)).toBeTruthy();
  });

  it('leads with the curated reason when there is one', () => {
    render(
      <AssetScamCallout
        directoryTags={['malicious']}
        scamReason="Drains trustlines"
        directoryDomain="evil.example"
      />,
    );
    expect(screen.getByText(/Known scam asset/i)).toBeTruthy();
    expect(screen.getByText(/Drains trustlines/)).toBeTruthy();
    expect(screen.getByText(/evil\.example/)).toBeTruthy();
  });

  it('renders nothing for benign tags, so callers can mount it unconditionally', () => {
    // 'anchor' and 'issuer' are ordinary directory labels. Treating any
    // tag as a scam flag would put a "do not trust this asset" banner on
    // legitimate anchors — the more damaging failure mode.
    const { container } = render(
      <AssetScamCallout directoryTags={['anchor', 'issuer']} />,
    );
    expect(container.firstChild).toBeNull();
  });

  it('renders nothing when the asset carries no flags at all', () => {
    const { container } = render(<AssetScamCallout directoryTags={null} />);
    expect(container.firstChild).toBeNull();
  });
});
