import { Award, Flame, TrendingDown, TrendingUp } from 'lucide-react';
import { twMerge } from 'tailwind-merge';
import { formatRelative } from '@/lib/format';

export type StreakIndicatorProps =
  | {
      kind: 'streak';
      direction: 'up' | 'down' | 'flat';
      days: number;
    }
  | {
      kind: 'ath';
      /** ISO time the ATH was hit. Used for the "2h ago" relative label. */
      at: string;
    }
  | {
      kind: 'atl';
      at: string;
    }
  | {
      kind: 'new';
      /** Anything within the last 24h gets the badge per design-inventory §6.4. */
      since: string;
    };

/**
 * Streak / ATH / ATL / new-listing chip — the punchy badges from
 * design-inventory §6.4. Each variant uses a distinct colour +
 * icon so they're spottable in dense lists.
 */
export function StreakIndicator(
  props: StreakIndicatorProps & { className?: string },
) {
  const className = props.className ?? '';
  switch (props.kind) {
    case 'streak': {
      if (props.direction === 'flat' || props.days === 0) {
        return null;
      }
      const Icon = props.direction === 'up' ? TrendingUp : TrendingDown;
      const tone =
        props.direction === 'up'
          ? 'bg-up-subtle/50 text-up-strong'
          : 'bg-down-subtle/50 text-down-strong';
      return (
        <span
          className={twMerge(
            'inline-flex items-center gap-1 rounded-full px-2 py-0.5 text-xs font-medium',
            tone,
            className,
          )}
        >
          <Icon className="h-3 w-3" aria-hidden />
          {props.days} {props.days === 1 ? 'day' : 'days'} {props.direction}
        </span>
      );
    }
    case 'ath':
      return (
        <span
          className={twMerge(
            'bg-warn-50 text-warn-700 inline-flex items-center gap-1 rounded-full px-2 py-0.5 text-xs font-medium',
            className,
          )}
          title={`All-time high reached ${props.at}`}
        >
          <Award className="h-3 w-3" aria-hidden />
          ATH {formatRelative(props.at)}
        </span>
      );
    case 'atl':
      return (
        <span
          className={twMerge(
            'bg-down-subtle text-down-strong inline-flex items-center gap-1 rounded-full px-2 py-0.5 text-xs font-medium',
            className,
          )}
          title={`All-time low reached ${props.at}`}
        >
          <TrendingDown className="h-3 w-3" aria-hidden />
          ATL {formatRelative(props.at)}
        </span>
      );
    case 'new':
      return (
        <span
          className={twMerge(
            'inline-flex items-center gap-1 rounded-full bg-purple-100 px-2 py-0.5 text-xs font-medium text-purple-800',
            className,
          )}
          title={`First seen ${props.since}`}
        >
          <Flame className="h-3 w-3" aria-hidden />
          new
        </span>
      );
  }
}
