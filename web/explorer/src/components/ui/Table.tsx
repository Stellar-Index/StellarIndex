import type { ComponentProps } from 'react';

import { cn } from '@/lib/cn';

/**
 * Table primitives — a clean, hairline-ruled data table. Numbers should use
 * `align="right"` + the tnum class (Td applies tnum on right-aligned cells).
 * Wrap in TableWrap for horizontal scroll on narrow viewports.
 */
export function TableWrap({ className, ...props }: ComponentProps<'div'>) {
  return (
    <div
      className={cn(
        'rounded-card border-line bg-surface overflow-x-auto border',
        className,
      )}
      {...props}
    />
  );
}

export function Table({ className, ...props }: ComponentProps<'table'>) {
  return (
    <table
      className={cn('w-full border-collapse text-sm', className)}
      {...props}
    />
  );
}

export function THead({ className, ...props }: ComponentProps<'thead'>) {
  return (
    <thead
      className={cn(
        'border-line bg-surface-muted text-ink-muted border-b text-left text-[11px] font-medium tracking-wider uppercase',
        className,
      )}
      {...props}
    />
  );
}

export function TBody({ className, ...props }: ComponentProps<'tbody'>) {
  return <tbody className={cn('divide-line divide-y', className)} {...props} />;
}

export function TR({ className, ...props }: ComponentProps<'tr'>) {
  return (
    <tr
      className={cn('hover:bg-surface-muted/70 transition-colors', className)}
      {...props}
    />
  );
}

type CellProps = ComponentProps<'td'> & { align?: 'left' | 'right' | 'center' };

export function Th({
  align = 'left',
  className,
  ...props
}: ComponentProps<'th'> & { align?: 'left' | 'right' | 'center' }) {
  return (
    <th
      scope="col"
      className={cn(
        'px-4 py-2.5 font-medium whitespace-nowrap',
        align === 'right' && 'text-right',
        align === 'center' && 'text-center',
        className,
      )}
      {...props}
    />
  );
}

export function Td({ align = 'left', className, ...props }: CellProps) {
  return (
    <td
      className={cn(
        'text-ink-body px-4 py-3',
        align === 'right' && 'tnum text-right',
        align === 'center' && 'text-center',
        className,
      )}
      {...props}
    />
  );
}
