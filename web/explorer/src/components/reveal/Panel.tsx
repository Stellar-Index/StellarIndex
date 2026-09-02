import { twMerge } from 'tailwind-merge';
import type { RequestExample } from '@/api/client';
import { RequestReveal } from './RequestReveal';

export type PanelProps = {
  /** Title rendered in the panel header. Optional — some panels render their own. */
  title?: string;
  /**
   * Sub-label / context shown next to the title. Strings are the
   * common case; React nodes allow inline components (e.g. live-
   * updating freshness chips).
   */
  hint?: React.ReactNode;
  /** API request that produced this panel's data. */
  source?: RequestExample;
  /** Anchor id for deep-linking (e.g. `#confidence-card`). */
  panelId?: string;
  /**
   * Heading rank for `title`. Defaults to 3 (a panel nested under a
   * SectionHeader `<h2>`). A panel that IS a page's top-level section must
   * pass 2 or the outline skips h2 and `heading-order` (WCAG 1.3.1) fails.
   * Purely semantic — the visual style is on the className, not the tag.
   */
  headingLevel?: 2 | 3 | 4;
  className?: string;
  bodyClassName?: string;
  children: React.ReactNode;
};

/**
 * Panel — every visible card on the showcase composes one of these.
 * Provides:
 *   - Optional title + hint
 *   - The `<>` reveal tucked top-right
 *   - An anchor id so the article system (Phase 12) can deep-link
 *     `<RatesPanel anchorId="confidence-card" />` into the page.
 *
 * The component is intentionally thin — it renders chrome around
 * `children` and never touches data.
 */
export function Panel({
  title,
  hint,
  source,
  panelId,
  headingLevel = 3,
  className,
  bodyClassName,
  children,
}: PanelProps) {
  const H = `h${headingLevel}` as const;
  return (
    <section
      id={panelId}
      className={twMerge(
        'border-line bg-surface relative rounded-lg border p-4',
        className,
      )}
    >
      {(title || source) && (
        <header className="mb-3 flex items-start justify-between gap-2">
          {title && (
            <div>
              <H className="text-sm font-medium">{title}</H>
              {hint && <p className="text-ink-muted text-xs">{hint}</p>}
            </div>
          )}
          {source && <RequestReveal example={source} position="inline" />}
        </header>
      )}
      <div className={twMerge('text-sm', bodyClassName)}>{children}</div>
    </section>
  );
}
