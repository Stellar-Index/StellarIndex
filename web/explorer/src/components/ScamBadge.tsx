import { Badge } from '@/components/ui';
import { scamFlagTags } from '@/lib/directory-tags';

// ScamBadge — compact directory-flag pill for an asset row. Renders only
// when the issuer's curated directory tags include a scam-warning flag
// (malicious/unsafe/fraud/scam/hack/phishing). Third-party attribution,
// never a verification signal — but the same flag DOES withhold the row's
// price/market cap (the scam gate) and sinks the row to the bottom of the
// ranking (demoteFlaggedLast, #356). Badged, demoted, never hidden.
export function ScamBadge({
  tags,
  className,
}: {
  tags?: string[] | null;
  className?: string;
}) {
  const flagged = scamFlagTags(tags);
  if (flagged.length === 0) return null;
  return (
    <Badge
      tone="bad"
      className={className}
      title={`Flagged by the stellar-expert community directory as: ${flagged.join(
        ', ',
      )} (third-party attribution — display-only, not a StellarIndex verification signal)`}
    >
      ⚠ Flagged
    </Badge>
  );
}
