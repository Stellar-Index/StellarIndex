import { readFileSync, readdirSync, statSync } from 'node:fs';
import { dirname, join, relative } from 'node:path';
import { fileURLToPath } from 'node:url';

import { describe, it, expect } from 'vitest';

// Guard for #879: `lib/directory-tags.ts` is the ONE place a
// directory/scam tag-class vocabulary (malicious/unsafe/fraud/scam/
// hack/phishing/counterfeit) may be named in code. A re-derivation
// elsewhere is how DirectoryLabel and IssuersTable each grew their own,
// mismatched subset. This walks `src`, skips generated/type/test files,
// and fails on a live tag-class literal (a regex `.test(` over those
// words, or a `Set`/array literal containing one) outside the authority.

const SRC_DIR = join(dirname(fileURLToPath(import.meta.url)), '..');
const AUTHORITY = join(SRC_DIR, 'lib', 'directory-tags.ts');

const TAG_WORDS =
  '(malicious|unsafe|fraud|scam|hack|phishing|counterfeit)';
// A regex literal built from tag words and applied with `.test(`.
const REGEX_CLASSIFIER = new RegExp(
  `\\/[^/\\n]*\\b${TAG_WORDS}\\b[^/\\n]*\\/[a-z]*\\s*\\.test\\(`,
  'i',
);
// A Set/array literal enumerating tag words as string members.
const SET_CLASSIFIER = new RegExp(
  `\\[[^\\]]*['"]${TAG_WORDS}['"][^\\]]*\\]`,
  'i',
);

function listFiles(dir: string): string[] {
  const out: string[] = [];
  for (const entry of readdirSync(dir)) {
    const full = join(dir, entry);
    const st = statSync(full);
    if (st.isDirectory()) {
      out.push(...listFiles(full));
    } else if (/\.(ts|tsx)$/.test(entry) && !/\.test\.(ts|tsx)$/.test(entry)) {
      out.push(full);
    }
  }
  return out;
}

describe('directory tag-class authority guard', () => {
  it('names no scam/directory tag-class literal outside lib/directory-tags.ts', () => {
    const offenders: string[] = [];
    for (const file of listFiles(SRC_DIR)) {
      if (file === AUTHORITY) continue;
      if (file.endsWith(join('api', 'types.ts'))) continue; // generated
      const text = readFileSync(file, 'utf8');
      for (const line of text.split('\n')) {
        const trimmed = line.trim();
        if (trimmed.startsWith('//') || trimmed.startsWith('*')) continue;
        if (REGEX_CLASSIFIER.test(line) || SET_CLASSIFIER.test(line)) {
          offenders.push(`${relative(SRC_DIR, file)}: ${trimmed}`);
        }
      }
    }
    expect(offenders).toEqual([]);
  });
});
