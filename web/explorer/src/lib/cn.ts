import { clsx, type ClassValue } from 'clsx';
import { extendTailwindMerge } from 'tailwind-merge';

// Custom theme tokens that collide with a default Tailwind scale MUST be
// registered here, or twMerge silently keeps both classes and stylesheet
// order decides the winner (e.g. `cn('max-w-page','max-w-4xl')` would keep
// both and Container's 1728px cap would beat caller overrides).
const twMerge = extendTailwindMerge({
  extend: { classGroups: { 'max-w': ['max-w-page'] } },
});

/**
 * cn merges Tailwind class strings with conflict resolution. clsx handles
 * conditional/array/object class inputs; twMerge dedupes conflicting Tailwind
 * utilities (the last wins), so callers can layer overrides on a base class
 * without specificity fights. This is the single class-composition helper for
 * the whole design system — every UI primitive uses it.
 *
 *   cn('px-3 py-2', isActive && 'bg-brand-600', className)
 */
export function cn(...inputs: ClassValue[]): string {
  return twMerge(clsx(inputs));
}
