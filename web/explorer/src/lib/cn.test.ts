import { describe, it, expect } from 'vitest';

import { cn } from './cn';

describe('cn', () => {
  it('joins truthy classes and drops falsy ones', () => {
    expect(cn('a', false && 'b', undefined, null, 'c')).toBe('a c');
  });

  it('resolves conflicting Tailwind utilities last-wins', () => {
    expect(cn('px-2', 'px-4')).toBe('px-4');
    expect(cn('p-2', 'p-4')).toBe('p-4');
  });

  it('accepts conditional object inputs', () => {
    expect(cn('base', { active: true, hidden: false })).toBe('base active');
  });

  // FEC audit A1-1 guard: the custom max-w-page theme token must live in the
  // max-w conflict group, or Container className="max-w-*" overrides are
  // silently dead (both classes emitted; .max-w-page wins on stylesheet order).
  it('lets callers override the custom max-w-page token (and vice versa)', () => {
    expect(cn('max-w-page', 'max-w-4xl')).toBe('max-w-4xl');
    expect(cn('max-w-4xl', 'max-w-page')).toBe('max-w-page');
    expect(
      cn(
        'mx-auto w-full max-w-page px-4 sm:px-6 lg:px-8',
        'max-w-4xl space-y-6 py-10',
      ),
    ).not.toContain('max-w-page');
  });
});
