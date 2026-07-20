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
});
