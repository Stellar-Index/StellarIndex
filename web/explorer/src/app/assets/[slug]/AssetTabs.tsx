'use client';

import { useSearchParams } from 'next/navigation';

import { TabNav } from '@/components/ui';

export type AssetTab =
  | 'overview'
  | 'chart'
  | 'markets'
  | 'history'
  | 'supply'
  | 'holders'
  | 'liquidity'
  | 'oracles';

/**
 * Client tab strip for /assets/[slug]. Reads `?tab=` from URL state;
 * the parent server component renders all tab bodies and toggles
 * visibility based on the active tab.
 */
export function AssetTabs({ slug }: { slug: string }) {
  const params = useSearchParams();
  // Chart is the default (the bare /assets/{slug} URL) — the page leads
  // with price action, with the dense stats always present in the rail.
  const active = (params.get('tab') as AssetTab) || 'chart';

  type T = { key: AssetTab; label: string };
  const tabs: T[] = [
    { key: 'chart', label: 'Chart' },
    { key: 'markets', label: 'Markets' },
    { key: 'history', label: 'Trades' },
    { key: 'holders', label: 'Holders' },
    { key: 'liquidity', label: 'Liquidity' },
    { key: 'supply', label: 'Supply' },
    { key: 'oracles', label: 'Oracles' },
    { key: 'overview', label: 'About' },
  ];

  const hrefFor = (key: AssetTab) =>
    key === 'chart' ? `/assets/${slug}` : `/assets/${slug}?tab=${key}`;

  return (
    <TabNav
      items={tabs.map((t) => ({ label: t.label, href: hrefFor(t.key) }))}
      activeHref={hrefFor(active)}
    />
  );
}

export function ActiveTabSlot({
  overview,
  chart,
  markets,
  history,
  supply,
  holders,
  liquidity,
  oracles,
}: {
  overview: React.ReactNode;
  chart: React.ReactNode;
  markets?: React.ReactNode;
  history?: React.ReactNode;
  supply?: React.ReactNode;
  holders?: React.ReactNode;
  liquidity?: React.ReactNode;
  oracles?: React.ReactNode;
}) {
  const params = useSearchParams();
  const tab = (params.get('tab') as AssetTab) || 'chart';
  if (tab === 'overview') return <>{overview}</>;
  if (tab === 'markets' && markets) return <>{markets}</>;
  if (tab === 'history' && history) return <>{history}</>;
  if (tab === 'supply' && supply) return <>{supply}</>;
  if (tab === 'holders' && holders) return <>{holders}</>;
  if (tab === 'liquidity' && liquidity) return <>{liquidity}</>;
  if (tab === 'oracles' && oracles) return <>{oracles}</>;
  return <>{chart}</>;
}
