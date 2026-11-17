import { describe, it, expect } from 'vitest';
import { render, screen } from '@testing-library/react';

import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { AssetScamCallout, LiveAssetScamCallout } from './AssetScamCallout';

// The shared scam banner. Its behaviour is pinned here rather than only
// through the two pages that mount it, so the warning's own rules — which
// flags trigger it, and that a clean asset stays quiet — survive a
// refactor of either page.
describe('LiveAssetScamCallout', () => {
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
