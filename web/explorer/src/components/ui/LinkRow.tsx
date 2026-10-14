'use client';

import { useRouter } from 'next/navigation';
import type { ComponentProps, MouseEvent } from 'react';

import { cn } from '@/lib/cn';

type Props = ComponentProps<'tr'> & { href: string | null };

/**
 * A table row that opens `href` from a click anywhere on it. The Link inside
 * the row stays the keyboard control (keyboard-operable.a11y.test.ts). A null
 * href renders a plain row.
 */
export function LinkRow({ href, className, onClick, ...props }: Props) {
  const router = useRouter();
  const open = (e: MouseEvent<HTMLTableRowElement>) => {
    onClick?.(e);
    if (href === null || e.defaultPrevented || e.button !== 0) return;
    // Controls inside the row and a text selection keep their own behaviour.
    const target = e.target as Element;
    if (
      target.closest(
        'a, button, input, select, textarea, label, [role="button"]',
      )
    )
      return;
    if (window.getSelection()?.toString()) return;
    if (e.metaKey || e.ctrlKey || e.shiftKey) {
      window.open(href, '_blank', 'noopener');
      return;
    }
    router.push(href);
  };
  return (
    <tr
      className={cn(
        'hover:bg-surface-muted/70 transition-colors',
        href !== null && 'cursor-pointer',
        className,
      )}
      onClick={open}
      {...props}
    />
  );
}
