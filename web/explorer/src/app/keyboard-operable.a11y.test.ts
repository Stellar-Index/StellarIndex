import { readdirSync, readFileSync, statSync } from 'node:fs';
import { join } from 'node:path';

import { describe, expect, it } from 'vitest';

/**
 * KEYBOARD-OPERABILITY GUARD (WCAG 2.1.1)
 *
 * `DivergenceFeed.a11y.test.tsx` proves the divergence board is operable by
 * keyboard. It proves it for that board only — and the board was never the
 * whole finding. The same `<tr onClick>` shape carried pool-depth expansion
 * on `/liquidity-pools` and `/dexes/[source]`, where the row is the only way
 * to open the reserves detail. Three components, one mistake, and the thing
 * that let it spread three times is that nothing derived the set of rows
 * from source.
 *
 * So this file asks the structural question a behavioural test cannot ask of
 * a component it does not import: for EVERY clickable row in the tree, is
 * there a real control inside it?
 *
 * A `<tr onClick>` is not banned. Whole-row click is a good mouse
 * affordance, and the fix that shipped keeps it — it just stops being the
 * ONLY affordance. A native <button> in the row is what puts the action in
 * the tab order and gives Enter and Space activation without a hand-rolled
 * key handler, so a native button in the row is what this asserts.
 */

const SRC = join(__dirname, '..');

function blankComments(body: string): string {
  const blank = (m: string) => m.replace(/[^\n]/g, ' ');
  return body.replace(/\/\*[\s\S]*?\*\//g, blank).replace(/\/\/[^\n]*/g, blank);
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
      if (!/\.tsx$/.test(entry)) continue;
      if (/\.(test|spec)\.tsx$/.test(entry)) continue;
      out.push([full.slice(SRC.length + 1), readFileSync(full, 'utf8')]);
    }
  };
  walk(SRC);
  return out;
}

/** The `<tag ...>` opening tags in `body`, brace-aware, as [line, tag, source]. */
function openingTags(body: string): Array<[number, string, string]> {
  const text = blankComments(body);
  const out: Array<[number, string, string]> = [];
  for (const m of text.matchAll(/<([a-z][a-zA-Z0-9]*)\b/g)) {
    let i = m.index + m[0].length;
    let depth = 0;
    while (i < text.length) {
      const c = text[i];
      if (c === '{') depth++;
      else if (c === '}') depth--;
      else if (c === '>' && depth === 0) break;
      i++;
    }
    const line = text.slice(0, m.index).split('\n').length;
    out.push([line, m[1], text.slice(m.index, i + 1)]);
  }
  return out;
}

/** The markup from an opening `<tr ...>` to the row's closing `</tr>`. */
function rowBody(body: string, from: number): string {
  const end = body.indexOf('</tr>', from);
  return body.slice(from, end === -1 ? body.length : end);
}

/** `<button …>…</button>` elements inside a slice of markup. */
function buttonsIn(markup: string): string[] {
  const out: string[] = [];
  for (const m of markup.matchAll(/<button[\s>]/g)) {
    const end = markup.indexOf('</button>', m.index);
    out.push(markup.slice(m.index, end === -1 ? markup.length : end));
  }
  return out;
}

/** A control names itself with aria-label, or with the text it renders. */
function isNamed(button: string): boolean {
  const open = button.slice(0, button.indexOf('>') + 1);
  if (/aria-label[=\s]/.test(open)) return true;
  const children = button.slice(open.length).replace(/<[^>]*>/g, '');
  return /[A-Za-z]/.test(children);
}

describe('every clickable table row carries a real control', () => {
  const rows = sourceFiles().flatMap(([path, body]) => {
    const text = blankComments(body);
    return openingTags(body)
      .filter(([, tag, src]) => tag === 'tr' && /\bonClick\b/.test(src))
      .map(([line, , src]) => ({
        where: `${path}:${line}`,
        markup: rowBody(text, text.indexOf(src)),
      }));
  });

  it('finds the clickable rows to check', () => {
    // Vacuous-pass guard. Three today: the divergence board and the two
    // pool-depth expanders. If a refactor drops this to zero the guard has
    // stopped guarding, and that should be loud rather than green.
    expect(rows.length).toBeGreaterThanOrEqual(3);
  });

  it('puts a native <button> inside each of them', () => {
    // `<tr onClick>` alone is inoperable by keyboard and invisible to a
    // screen reader: no tab stop, no role, no Enter/Space activation.
    const offenders = rows
      .filter((r) => !/<button[\s>]/.test(r.markup))
      .map((r) => r.where);
    expect(offenders).toEqual([]);
  });

  it('gives each of those controls an accessible name and a focus style', () => {
    // A button reachable by Tab but unlabelled, or focusable with no visible
    // focus ring, trades one barrier for another (WCAG 4.1.2 / 2.4.7).
    //
    // Either source of a name counts. `aria-label` is right for the
    // divergence board, whose control reads "Plot" and needs to say WHICH
    // series; visible text is right for the depth expanders, which read
    // "Depth ▾" — and labelling those as well would put a second, different
    // name on visible words (WCAG 2.5.3). What is NOT a name is an icon on
    // its own, which is the case with no text and no aria-label.
    const unnamed = rows
      .filter((r) => !buttonsIn(r.markup).some(isNamed))
      .map((r) => r.where);
    expect(unnamed).toEqual([]);

    const unfocusable = rows
      .filter((r) => !/focus-visible:|focus:/.test(r.markup))
      .map((r) => r.where);
    expect(unfocusable).toEqual([]);
  });

  it('exposes the row state to assistive tech', () => {
    // Single-select boards say aria-current; expanders say aria-expanded.
    // Either is a claim about state; neither is optional.
    const mute = rows
      .filter((r) => !/aria-(current|expanded|pressed)[=\s]/.test(r.markup))
      .map((r) => r.where);
    expect(mute).toEqual([]);
  });
});

describe('a click-to-dismiss overlay always has a key path out', () => {
  // The other `onClick`-on-a-<div> shape in the tree is the modal backdrop.
  // Clicking it closes the dialog; a keyboard user needs Escape to do the
  // same, and `lib/useDialog` is where Escape, the focus trap, focus
  // move-in and focus restore all live. Reaching for a bare backdrop
  // without it is the regression this catches.
  const overlays = sourceFiles().filter(([, body]) =>
    openingTags(body).some(
      ([, tag, src]) =>
        tag === 'div' &&
        /\bonClick\b/.test(src) &&
        /\b(fixed|absolute)\b/.test(src) &&
        /\binset-0\b/.test(src),
    ),
  );

  it('finds the overlays to check', () => {
    expect(overlays.length).toBeGreaterThanOrEqual(3);
  });

  it('routes every one through useDialog', () => {
    const offenders = overlays
      .filter(([, body]) => !body.includes('useDialog'))
      .map(([path]) => path);
    expect(offenders).toEqual([]);
  });
});

describe('nothing rewrites the tab order by hand', () => {
  it('uses no positive tabIndex', () => {
    // A positive tabindex takes an element out of DOM order and ahead of
    // every natural tab stop on the page, which reorders the whole document
    // for everyone else too (WCAG 2.4.3).
    const offenders: string[] = [];
    for (const [path, body] of sourceFiles()) {
      const text = blankComments(body);
      for (const m of text.matchAll(/tabIndex=\{?\s*([0-9]+)/g)) {
        if (Number(m[1]) > 0) {
          offenders.push(
            `${path}:${text.slice(0, m.index).split('\n').length}`,
          );
        }
      }
    }
    expect(offenders).toEqual([]);
  });
});
