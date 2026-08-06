import { Badge, type BadgeTone } from '@/components/ui';

// Wire shape of the API's DirectoryInfo schema (accounts/contracts
// `directory` field + /v1/directory entries). Structural rather than
// importing the generated type so non-generated callers (the account
// page's hand-mirrored AccountStateResp) can pass their copy.
export interface DirectoryInfo {
  name: string;
  domain?: string;
  tags: string[];
  source: string;
}

// Upstream tags that mean "warn the user", per the public-directory
// registry: #malicious = theft/scam/spam/phishing, #unsafe =
// obsolete or potentially dangerous.
const WARN_TAGS = new Set(['malicious', 'unsafe']);

function toneFor(tag: string): BadgeTone {
  return WARN_TAGS.has(tag) ? 'bad' : 'neutral';
}

/**
 * DirectoryLabel — curated third-party name + tags for an address,
 * from the MIT-licensed stellar-expert/public-directory set (synced
 * server-side; see the API's `directory` field). Display attribution
 * only: listing is not endorsement, so the source is always named
 * inline. `malicious`/`unsafe` tags render in the danger tone.
 */
export function DirectoryLabel({ info }: { info: DirectoryInfo }) {
  const warn = info.tags.some((t) => WARN_TAGS.has(t));
  return (
    <div className="space-y-1.5">
      <div className="flex flex-wrap items-center gap-2">
        <span className={warn ? 'font-medium text-bad-700' : 'font-medium text-ink'}>
          {info.name}
        </span>
        {info.tags.map((t) => (
          <Badge key={t} tone={toneFor(t)}>
            #{t}
          </Badge>
        ))}
      </div>
      <p className="text-[11px] text-ink-muted">
        {warn && (
          <span className="font-medium text-bad-700">
            Flagged {info.tags.filter((t) => WARN_TAGS.has(t)).join(' + ')} by
            the community directory — treat with caution.{' '}
          </span>
        )}
        Label from the{' '}
        <a
          href="https://github.com/stellar-expert/public-directory"
          target="_blank"
          rel="noreferrer noopener"
          className="underline hover:text-brand-600"
        >
          StellarExpert public directory
        </a>{' '}
        (community-curated; listing is not endorsement)
        {info.domain ? <> · {info.domain}</> : null}.
      </p>
    </div>
  );
}
