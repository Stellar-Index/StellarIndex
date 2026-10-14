import type { ReactNode } from 'react';

import { cn } from '@/lib/cn';

/** Shows the destination itself as the link text, so an off-site hop is never a surprise. */
export function ExternalRef({
  href,
  className,
  children,
}: {
  href: string;
  className?: string;
  children?: ReactNode;
}) {
  return (
    <a
      href={href}
      target="_blank"
      rel="noreferrer"
      className={cn(
        'text-ink-muted hover:text-brand-600 hover:underline',
        className,
      )}
    >
      {children ?? href.replace(/^https?:\/\/(www\.)?/, '')}
    </a>
  );
}
