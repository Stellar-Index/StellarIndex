'use client';

import { CopyButton } from '@/components/ui';

/**
 * Snippet block with a Copy button. Lifted out of WidgetsPage so
 * the parent can stay a server component (file reads, no client
 * state) while just this island opts into the browser bundle.
 * Copy behavior is the canonical ui CopyButton (FEC audit A3-F7).
 */
export function CopyableSnippet({ snippet }: { snippet: string }) {
  return (
    <div className="relative">
      <pre className="bg-surface-subtle text-ink-body overflow-x-auto px-3 py-2.5 text-[11px] leading-5">
        <code>{snippet}</code>
      </pre>
      <CopyButton
        value={snippet}
        aria-label="Copy snippet"
        className="absolute top-2 right-2"
      />
    </div>
  );
}
