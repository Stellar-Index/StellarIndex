import { readdirSync, readFileSync, statSync } from 'node:fs';
import { join } from 'node:path';

import { describe, expect, it } from 'vitest';

/**
 * COLOUR-CONTRAST GUARD (WCAG 1.4.3 / 1.4.11)
 *
 * A sibling of `lib/trust-surface-guards.test.ts`, and written for the same
 * reason: the obligation is enforced by convention at hundreds of call sites,
 * there is no chokepoint, and nothing derived the site set from source. So
 * the failures were never in one component — they were in whichever
 * components a remediation pass did not happen to open:
 *
 *   - `bg-brand-600 text-white` = 4.37:1 was fixed on `ui/Button` and
 *     `SortPill` and left live on ten other segmented controls;
 *   - `text-brand-600` = 3.95:1 on surface-muted stood on ~380 link sites,
 *     recorded as known-failing rather than fixed;
 *   - `bg-down text-white` = 3.53:1 on the issuer scam badge — the same
 *     white-on-brand mistake in a different hue.
 *
 * Enumerating from `src/` at test time is the point: the eleventh segmented
 * control fails on the day it is written, not at the next audit.
 *
 * SCOPE AND ITS LIMITS, stated so a green run is not read as more than it is:
 *   - It pairs a `text-*` with the `bg-*` in the SAME class string, and with
 *     the ambient surfaces when the string names no background. Text placed
 *     on a tinted background by an ANCESTOR element is scored against the
 *     ambient surfaces, which is the common case but not every case.
 *   - It applies the 4.5:1 normal-text threshold to every pair, including
 *     ones that carry only an icon and would be allowed 3:1 by 1.4.11. That
 *     is deliberate and currently free: nothing in the tree needs the looser
 *     bound, and a uniform rule needs no per-site judgement.
 */

const APP = __dirname;
const SRC = join(APP, '..');
const CSS = readFileSync(join(APP, 'globals.css'), 'utf8');

// ── WCAG 2.x relative luminance and contrast, from the spec text rather
// than a library, so the numbers in the review are checkable by hand.
// https://www.w3.org/TR/WCAG21/#dfn-relative-luminance

type Rgb = [number, number, number];

function parseHex(hex: string): Rgb {
  const h = hex.replace('#', '');
  const full =
    h.length === 3
      ? h
          .split('')
          .map((c) => c + c)
          .join('')
      : h;
  return [
    parseInt(full.slice(0, 2), 16),
    parseInt(full.slice(2, 4), 16),
    parseInt(full.slice(4, 6), 16),
  ];
}

function luminance(rgb: Rgb): number {
  const [r, g, b] = rgb.map((c) => {
    const s = c / 255;
    return s <= 0.03928 ? s / 12.92 : ((s + 0.055) / 1.055) ** 2.4;
  }) as Rgb;
  return 0.2126 * r + 0.7152 * g + 0.0722 * b;
}

function contrast(fg: string, bg: string): number {
  const a = luminance(parseHex(fg));
  const b = luminance(parseHex(bg));
  const [hi, lo] = a > b ? [a, b] : [b, a];
  return (hi + 0.05) / (lo + 0.05);
}

/** `fg` composited over `bg` at `alpha` — what a `/NN` opacity utility does. */
function over(fg: string, bg: string, alpha: number): string {
  const f = parseHex(fg);
  const b = parseHex(bg);
  const mix = f.map((c, i) => Math.round(c * alpha + b[i] * (1 - alpha)));
  return '#' + mix.map((c) => c.toString(16).padStart(2, '0')).join('');
}

// ── The palette, read from the stylesheet that ships.

