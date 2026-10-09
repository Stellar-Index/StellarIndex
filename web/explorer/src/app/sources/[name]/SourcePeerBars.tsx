'use client';

import { Panel } from '@/components/reveal';
import { asExample } from '@/api/client';
import { HBarList } from '@/components/charts/Bars';
import { useSources, type Source } from '@/api/hooks';
import { formatCompact } from '@/lib/format';

const MAX_PEERS = 8;

/** Rows for the peer chart: same-class sources by 24h trades, this one always included. */
export function peerRows(name: string, all: Source[]) {
  const self = all.find((s) => s.name === name);
  if (!self) return [];
  const ranked = all
    .filter(
      (s) => s.class === self.class && typeof s.trade_count_24h === 'number',
    )
    .sort((a, b) => (b.trade_count_24h ?? 0) - (a.trade_count_24h ?? 0));
  const top = ranked.slice(0, MAX_PEERS);
  if (
    !top.some((s) => s.name === name) &&
    typeof self.trade_count_24h === 'number'
  ) {
    top.push(self);
  }
  return top.map((s) => ({
    label: s.name,
    value: s.trade_count_24h ?? 0,
    display: formatCompact(s.trade_count_24h ?? 0),
    color:
      s.name === name ? 'var(--color-brand-500)' : 'var(--color-ink-faint)',
  }));
}

export function SourcePeerBars({ source }: { source: string }) {
  const { data } = useSources(undefined, true);
  const items = peerRows(source, data ?? []);
  if (items.length < 2) return null;
  return (
    <Panel
      headingLevel={2}
      title="Versus peers"
      hint="24h trades, same class"
      source={asExample('/v1/sources', { include: 'stats' })}
    >
      <HBarList
        items={items}
        ariaLabel={`24h trades for ${source} versus same-class sources`}
      />
    </Panel>
  );
}
