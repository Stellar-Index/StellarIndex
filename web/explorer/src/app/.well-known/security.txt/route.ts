// /.well-known/security.txt — RFC-9116 disclosure metadata.
//
// Generated at build time from values that mirror the SECURITY.md
// policy. The Expires field is stamped at one year from the build
// date so it stays valid as long as we rebuild and redeploy in
// that window (CF Pages rebuilds on every push to main; status
// site rebuilds with the explorer).

import { NextResponse } from 'next/server';

export const dynamic = 'force-static';

const SITE_URL = 'https://stellarindex.io';

export function GET() {
  const now = new Date();
  const expires = new Date(now);
  expires.setUTCFullYear(now.getUTCFullYear() + 1);

  const lines = [
    `# Stellar Index — security.txt`,
    `# RFC-9116. Mirrors ${SITE_URL}/research → SECURITY.md.`,
    ``,
    `Contact: mailto:security@stellarindex.io`,
    `Expires: ${expires.toISOString()}`,
    `Preferred-Languages: en`,
    `Canonical: ${SITE_URL}/.well-known/security.txt`,
    `Policy: https://github.com/StellarIndex/stellar-index/blob/main/SECURITY.md`,
    `Acknowledgments: https://github.com/StellarIndex/stellar-index/security/advisories`,
    ``,
  ].join('\n');

  return new NextResponse(lines, {
    headers: {
      'content-type': 'text/plain; charset=utf-8',
      'cache-control': 'public, max-age=86400',
    },
  });
}
