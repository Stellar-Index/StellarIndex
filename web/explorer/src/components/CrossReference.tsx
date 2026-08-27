// CrossReference — one short line pointing at the other Stellar explorers.
//
// Replaces the scatter of inline "view on stellar.expert" links that used to
// sit mid-content on asset, account, contract and issuer pages. Those read as
// though StellarIndex were deferring to a third party for the data it was
// itself displaying; a single, quiet line at the foot of the page is the
// honest amount of deference — the reader can cross-check, without being
// pushed off the page mid-sentence.
//
// Network-aware on both targets. stellar.expert has no futurenet explorer, so
// on futurenet only stellarchain.io is offered rather than a link that would
// silently land the reader on the wrong chain.
import { stellarChainEntityUrl, stellarExpertUrl } from '@/lib/networks';

/** Our entity kind → each explorer's own path segment. */
const KINDS = {
  tx: { expert: 'tx', chain: 'transactions' },
  account: { expert: 'account', chain: 'accounts' },
  contract: { expert: 'contract', chain: 'contracts' },
  asset: { expert: 'asset', chain: null },
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
  const expert = stellarExpertUrl(map.expert, id);
  const chain = map.chain ? stellarChainEntityUrl(map.chain, id) : null;
  if (!expert && !chain) return null;

  return (
    <p className={className ?? 'text-ink-faint mt-4 text-[11px]'}>
      Cross-reference on{' '}
      {expert && (
        <a
          href={expert}
          target="_blank"
          rel="noreferrer noopener"
          className="underline hover:text-brand-600"
        >
          stellar.expert
        </a>
      )}
      {expert && chain ? ' · ' : null}
      {chain && (
        <a
          href={chain}
          target="_blank"
          rel="noreferrer noopener"
          className="underline hover:text-brand-600"
        >
          stellarchain.io
        </a>
      )}
      .
    </p>
  );
}