function tokens(): Record<string, string> {
  const out: Record<string, string> = {};
  for (const m of CSS.matchAll(
    /--color-([a-z0-9-]+):\s*(#[0-9a-fA-F]{3,6})\s*;/g,
  )) {
    out[m[1]] = m[2];
  }
  return out;
}

const T = tokens();
const NAMED: Record<string, string> = { white: '#ffffff', black: '#000000' };
const colour = (name: string): string | undefined => T[name] ?? NAMED[name];

/** Every background a component can be dropped onto without saying so. */
const AMBIENT = [
  'surface',
  'surface-canvas',
  'surface-muted',
  'surface-subtle',
];

const AA_TEXT = 4.5;

// ── Source scan.

/** A `bg-`/`text-` utility, with any variant prefixes and `/NN` opacity. */
const UTILITY =
  /(?:^|\s)(?:[a-z-]+:)*(bg|text)-([a-z0-9-]+)(?:\/(\d{1,3}))?(?=\s|$)/g;

/**
 * A literal made ENTIRELY of class tokens. The filter is what makes the scan
 * trustworthy: a lone apostrophe in the source can desynchronise any quote
 * matcher, and the debris it produces always carries `<`, `=`, `{` or `(`.
 */
const CLASS_SHAPE = /^[\sA-Za-z0-9:_\-./[\]%!,#]*$/;

function blankComments(body: string): string {
  const blank = (m: string) => m.replace(/[^\n]/g, ' ');
  return body.replace(/\/\*[\s\S]*?\*\//g, blank).replace(/\/\/[^\n]*/g, blank);
}

/**
 * Class strings in `body`, as [1-based line, string].
 *
 * Three independent passes — single-quoted, double-quoted, template — so a
 * desync in one cannot hide the others. A template keeps only its static
 * text: the `${...}` holes hold their own quoted strings (the two branches of
 * an inline ternary, typically), and those are found by the first two passes
 * as the separate elements they describe.
 */
function classStrings(body: string): Array<[number, string]> {
  const text = blankComments(body);
  const out: Array<[number, string]> = [];
  const passes: Array<[RegExp, boolean]> = [
    [/'([^'\n]*)'/g, false],
    [/"([^"\n]*)"/g, false],
    [/`([^`]*)`/g, true],
  ];
  for (const [re, isTemplate] of passes) {
    for (const m of text.matchAll(re)) {
      let s = m[1];
      if (isTemplate) s = s.replace(/\$\{[^{}]*\}/g, ' ');
      if (!CLASS_SHAPE.test(s)) continue;
      UTILITY.lastIndex = 0;
      if (!UTILITY.test(` ${s} `)) continue;
      const line = text.slice(0, m.index).split('\n').length;
      out.push([line, s]);
    }
  }
  return out;
}

type Layer = { name: string; hex: string; alpha: number };

function layers(classString: string): { bg: Layer[]; fg: Layer[] } {
  const bg: Layer[] = [];
  const fg: Layer[] = [];
  for (const m of ` ${classString} `.matchAll(UTILITY)) {
    const hex = colour(m[2]);
    if (!hex) continue;
    const layer = { name: m[2], hex, alpha: m[3] ? Number(m[3]) / 100 : 1 };
    (m[1] === 'bg' ? bg : fg).push(layer);
  }
  return { bg, fg };
}

/** The WORST ratio this foreground can reach, and the background it hits it on. */
function worstCase(fg: Layer, bg: Layer[]): { ratio: number; on: string } {
  const cases: Array<{ ratio: number; on: string }> = [];
  const measure = (base: string, label: string, alpha: number) => {
    for (const ambient of alpha < 1 ? AMBIENT : [null]) {
      const behind = ambient ? over(base, T[ambient], alpha) : base;
      const ink = fg.alpha < 1 ? over(fg.hex, behind, fg.alpha) : fg.hex;
      cases.push({ ratio: contrast(ink, behind), on: label });
    }
  };
  if (bg.length > 0) {
    for (const b of bg) measure(b.hex, b.name, b.alpha);
  } else {
    for (const a of AMBIENT) measure(T[a], a, 1);
  }
  return cases.reduce((a, b) => (a.ratio <= b.ratio ? a : b));
}

function sourceFiles(): Array<[string, string]> {
  const out: Array<[string, string]> = [];
  const walk = (dir: string) => {
    for (const entry of readdirSync(dir)) {
      const full = join(dir, entry);
      if (statSync(full).isDirectory()) {
        if (entry === 'node_modules' || entry === '.next') continue;
        walk(full);
        continue;
      }
      if (!/\.tsx?$/.test(entry)) continue;
      if (/\.(test|spec)\.tsx?$/.test(entry)) continue;
      out.push([full.slice(SRC.length + 1), readFileSync(full, 'utf8')]);
    }
  };
  walk(SRC);
  return out;
}

type Offender = { where: string; pair: string; ratio: string };

function offenders(files: Array<[string, string]>): {
  bad: Offender[];
  evaluated: number;
} {
  const bad: Offender[] = [];
  let evaluated = 0;
  for (const [path, body] of files) {
    for (const [line, s] of classStrings(body)) {
      const { bg, fg } = layers(s);
      for (const ink of fg) {
        evaluated++;
        const { ratio, on } = worstCase(ink, bg);
        if (ratio < AA_TEXT) {
          bad.push({
            where: `${path}:${line}`,
            pair: `text-${ink.name} on ${on}`,
            ratio: `${ratio.toFixed(2)}:1`,
          });
        }
      }
    }
  }
  return { bad, evaluated };
}

describe('the contrast maths itself', () => {
  // A guard whose instrument is wrong reports a confident PASS over nothing.
  it('reproduces known WCAG ratios', () => {
    expect(contrast('#ffffff', '#000000')).toBeCloseTo(21, 5);
    expect(contrast('#808080', '#808080')).toBeCloseTo(1, 5);
    // The two numbers this change is about, at their pre-fix values.
    expect(contrast('#4270f0', '#101216')).toBeCloseTo(4.29, 2);
    expect(contrast('#ffffff', '#4270f0')).toBeCloseTo(4.37, 2);
  });

  it('composites a /NN opacity the way the browser does', () => {
    expect(over('#ffffff', '#000000', 0.5)).toBe('#808080');
    expect(over('#ffffff', '#000000', 1)).toBe('#ffffff');
  });

  it('reads the shipped palette, not a copy of it', () => {
    expect(Object.keys(T).length).toBeGreaterThan(40);
    for (const name of [...AMBIENT, 'brand-600', 'brand-fill', 'up', 'down']) {
      expect(T[name]).toMatch(/^#[0-9a-f]{6}$/i);
    }
  });
});

describe('the explorer ships one theme, and this matrix covers it', () => {
  // "Check both themes" has an answer here, and it is "there is one". That
  // is a fact about the stylesheet, so it is asserted rather than assumed:
  // the day a light palette lands, this fails and points at the matrix that
  // has to grow a second column.
  it('declares a single dark colour-scheme with no light override', () => {
    expect(CSS).toMatch(/color-scheme:\s*dark/);
    expect(CSS).not.toMatch(/prefers-color-scheme:\s*light/);
    expect(CSS).not.toMatch(/@media\s*\(prefers-color-scheme/);
    expect(CSS).not.toMatch(/\[data-theme/);
  });

  it('defines every colour token exactly once', () => {
    const names = [...CSS.matchAll(/--color-([a-z0-9-]+):/g)].map((m) => m[1]);
    const dupes = names.filter((n, i) => names.indexOf(n) !== i);
    expect(dupes).toEqual([]);
  });

  it('renders no `dark:` variant, which would imply a second palette', () => {
    const users = sourceFiles()
      .filter(([, body]) => /(?:^|\s|')dark:[a-z]/.test(blankComments(body)))
      .map(([path]) => path);
    expect(users).toEqual([]);
  });
});

describe('the movers palette clears AA in every band', () => {
  // The audit's sharpest finding: the >±20% bands — the biggest movers, the
  // numbers people opened the page to read — were the least readable on it
  // (`bg-up text-white` = 2.23:1, `bg-down` = 3.53:1). Derived from the
  // component's own class strings so a new band is covered by construction.
  const pill = readFileSync(
    join(SRC, 'components/primitives/DirectionPill.tsx'),
    'utf8',
  );

  const bands = classStrings(pill)
    .map(([, s]) => ({ s, ...layers(s) }))
    .filter((b) => b.bg.length > 0 && b.fg.length > 0);

  it('covers every band DirectionPill can render', () => {
    // Vacuous-pass guard: eight bands today (no-data, flat, ±<5, ±<20, ±20+).
    expect(bands.length).toBeGreaterThanOrEqual(8);
  });

  it.each(bands.map((b) => [b.s, b] as const))('%s', (_label, band) => {
    for (const ink of band.fg) {
      expect(worstCase(ink, band.bg).ratio).toBeGreaterThanOrEqual(AA_TEXT);
    }
  });
});

describe('white text sits only on the brand FILL steps', () => {
  // The role split is the fix, not the individual hex. A fill has to go
  // DARKER to carry white text and ink has to go LIGHTER to sit on a dark
  // surface; one token doing both is what put `bg-brand-600 text-white` at
  // 4.37:1 on eleven segmented controls while its link role sat at 3.95:1.
  it('keeps both fill steps legible under white', () => {
    expect(contrast('#ffffff', T['brand-fill'])).toBeGreaterThanOrEqual(
      AA_TEXT,
    );
    expect(contrast('#ffffff', T['brand-fill-hover'])).toBeGreaterThanOrEqual(
      AA_TEXT,
    );
  });

  it('keeps brand-600 legible as ink on every surface it lands on', () => {
    for (const bg of [...AMBIENT, 'brand-50']) {
      expect(contrast(T['brand-600'], T[bg])).toBeGreaterThanOrEqual(AA_TEXT);
    }
  });

  it('never fills with a token that is itself an ink', () => {
    // The rule, derived rather than listed: a token bright enough to be READ
    // on the dark canvas is by construction too bright to sit behind white.
    // Every white-on-colour failure in the audit was one of those — brand-600
    // (4.37:1), brand-700 on the sidebar hover (2.30:1), `bg-down` on the
    // scam badge (3.53:1) — and the fill steps, which are dark tints, are
    // admitted by the same test without an exception list.
    const isInk = (name: string) =>
      T[name] !== undefined &&
      Math.min(...AMBIENT.map((a) => contrast(T[name], T[a]))) >= AA_TEXT;
    expect(isInk('brand-600')).toBe(true);
    expect(isInk('brand-fill')).toBe(false);

    const bad: string[] = [];
    for (const [path, body] of sourceFiles()) {
      for (const [line, s] of classStrings(body)) {
        const { bg, fg } = layers(s);
        if (!fg.some((f) => f.hex.toLowerCase() === '#ffffff')) continue;
        for (const b of bg.filter((x) => isInk(x.name))) {
          bad.push(`${path}:${line} text-white on bg-${b.name}`);
        }
      }
    }
    expect(bad).toEqual([]);
  });
});

describe('every colour pair the explorer renders clears WCAG AA', () => {
  const files = sourceFiles();
  const { bad, evaluated } = offenders(files);

  it('scanned the whole tree', () => {
    // Without this, a scanner that quietly stopped matching would report a
    // confident pass over nothing at all.
    expect(files.length).toBeGreaterThan(150);
    expect(evaluated).toBeGreaterThan(1500);
  });

  it('finds no pair below 4.5:1', () => {
    expect(bad).toEqual([]);
  });
});
