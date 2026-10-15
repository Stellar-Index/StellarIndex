'use client';

import { useCallback, useState } from 'react';
import Link from 'next/link';
import { X } from 'lucide-react';

import { useDialog } from '@/lib/useDialog';

type Source = { name: string; href: string; note: string };

// Mirrors docs/operations/data-licensing-register.md: the enabled feeds and
// the credit each one's terms ask for. Disabled vendors are not listed.
const GROUPS: { title: string; sources: Source[] }[] = [
  {
    title: 'Ledger data',
    sources: [
      {
        name: 'Stellar history archives',
        href: 'https://developers.stellar.org/docs/data/apis/history-archives',
        note: 'Every ledger, transaction and event, exported and verified from the public archives.',
      },
      {
        name: 'AWS Public Blockchain Data',
        href: 'https://registry.opendata.aws/aws-public-blockchain/',
        note: 'Raw Stellar ledger metadata, from the AWS Open Data Registry.',
      },
    ],
  },
  {
    title: 'Prices and markets',
    sources: [
      {
        name: 'CoinGecko',
        href: 'https://www.coingecko.com',
        note: 'Price data powered by CoinGecko.',
      },
      {
        name: 'Binance, Coinbase, Kraken, Bitstamp',
        href: '/sources',
        note: "Public market data from each exchange. A venue's own terms may limit reuse of its raw observations.",
      },
      {
        name: 'Chainlink, Band, RedStone, Reflector',
        href: '/oracles',
        note: 'On-chain oracle prices, recorded as published.',
      },
    ],
  },
  {
    title: 'Foreign exchange',
    sources: [
      {
        name: 'European Central Bank',
        href: 'https://www.ecb.europa.eu/stats/policy_and_exchange_rates/euro_reference_exchange_rates/html/index.en.html',
        note: 'Source: ECB euro foreign exchange reference rates.',
      },
      {
        name: 'Massive',
        href: 'https://massive.com',
        note: 'Fiat exchange rates.',
      },
    ],
  },
  {
    title: 'Directories and metadata',
    sources: [
      {
        name: 'StellarExpert directory',
        href: 'https://github.com/stellar-expert/stellar-expert-explorer',
        note: 'Account labels, used under the MIT licence.',
      },
      {
        name: 'Issuer stellar.toml files (SEP-1)',
        href: 'https://developers.stellar.org/docs/tokens/publishing-asset-info',
        note: 'Asset names, logos and issuer details, as each issuer publishes them.',
      },
      {
        name: 'Stellar Development Foundation',
        href: 'https://stellar.org',
        note: 'Curated real-world asset series, credited with the curator’s consent.',
      },
    ],
  },
];

/** Footer link that opens the list of data sources and their required credits. */
export function DataSourceDisclosures() {
  const [open, setOpen] = useState(false);
  const close = useCallback(() => setOpen(false), []);
  const dialogRef = useDialog<HTMLDivElement>(open, close);

  return (
    <>
      <button
        type="button"
        onClick={() => setOpen(true)}
        className="hover:text-ink-body"
      >
        Data Source Disclosures
      </button>
      {open && (
        <div
          className="fixed inset-0 z-50 flex items-end justify-center bg-black/60 p-4 sm:items-center"
          onClick={close}
        >
          <div
            ref={dialogRef}
            tabIndex={-1}
            role="dialog"
            aria-modal="true"
            aria-labelledby="data-sources-title"
            className="border-line bg-surface max-h-[85vh] w-full max-w-2xl overflow-y-auto rounded-md border p-6 text-left"
            onClick={(e) => e.stopPropagation()}
          >
            <header className="mb-5 flex items-center justify-between">
              <h2
                id="data-sources-title"
                className="text-ink text-base font-medium"
              >
                Data source disclosures
              </h2>
              <button
                type="button"
                onClick={close}
                className="text-ink-faint hover:text-ink-body p-1"
                aria-label="Close"
              >
                <X className="h-4 w-4" />
              </button>
            </header>
            <div className="text-ink-muted space-y-5 text-[13px]">
              {GROUPS.map((g) => (
                <section key={g.title} className="space-y-2">
                  <h3 className="text-ink-faint text-[11px] font-medium tracking-wider uppercase">
                    {g.title}
                  </h3>
                  <ul className="space-y-2">
                    {g.sources.map((s) => (
                      <li key={s.name}>
                        <SourceName source={s} />
                        <span> · {s.note}</span>
                      </li>
                    ))}
                  </ul>
                </section>
              ))}
              <p className="border-line border-t pt-4">
                Values are provided as is, may be delayed, and are not financial
                advice. How each number is built:{' '}
                <Link
                  href="/methodology"
                  className="text-ink-body hover:text-ink underline underline-offset-4"
                  onClick={close}
                >
                  methodology
                </Link>
                . Every feed and its role:{' '}
                <Link
                  href="/sources"
                  className="text-ink-body hover:text-ink underline underline-offset-4"
                  onClick={close}
                >
                  sources
                </Link>
                .
              </p>
            </div>
          </div>
        </div>
      )}
    </>
  );
}

function SourceName({ source }: { source: Source }) {
  const cls = 'text-ink-body hover:text-ink font-medium';
  if (source.href.startsWith('/')) {
    return (
      <Link href={source.href} className={cls}>
        {source.name}
      </Link>
    );
  }
  return (
    <a
      href={source.href}
      target="_blank"
      rel="noopener noreferrer"
      className={cls}
    >
      {source.name}
    </a>
  );
}
