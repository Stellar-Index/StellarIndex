// Re-export every design-system primitive so callers import from one
// barrel rather than the per-file paths.
//
// Per docs/architecture/explorer-data-inventory.md §6 these
// primitives compose throughout the site. Keep them small,
// presentational, and free of API/data-fetch concerns — every
// primitive takes its data via props and renders deterministically.

export { DirectionPill } from './DirectionPill';
export type { DirectionPillProps } from './DirectionPill';

export { Sparkline } from './Sparkline';
export type { SparklineProps } from './Sparkline';

export { RollingNumber } from './RollingNumber';

export { FreshnessMarker } from './FreshnessMarker';
export type { FreshnessMarkerProps } from './FreshnessMarker';
