import type { EnvelopeFlags } from '@/api/client';
import { Badge, type BadgeTone } from '@/components/ui';

export type FreshnessMarkerProps = {
  /** The response envelope's `flags`; absent renders nothing. */
  flags?: EnvelopeFlags | null;
  className?: string;
};

const MARKERS: {
  key: keyof EnvelopeFlags;
  label: string;
  tone: BadgeTone;
  title: string;
}[] = [
  {
    key: 'frozen',
    label: 'Frozen',
    tone: 'bad',
    title: 'Anomaly detection froze this pair; the price is held, not live.',
  },
  {
    key: 'stale',
    label: 'Stale',
    tone: 'warn',
    title:
      'Served past its freshness bound; the latest reading is older than expected.',
  },
  {
    key: 'divergence_warning',
    label: 'Sources diverge',
    tone: 'warn',
    title: 'Cross-reference sources disagree with this value beyond tolerance.',
  },
  {
    key: 'reduced_redundancy',
    label: 'Reduced redundancy',
    tone: 'warn',
    title: 'Fewer independent sources than usual contributed to this value.',
  },
  {
    key: 'triangulated',
    label: 'Triangulated',
    tone: 'brand',
    title: 'No direct market; derived through an intermediate pair.',
  },
];

/**
 * FreshnessMarker — renders the degradation flags a /v1 envelope carries
 * (frozen, stale, divergence, reduced redundancy, triangulated) so a
 * client-fetched page shows the same caveats the API published.
 */
export function FreshnessMarker({ flags, className }: FreshnessMarkerProps) {
  if (!flags) return null;
  const set = MARKERS.filter((m) => flags[m.key] === true);
  if (set.length === 0) return null;
  return (
    <span className={className}>
      {set.map((m) => (
        <Badge key={m.key} tone={m.tone} title={m.title} className="mr-1">
          {m.label}
        </Badge>
      ))}
    </span>
  );
}
