import type { ComponentProps, ReactNode } from 'react';

import { cn } from '@/lib/cn';

/** EmptyState — centered icon + title + description for no-data surfaces. */
export function EmptyState({
  icon,
  title,
  description,
  action,
  headingLevel = 3,
  className,
}: {
  icon?: ReactNode;
  title: ReactNode;
  description?: ReactNode;
  action?: ReactNode;
  /**
   * Heading rank for `title`. Defaults to 3, which is right for an empty
   * state inside a card/panel (itself an `<h3>` under a SectionHeader
   * `<h2>`). An empty state that IS a page's top-level section must pass
   * 2, or the outline skips h2 and `heading-order` (WCAG 1.3.1) fails —
   * the visual style is unchanged either way, since it lives in the
   * className, not the tag. (#486)
   */
  headingLevel?: 2 | 3 | 4;
  className?: string;
}) {
  const H = `h${headingLevel}` as const;
  return (
    <div
      className={cn(
        'rounded-card border-line-strong bg-surface-muted flex flex-col items-center justify-center border border-dashed px-6 py-12 text-center',
        className,
      )}
    >
      {icon && (
        <div className="bg-surface text-ink-faint ring-line mb-3 flex h-11 w-11 items-center justify-center rounded-full ring-1">
          {icon}
        </div>
      )}
      <H className="text-ink text-sm font-semibold">{title}</H>
      {description && (
        <p className="text-ink-muted mt-1 max-w-sm text-sm">{description}</p>
      )}
      {action && <div className="mt-4">{action}</div>}
    </div>
  );
}

/** Skeleton — a pulsing placeholder block for loading states. */
export function Skeleton({ className, ...props }: ComponentProps<'div'>) {
  return (
    <div
      className={cn('bg-surface-subtle animate-pulse rounded-md', className)}
      {...props}
    />
  );
}

/** Inline alert / callout for info, warnings, and errors. */
export function Callout({
  tone = 'info',
  title,
  children,
  className,
}: {
  tone?: 'info' | 'warn' | 'bad' | 'ok';
  title?: ReactNode;
  children?: ReactNode;
  className?: string;
}) {
  const tones = {
    info: 'border-brand-200 bg-brand-50 text-brand-900',
    warn: 'border-warn-300 bg-warn-50 text-warn-900',
    bad: 'border-bad-300 bg-bad-50 text-bad-900',
    ok: 'border-ok-300 bg-ok-50 text-ok-700',
  }[tone];
  // LC-052: announce to assistive tech. bad/warn are errors → assertive
  // role=alert (interrupts); ok/info are status → polite. Callouts that
  // render dynamically after a form submit (sign-in, key create/revoke) are
  // now spoken instead of silently appearing.
  const urgent = tone === 'bad' || tone === 'warn';
  return (
    <div
      role={urgent ? 'alert' : 'status'}
      aria-live={urgent ? 'assertive' : 'polite'}
      className={cn('rounded-lg border px-4 py-3 text-sm', tones, className)}
    >
      {title && <div className="mb-0.5 font-semibold">{title}</div>}
      {children}
    </div>
  );
}
