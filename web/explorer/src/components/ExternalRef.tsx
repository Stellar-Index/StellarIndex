import { cn } from '@/lib/cn';

/** Shows the destination itself as the link text, so an off-site hop is never a surprise. */
export function ExternalRef({
  href,
  className,
}: {
  href: string;
  className?: string;
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
      {href.replace(/^https?:\/\/(www\.)?/, '')}
    </a>
  );
}
