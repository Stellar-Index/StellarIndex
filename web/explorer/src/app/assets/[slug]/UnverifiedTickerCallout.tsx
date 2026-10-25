import Link from 'next/link';

import type { components } from '@/api/types';
import { Badge, Callout } from '@/components/ui';
import { assetHref } from '@/lib/fiat-slugs';

export function UnverifiedTickerCallout({
  symbol,
  warning,
}: {
  symbol: string;
  warning: components['schemas']['UnverifiedWarning'];
}) {
  return (
    <Callout
      tone="warn"
      title={
        <span className="flex flex-wrap items-center gap-2">
          Unverified {symbol}
          <Badge tone="warn">Ticker collision</Badge>
        </span>
      }
    >
      {warning.note}
      {/* An empty verified_asset_id means no Stellar issuance exists
          (XRP, BTC, …): /assets/{slug} would render "not found". */}
      {warning.verified_asset_id && (
        <>
          {' '}
          The verified asset is available at{' '}
          <Link
            href={assetHref(warning.verified_slug)}
            className="font-medium underline"
          >
            {warning.verified_name}
          </Link>
          {warning.verified_issuer && (
            <>
              {' '}
              — issued by{' '}
              <span className="font-medium">{warning.verified_issuer}</span>
            </>
          )}
          .
        </>
      )}
    </Callout>
  );
}
