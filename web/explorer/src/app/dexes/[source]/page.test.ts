import { describe, it, expect } from 'vitest';

import type { ReactElement } from 'react';

import SourceDetailPage, { generateMetadata } from './page';
import { SourceStatsPanel } from './SourceStatsPanel';

async function statsPanelUnits(source: string): Promise<unknown> {
  const page = (await SourceDetailPage({
    params: Promise.resolve({ source }),
  })) as ReactElement<{ children: ReactElement[] }>;
  const panel = page.props.children.find(
    (c) => (c as ReactElement | null)?.type === SourceStatsPanel,
  ) as ReactElement<{ unitsLabel?: string }> | undefined;
  expect(panel).toBeDefined();
  return panel?.props.unitsLabel;
}

describe('/dexes/[source] 24h activity strip', () => {
  it('counts SDEX markets as pairs', async () => {
    expect(await statsPanelUnits('sdex')).toBe('pairs');
  });

  it('counts AMM markets as pools', async () => {
    // Unset falls back to the panel's 'pools' default.
    expect((await statsPanelUnits('soroswap')) ?? 'pools').toBe('pools');
  });
});

describe('/dexes/[source] metadata', () => {
  it('frames SDEX as markets of pairs, not pools', async () => {
    const meta = await generateMetadata({
      params: Promise.resolve({ source: 'sdex' }),
    });
    expect(meta.title).toBe('SDEX — every market, live');
    expect(meta.description).toMatch(/^All SDEX markets observed/);
    expect(meta.description).toContain('per-pair 24h trade count');
    expect(String(meta.description)).not.toMatch(/pool/i);
  });

  it('keeps the pool framing for an AMM', async () => {
    const meta = await generateMetadata({
      params: Promise.resolve({ source: 'soroswap' }),
    });
    expect(meta.title).toBe('Soroswap — every pool, live');
    expect(meta.description).toMatch(/^All Soroswap pools observed/);
  });
});
