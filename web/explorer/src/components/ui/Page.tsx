import Link from 'next/link';
import type { ComponentProps, ReactNode } from 'react';

import { cn } from '@/lib/cn';
import { breadcrumbJsonLd, serializeJsonLd } from '@/lib/seo';

/** The console content frame: centered, capped at max-w-page (1728px), with
 *  responsive horizontal padding. */
export function Container({ className, ...props }: ComponentProps<'div'>) {
  return (
    <div
      className={cn(
        'max-w-page mx-auto w-full px-4 sm:px-6 lg:px-8',
        className,
      )}
      {...props}
    />
  );
}

/** Vertical section rhythm — generous, consistent whitespace between blocks. */
export function Section({ className, ...props }: ComponentProps<'section'>) {
  return <section className={cn('py-6 sm:py-8', className)} {...props} />;
}

export type Crumb = { label: string; href?: string };

/**
 * PageHeader is the consistent top-of-page block: optional breadcrumb +
 * eyebrow, an h1 title, a description, and a right-aligned actions slot.
 */
export function PageHeader({
  title,
  description,
  eyebrow,
  breadcrumbs,
  actions,
  className,
}: {
  title: ReactNode;
  description?: ReactNode;
  eyebrow?: ReactNode;
  breadcrumbs?: Crumb[];
  actions?: ReactNode;
  className?: string;
}) {
  return (
    <div
      className={cn(
        'flex flex-col gap-4 sm:flex-row sm:items-end sm:justify-between',
        className,
      )}
    >
      <div className="min-w-0">
        {breadcrumbs && breadcrumbs.length > 0 && (
          <Breadcrumbs items={breadcrumbs} />
        )}
        {eyebrow && (
          <div className="text-brand-600 mb-1.5 text-xs font-medium tracking-wider uppercase">
            {eyebrow}
          </div>
        )}
        <h1 className="text-h1 text-ink font-semibold">{title}</h1>
        {description && (
          <p className="text-ink-muted mt-2 max-w-prose text-[15px] leading-relaxed">
            {description}
          </p>
        )}
      </div>
      {actions && (
        <div className="flex shrink-0 items-center gap-2">{actions}</div>
      )}
    </div>
  );
}

/**
 * Breadcrumbs — the visible trail AND its schema.org BreadcrumbList in one
 * place (FEC A1-6 one-rule). The JSON-LD is derived from the same `items`
 * array the nav renders, via lib/seo's breadcrumbJsonLd — pages must not
 * hand-roll BreadcrumbList (guarded in lib/fec-consolidation-guards.test.ts).
 */
export function Breadcrumbs({ items }: { items: Crumb[] }) {
  return (
    <nav
      aria-label="Breadcrumb"
      className="text-ink-muted mb-3 flex flex-wrap items-center gap-x-2 gap-y-1 text-xs"
    >
      <script
        type="application/ld+json"
        dangerouslySetInnerHTML={{
          __html: serializeJsonLd(breadcrumbJsonLd(items)),
        }}
      />
      {items.map((c, i) => (
        <span key={`${c.label}-${i}`} className="flex items-center gap-x-2">
          {/* Chevron reads as hierarchy ("Assets › USD"); a slash looked
              like a path fragment. Rendered only between crumbs — never
              trailing, so no orphaned separator. */}
          {i > 0 && (
            <span aria-hidden className="text-ink-faint select-none">
              ›
            </span>
          )}
          {c.href ? (
            <Link
              href={c.href}
              className="hover:text-brand-600 transition-colors"
            >
              {c.label}
            </Link>
          ) : (
            // Current page — the leaf of the trail: brighter + medium so the
            // hierarchy reads muted-parent › bold-current at a glance.
            <span
              className="text-ink-body max-w-[60vw] truncate font-medium sm:max-w-none"
              aria-current="page"
            >
              {c.label}
            </span>
          )}
        </span>
      ))}
    </nav>
  );
}

/** A lightweight section heading used inside pages (between cards). */
export function SectionHeader({
  title,
  description,
  actions,
  className,
}: {
  title: ReactNode;
  description?: ReactNode;
  actions?: ReactNode;
  className?: string;
}) {
  return (
    <div className={cn('mb-4 flex items-end justify-between gap-4', className)}>
      <div className="min-w-0">
        <h2 className="text-h3 text-ink font-semibold">{title}</h2>
        {description && (
          <p className="text-ink-muted mt-1 text-sm">{description}</p>
        )}
      </div>
      {actions && (
        <div className="flex shrink-0 items-center gap-2">{actions}</div>
      )}
    </div>
  );
}
