'use client';

import { Check, Copy } from 'lucide-react';
import type { ComponentProps } from 'react';
import { useEffect, useState } from 'react';

import { cn } from '@/lib/cn';
import { truncateMiddle } from '@/lib/format';

// Back-compat: re-export the canonical from lib so existing
// '@/components/ui' client-side imports keep working. Server code must
// import from '@/lib/format' directly.
export { truncateMiddle } from '@/lib/format';

/**
 * Mono renders a monospace identifier (address / hash / contract id) with an
 * optional inline copy button. Use `truncate` to shorten long strkeys.
 *
 * When the value is actually shortened, the FULL value goes on the
 * rendered span's `title` so hover reveals it, and the copy button still
 * copies the full value — a truncated identifier must never be the only
 * copy of itself on the page (#356). `title` is omitted when nothing was
 * elided, so an untruncated id doesn't grow a redundant tooltip.
 */
export function Mono({
  value,
  truncate,
  copy = true,
  className,
}: {
  value: string;
  truncate?: boolean | { head: number; tail: number };
  copy?: boolean;
  className?: string;
}) {
  const display = truncate
    ? typeof truncate === 'object'
      ? truncateMiddle(value, truncate.head, truncate.tail)
      : truncateMiddle(value)
    : value;
  return (
    <span
      className={cn(
        'inline-flex items-center gap-1.5 font-mono text-[13px]',
        className,
      )}
    >
      <span className="break-all" title={display === value ? undefined : value}>
        {display}
      </span>
      {copy && <CopyButton value={value} />}
    </span>
  );
}

/**
 * useCopyToClipboard — THE clipboard behavior (FEC audit A3-F7): 5 forks
 * existed and the differences were all defects on one side or the other.
 * This hook keeps the winning behaviors from each: unmount-safe reset
 * timer (CopyValue's effect cleanup — Mono's bare setTimeout fired after
 * unmount), preventDefault+stopPropagation (a copy click inside a row
 * <Link> must not navigate — live on OperationView), and a try/catch
 * (insecure context / permission denial must not be an unhandled
 * rejection). Reset delay 1400ms (majority).
 */
export function useCopyToClipboard(value: string, resetMs = 1400) {
  const [copied, setCopied] = useState(false);
  useEffect(() => {
    if (!copied) return;
    const t = setTimeout(() => setCopied(false), resetMs);
    return () => clearTimeout(t);
  }, [copied, resetMs]);
  const copy = async (e?: {
    preventDefault(): void;
    stopPropagation(): void;
  }) => {
    e?.preventDefault();
    e?.stopPropagation();
    try {
      await navigator.clipboard.writeText(value);
      setCopied(true);
    } catch {
      /* clipboard unavailable — no-op */
    }
  };
  return { copied, copy };
}

export function CopyButton({
  value,
  className,
  ...props
}: { value: string } & Omit<
  ComponentProps<'button'>,
  'onClick' | 'type' | 'value'
>) {
  const { copied, copy } = useCopyToClipboard(value);
  return (
    <button
      type="button"
      aria-label="Copy to clipboard"
      onClick={copy}
      className={cn(
        'text-ink-faint hover:bg-surface-subtle hover:text-ink-body inline-flex h-5 w-5 shrink-0 items-center justify-center rounded-sm transition-colors',
        className,
      )}
      {...props}
    >
      {copied ? (
        <Check className="text-up h-3.5 w-3.5" />
      ) : (
        <Copy className="h-3.5 w-3.5" />
      )}
    </button>
  );
}
