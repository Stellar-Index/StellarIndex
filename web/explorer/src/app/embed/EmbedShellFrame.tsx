import type { ReactNode } from 'react';

import { CURRENT_NETWORK } from '@/lib/networks';

/**
 * EmbedShellFrame — the chrome shared by the /embed/* runtime shells
 * (T291): the same label row, explorer link and attribution the baked
 * widgets render, around whatever live body the shell fetched.
 */
export function EmbedShellFrame({
  label,
  sublabel,
  href,
  children,
}: {
  label: string;
  sublabel: string;
  href: string;
  children: ReactNode;
}) {
  return (
    <div className="bg-surface text-ink flex h-full min-h-32 flex-col gap-2 px-4 py-3">
      <div className="flex items-baseline justify-between gap-2">
        <div className="flex items-baseline gap-2">
          <span className="text-base font-semibold tracking-tight">
            {label}
          </span>
          <span className="text-ink-muted font-mono text-[10px]">
            {sublabel}
          </span>
        </div>
        <a
          href={`${CURRENT_NETWORK.explorerUrl}${href}`}
          target="_blank"
          rel="noreferrer noopener"
          className="text-ink-faint hover:text-brand-600 text-[10px]"
        >
          stellarindex.io ↗
        </a>
      </div>
      <div className="flex flex-wrap items-baseline gap-x-2 gap-y-1">
        {children}
      </div>
      <div className="text-ink-faint mt-auto text-[10px]">
        Powered by Stellar Index
      </div>
    </div>
  );
}

/** EmbedShellMessage — the centred one-liner for a shell with no data. */
export function EmbedShellMessage({ children }: { children: ReactNode }) {
  return (
    <div className="text-ink-muted flex h-full min-h-32 items-center justify-center px-3 py-3 text-sm">
      <span>{children}</span>
    </div>
  );
}
