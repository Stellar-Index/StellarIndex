// StellarExpertLink — an outbound stellar.expert link for THIS network.
//
// Every stellar.expert link in the explorer used to be hardcoded to
// `/explorer/public`, i.e. MAINNET. On the test nets that sent the reader to
// the wrong chain: at best a "not found", at worst an unrelated mainnet
// account/contract that happens to share the id, presented as if it were the
// entity they were looking at. Same class as the /network page reporting
// "Pubnet" on testnet.
//
// stellar.expert has no futurenet explorer, so there is no correct URL to
// emit there. In that case this renders the children as plain text rather
// than a dead or misdirected link — an absent link is honest; a wrong one
// is not.
import type { ReactNode } from 'react';

import { stellarExpertUrl } from '@/lib/networks';

export function StellarExpertLink({
  kind,
  id,
  className,
  title,
  children,
}: {
  /** stellar.expert's own path segment: 'tx' | 'account' | 'contract' | 'asset'. */
  kind: 'tx' | 'account' | 'contract' | 'asset';
  id: string;
  className?: string;
  title?: string;
  children: ReactNode;
}) {
  const href = stellarExpertUrl(kind, id);
  if (!href) return <span className={className}>{children}</span>;
  return (
    <a
      href={href}
      target="_blank"
      rel="noreferrer noopener"
      className={className}
      title={title}
    >
      {children}
    </a>
  );
}
