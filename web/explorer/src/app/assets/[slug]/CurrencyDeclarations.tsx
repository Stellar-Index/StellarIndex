import { type AssetDetail } from '@/api/hooks';

/**
 * CurrencyDeclarations — the issuer's own SEP-1 statements about the
 * asset's standing and backing (status, anchoring, reserve attestation,
 * redemption, SEP-8 regulation). Issuer-declared and unverified; the API
 * serves them only for a verified [[CURRENCIES]] match.
 */
export function CurrencyDeclarations({ asset }: { asset: AssetDetail }) {
  const rows: { key: string; value: React.ReactNode }[] = [];
  if (asset.currency_status) {
    rows.push({ key: 'status', value: asset.currency_status });
  }
  if (asset.is_asset_anchored != null) {
    rows.push({
      key: 'is_asset_anchored',
      value: asset.is_asset_anchored ? 'true' : 'false',
    });
  }
  if (asset.attestation_of_reserve) {
    rows.push({
      key: 'attestation_of_reserve',
      value: <ExternalLink href={asset.attestation_of_reserve} />,
    });
  }
  if (asset.redemption_instructions) {
    rows.push({
      key: 'redemption_instructions',
      value: asset.redemption_instructions,
    });
  }
  if (asset.regulated != null) {
    rows.push({ key: 'regulated', value: asset.regulated ? 'true' : 'false' });
  }
  if (asset.approval_server) {
    rows.push({
      key: 'approval_server',
      value: <ExternalLink href={asset.approval_server} />,
    });
  }
  if (asset.approval_criteria) {
    rows.push({ key: 'approval_criteria', value: asset.approval_criteria });
  }
  if (rows.length === 0) return null;

  return (
    <div className="border-line bg-surface-muted rounded-lg border p-3 text-xs">
      <h3 className="text-ink-muted mb-1 text-[11px] font-semibold tracking-wider uppercase">
        SEP-1 currency declarations
      </h3>
      <p className="text-ink-body">
        What the issuer states about this asset in their{' '}
        <span className="font-mono">stellar.toml</span> — not verified by
        Stellar Index.
      </p>
      <dl className="mt-2 space-y-1">
        {rows.map((r) => (
          <div key={r.key} className="flex flex-wrap gap-x-2">
            <dt className="text-ink-muted font-mono">{r.key}</dt>
            <dd className="break-words">{r.value}</dd>
          </div>
        ))}
      </dl>
    </div>
  );
}

function ExternalLink({ href }: { href: string }) {
  return (
    <a
      href={href}
      target="_blank"
      rel="noopener noreferrer nofollow"
      className="hover:text-brand-600 font-mono break-all underline"
    >
      {href}
    </a>
  );
}
