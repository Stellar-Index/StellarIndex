// CrossReference — one quiet "External references" line at the foot of an
// entity page, never mid-content. Each target is network-aware; a target with
// no explorer (or no verified route) for this entity on this network is
// omitted rather than linked wrongly.
import { cn } from '@/lib/cn';
import {
  stellarChainEntityUrl,
  stellarExpertUrl,
  steexpEntityUrl,
} from '@/lib/networks';

/** Our entity kind → each explorer's own path segment (null = no route). */
const KINDS = {
  tx: { expert: 'tx', chain: 'transactions', steexp: 'tx' },
  account: { expert: 'account', chain: 'accounts', steexp: 'account' },
  contract: { expert: 'contract', chain: 'contracts', steexp: 'contract' },
  asset: { expert: 'asset', chain: null, steexp: 'asset' },
  ledger: { expert: 'ledger', chain: null, steexp: 'ledger' },
} as const;

export function CrossReference({
  kind,
  id,
  className,
}: {
  kind: keyof typeof KINDS;
  id: string;
  className?: string;
}) {
  const map = KINDS[kind];
  const links = [
    { label: 'stellar.expert', href: stellarExpertUrl(map.expert, id) },
    {
      label: 'stellarchain.io',
      href: map.chain ? stellarChainEntityUrl(map.chain, id) : null,
    },
    { label: 'steexp', href: steexpEntityUrl(map.steexp, id) },
  ].filter((l): l is { label: string; href: string } => l.href != null);

  return (
    <p
      className={cn(
        'border-line text-ink-faint flex flex-wrap gap-x-4 gap-y-1 border-t pt-4 text-xs',
        className,
      )}
    >
      <span>External references</span>
      {links.map((l) => (
        <a
          key={l.label}
          href={l.href}
          target="_blank"
          rel="noreferrer noopener"
          className="text-ink-muted hover:text-brand-600 hover:underline"
        >
          {l.label}
        </a>
      ))}
    </p>
  );
}
