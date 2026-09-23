import { Badge, type BadgeTone } from '@/components/ui';
import {
  DIRECTORY_OPERATOR_OVERRIDE_SOURCE,
  hasDirectoryScamFlag,
  scamFlagTags,
} from '@/lib/directory-tags';

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

// Which tags mean "warn the user" is NOT this component's decision to
// make: it is DIRECTORY_SCAM_FLAG_TAGS in lib/directory-tags, the one
// frontend list, pinned equal to the Go DirectoryScamFlagTags by
// pricingguard's TestScamFlagTagSet_MatchesFrontend. The same six tags
// make the server withhold the issuer's price and rank its assets last,
// so a private copy here (it held two of the six, matched
// case-sensitively) rendered an address the API refuses to price as a
// neutral, unwarned label.
function toneFor(tag: string): BadgeTone {
  return hasDirectoryScamFlag([tag]) ? 'bad' : 'neutral';
}

/**
 * DirectoryLabel — curated third-party name + tags for an address,
 * from the MIT-licensed stellar-expert/public-directory set (synced
 * server-side; see the API's `directory` field). Display attribution
 * only: listing is not endorsement, so the source is always named
 * inline. A scam-class tag (scamFlagTags — matched case-insensitively)
 * renders in the danger tone and adds the warning line. An operator
 * override row no longer carries the upstream's scam tag, so it is badged
 * and the attribution says the flag was removed after review.
 */
export function DirectoryLabel({ info }: { info: DirectoryInfo }) {
  const flagged = scamFlagTags(info.tags);
  const warn = flagged.length > 0;
  const overridden = info.source === DIRECTORY_OPERATOR_OVERRIDE_SOURCE;
  return (
    <div className="space-y-1.5">
      <div className="flex flex-wrap items-center gap-2">
        <span
          className={warn ? 'text-bad-700 font-medium' : 'text-ink font-medium'}
        >
          {info.name}
        </span>
        {info.tags.map((t) => (
          <Badge key={t} tone={toneFor(t)}>
            #{t}
          </Badge>
        ))}
        {overridden && <Badge tone="warn">flag lifted on review</Badge>}
      </div>
      <p className="text-ink-muted text-[11px]">
        {warn && (
          <span className="text-bad-700 font-medium">
            Flagged {flagged.join(' + ')} by the community directory — treat
            with caution.{' '}
          </span>
        )}
        Label from the{' '}
        <a
          href="https://github.com/stellar-expert/public-directory"
          target="_blank"
          rel="noreferrer noopener"
          className="hover:text-brand-600 underline"
        >
          StellarExpert public directory
        </a>{' '}
        (community-curated; listing is not endorsement)
        {overridden ? (
          <>
            , except its scam-class flag, which an operator reviewed as a false
            positive and removed
          </>
        ) : null}
        {info.domain ? <> · {info.domain}</> : null}.
      </p>
    </div>
  );
}
