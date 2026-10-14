import type { ComponentProps, ReactNode } from 'react';

import { cn } from '@/lib/cn';

type CardProps = ComponentProps<'div'> & {
  /** `flat` drops the shadow (for nested cards / dense grids). */
  flat?: boolean;
  /** `interactive` adds a hover lift — use for clickable cards. */
  interactive?: boolean;
};

/**
 * Card is the base surface primitive: a surface panel on the canvas with a
 * hairline border and no shadow. Depth comes from surface layering and
 * whitespace, not chrome. Compose with CardHeader / CardBody.
 */
export function Card({
  flat,
  interactive,
  className,
  children,
  ...props
}: CardProps) {
  return (
    <div
      className={cn(
        'rounded-card border-line bg-surface border',
        flat ? 'shadow-none' : 'shadow-card',
        interactive &&
          'hover:border-line-strong transition-colors duration-150',
        className,
      )}
      {...props}
    >
      {children}
    </div>
  );
}

type CardHeaderProps = {
  title?: ReactNode;
  description?: ReactNode;
  actions?: ReactNode;
  /**
   * Heading rank for `title`. Defaults to 3, which is right for a card
   * nested under a SectionHeader (`ui/Page`'s `<h2>`). A card that IS a
   * page's top-level section must pass 2, or the outline skips h2 and
   * `heading-order` (WCAG 1.3.1) fails — the visual size is unchanged
   * either way, since the styling lives in the className, not the tag.
   */
  headingLevel?: 2 | 3 | 4;
  className?: string;
};

export function CardHeader({
  title,
  description,
  actions,
  headingLevel = 3,
  className,
}: CardHeaderProps) {
  const H = `h${headingLevel}` as const;
  return (
    <div
      className={cn(
        'border-line flex items-start justify-between gap-4 border-b px-4 py-3',
        className,
      )}
    >
      <div className="min-w-0">
        {title && (
          <H className="text-ink truncate text-sm font-semibold">{title}</H>
        )}
        {description && (
          <p className="text-ink-muted mt-0.5 text-[13px]">{description}</p>
        )}
      </div>
      {actions && (
        <div className="flex shrink-0 items-center gap-2">{actions}</div>
      )}
    </div>
  );
}

export function CardBody({ className, ...props }: ComponentProps<'div'>) {
  return <div className={cn('p-4', className)} {...props} />;
}

export function CardFooter({ className, ...props }: ComponentProps<'div'>) {
  return (
    <div
      className={cn(
        'border-line text-ink-muted flex items-center justify-between gap-3 border-t px-4 py-3 text-[13px]',
        className,
      )}
      {...props}
    />
  );
}
