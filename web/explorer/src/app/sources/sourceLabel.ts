import { DEX_INFO } from '@/app/dexes/registry';
import { CEX_INFO } from '@/app/exchanges/registry';
import { protocolMeta } from '@/app/protocols/registry';

/** Display name for a registry source id; the id itself when none is curated. */
export function sourceLabel(name: string): string {
  return (
    DEX_INFO[name]?.name ??
    CEX_INFO[name]?.name ??
    protocolMeta(name)?.label ??
    name
  );
}

/** The counted unit on a source's 24h strip: exchange pairs, on-chain pools. */
export function sourceUnitsLabel(
  cls: string,
  subclass?: string | null,
): 'pairs' | 'pools' {
  return cls === 'exchange' && subclass !== 'dex' ? 'pairs' : 'pools';
}
