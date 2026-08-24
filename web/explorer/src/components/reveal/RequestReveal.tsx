'use client';

import { Check, Code2, Copy, ExternalLink, X } from 'lucide-react';
import { useCallback, useState } from 'react';
import { twMerge } from 'tailwind-merge';

import type { RequestExample } from '@/api/client';
import { useDialog } from '@/lib/useDialog';
import { useCopyToClipboard } from '@/components/ui';

export type RequestRevealProps = {
  example: RequestExample;
  /** Position the trigger button — top-right is the canonical place. */
  position?: 'top-right' | 'inline';
  className?: string;
};

/**
 * `<>` reveal — every panel exposes its underlying API request.
 *
 * Per data-inventory §3 + §6.10 every panel on the showcase must
 * carry one of these. Click → tray slides in showing the cURL form
 * + a copy button + a link to the live URL. `Cmd-/` toggles every
 * reveal on the page (handled by the surrounding KeyboardShortcuts
 * component once we add it).
 */
export function RequestReveal({
  example,
  position = 'top-right',
  className,
}: RequestRevealProps) {
  const [open, setOpen] = useState(false);
  // LC-050: Escape + focus-trap + focus move-in/restore for the dialog.
  const close = useCallback(() => setOpen(false), []);
  const dialogRef = useDialog<HTMLDivElement>(open, close);

  const curl = renderCurl(example);

  const trigger = (
    <button
      type="button"
      onClick={() => setOpen(true)}
      className={twMerge(
        'border-line bg-surface text-ink-muted hover:border-brand-500 hover:text-brand-600 inline-flex items-center gap-1 rounded-md border px-1.5 py-0.5 text-[10px] font-medium',
        position === 'top-right' && 'absolute top-2 right-2',
        className,
      )}
      aria-label="Show API request"
      title="Show API request"
    >
      <Code2 className="h-3 w-3" aria-hidden />
    </button>
  );

  return (
    <>
      {trigger}
      {open && (
        <div
          className="fixed inset-0 z-50 flex items-end justify-center bg-black/60 p-4 backdrop-blur-sm sm:items-center"
          onClick={() => setOpen(false)}
        >
          <div
            ref={dialogRef}
            tabIndex={-1}
            role="dialog"
            aria-modal="true"
            aria-labelledby="request-reveal-title"
            className="bg-surface w-full max-w-2xl rounded-lg p-6 shadow-2xl outline-hidden"
            onClick={(e) => e.stopPropagation()}
          >
            <header className="mb-4 flex items-center justify-between">
              <h3 id="request-reveal-title" className="font-medium">
                API request
              </h3>
              <button
                type="button"
                onClick={() => setOpen(false)}
                className="text-ink-faint hover:text-ink-body p-1"
                aria-label="Close"
              >
                <X className="h-4 w-4" />
              </button>
            </header>

            <div className="space-y-4">
              <Block label="cURL">
                <pre className="bg-surface-subtle overflow-x-auto rounded-md p-3 font-mono text-xs leading-relaxed whitespace-pre">
                  {curl}
                </pre>
                <CopyButton text={curl} />
              </Block>

              <Block label="URL">
                <code className="text-ink-body text-xs break-all">
                  {example.url}
                </code>
                <div className="mt-2 flex gap-2">
                  <CopyButton text={example.url} />
                  <a
                    href={example.url}
                    target="_blank"
                    rel="noopener noreferrer"
                    className="border-line text-ink-body hover:border-brand-500 hover:text-brand-600 inline-flex items-center gap-1 rounded-sm border px-2 py-1 text-xs"
                  >
                    <ExternalLink className="h-3 w-3" />
                    Open
                  </a>
                </div>
              </Block>

              <p className="text-ink-muted text-[11px]">
                Anonymous tier — no auth required for this endpoint.
              </p>
            </div>
          </div>
        </div>
      )}
    </>
  );
}

function Block({
  label,
  children,
}: {
  label: string;
  children: React.ReactNode;
}) {
  return (
    <div>
      <div className="text-ink-muted mb-1 text-[11px] font-medium tracking-wider uppercase">
        {label}
      </div>
      {children}
    </div>
  );
}

function CopyButton({ text }: { text: string }) {
  // FEC audit A3-F7: the bare clipboard-write await here was an
  // unhandled promise rejection on insecure contexts / permission denial;
  // the canonical ui hook carries the try/catch + unmount-safe reset.
  const { copied, copy } = useCopyToClipboard(text);
  return (
    <button
      type="button"
      onClick={copy}
      className="border-line text-ink-body hover:border-brand-500 hover:text-brand-600 mt-2 inline-flex items-center gap-1 rounded-sm border px-2 py-1 text-xs"
    >
      {copied ? (
        <>
          <Check className="text-up-strong h-3 w-3" />
          Copied
        </>
      ) : (
        <>
          <Copy className="h-3 w-3" />
          Copy
        </>
      )}
    </button>
  );
}

function renderCurl(example: RequestExample): string {
  const headerArgs = Object.entries(example.headers ?? {})
    .map(([k, v]) => `  -H '${k}: ${v}'`)
    .join(' \\\n');
  return [`curl -fsSL '${example.url}'`, headerArgs]
    .filter(Boolean)
    .join(' \\\n');
}
