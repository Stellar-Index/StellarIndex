// Single source of truth for pill / chip tones across the explorer.
//
// The explorer renders one (dark) theme, so every tone is an ADAPTIVE token
// pair — a dark-tint `*-subtle` / `*-50` background with a bright `*-strong` /
// `*-700` text. That keeps ≥ 4.5:1 WCAG contrast AND reads as a dark-surface
// chip. NEVER give a pill a raw Tailwind `-100/-800` palette pair: those are
// fixed LIGHT values that glare on the dark surface, and for the inverted
// brand ramp they also fail contrast (`bg-brand-100 text-brand-800` = 1.46:1).
//
// The accent families (violet/teal/indigo/purple/yellow) live in globals.css
// next to the semantic six (brand/up/down/warn/bad/ok); this module is the one
// place that maps a tone name — and the shared source/venue set — to classes.

export type PillTone =
  | 'neutral'
  | 'brand'
  | 'up'
  | 'warn'
  | 'down'
  | 'violet'
  | 'teal'
  | 'indigo'
  | 'purple'
  | 'yellow';

const PILL_TONE_CLASS: Record<PillTone, string> = {
  neutral: 'bg-line text-ink',
  brand: 'bg-brand-50 text-brand-700',
  up: 'bg-up-subtle text-up-strong',
  warn: 'bg-warn-50 text-warn-700',
  down: 'bg-down-subtle text-down-strong',
  violet: 'bg-violet-subtle text-violet-strong',
  teal: 'bg-teal-subtle text-teal-strong',
  indigo: 'bg-indigo-subtle text-indigo-strong',
  purple: 'bg-purple-subtle text-purple-strong',
  yellow: 'bg-yellow-subtle text-yellow-strong',
};

// The fallback for an unknown key — a quiet neutral chip so a source/category
// the map hasn't caught up with still renders. Matches the prior inline
// `?? 'bg-surface-subtle text-ink-body'` fallbacks the per-view maps used.
const PILL_TONE_FALLBACK = 'bg-surface-subtle text-ink-body';

/** Background + text classes for a pill tone (adaptive, dark-surface safe). */
export function pillToneClass(tone: PillTone): string {
  return PILL_TONE_CLASS[tone];
}

// Data-source / venue name → tone. Shared by every table that chips a source
// (/dexes, /exchanges, /oracles); each view reads only the keys it renders, so
// the union here is harmless. Assignments are DISTINCT within any one view's
// key set (e.g. comet=violet vs kraken=purple both show on /dexes).
export const SOURCE_PILL_TONE: Record<string, PillTone> = {
  // DEX / AMM venues
  soroswap: 'up',
  phoenix: 'warn',
  aquarius: 'brand',
  sdex: 'neutral',
  comet: 'violet',
  // CEX venues
  binance: 'yellow',
  coinbase: 'brand',
  kraken: 'purple',
  bitstamp: 'teal',
  // Oracle feeds
  'reflector-dex': 'up',
  'reflector-cex': 'brand',
  'reflector-fx': 'violet',
  redstone: 'down',
  band: 'warn',
};

/** Class string for a source/venue chip (neutral fallback for unknown names). */
export function sourceToneClass(name: string): string {
  const tone = SOURCE_PILL_TONE[name];
  return tone ? PILL_TONE_CLASS[tone] : PILL_TONE_FALLBACK;
}

// Protocol category → tone. The API serves the authoritative category string;
// unknown categories fall through to the neutral chip.
export const CATEGORY_PILL_TONE: Record<string, PillTone> = {
  dex: 'neutral',
  amm: 'up',
  lending: 'brand',
  yield: 'violet',
  bridge: 'warn',
  oracle: 'indigo',
  token: 'teal',
};

/** Class string for a category chip (neutral fallback for unknown categories). */
export function categoryToneClass(category: string): string {
  const tone = CATEGORY_PILL_TONE[category];
  return tone ? PILL_TONE_CLASS[tone] : PILL_TONE_FALLBACK;
}
