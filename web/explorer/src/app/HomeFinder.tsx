'use client';

import { useRouter } from 'next/navigation';
import { type FormEvent, useState, useSyncExternalStore } from 'react';

import { Button, Input } from '@/components/ui';
import { cn } from '@/lib/cn';

type Shape =
  'account' | 'contract' | 'transaction' | 'ledger' | 'pair' | 'asset' | null;

const LABEL: Record<NonNullable<Shape>, string> = {
  account: 'Account',
  contract: 'Contract',
  transaction: 'Tx hash',
  ledger: 'Ledger',
  pair: 'Pair',
  asset: 'Asset',
};

// The same shape rules the omnibox applies (SearchModal.tsx); keep them in
// step so the two entry points never disagree on where a query lands.
export function detectShape(raw: string): Shape {
  const s = raw.trim();
  if (!s) return null;
  if (/^G[A-Z2-7]{55}$/.test(s)) return 'account';
  if (/^C[A-Z2-7]{55}$/.test(s)) return 'contract';
  if (/^[0-9a-fA-F]{64}$/.test(s)) return 'transaction';
  if (/^\d{1,12}$/.test(s)) return 'ledger';
  if (/^[A-Za-z0-9]+[\s/\\-]+[A-Za-z0-9]+$/.test(s)) return 'pair';
  return 'asset';
}

export function destinationFor(raw: string): string | null {
  const s = raw.trim();
  switch (detectShape(s)) {
    case 'account':
      return `/accounts/${s}/`;
    case 'contract':
      return `/contracts/${s}/`;
    case 'transaction':
      return `/transactions/${s.toLowerCase()}/`;
    case 'ledger':
      return `/ledgers/${Number(s)}/`;
    // Pairs need coin lookups the omnibox owns; the assets directory search
    // resolves either leg, so it is the honest landing for both shapes.
    case 'pair':
    case 'asset':
      return `/assets/?q=${encodeURIComponent(s)}`;
    default:
      return null;
  }
}

/** The landing finder: one wide field that routes by input shape. */
const WIDE = '(min-width: 640px)';
const noop = () => {};

function subscribeWide(onChange: () => void) {
  if (typeof window.matchMedia !== 'function') return noop;
  const m = window.matchMedia(WIDE);
  m.addEventListener('change', onChange);
  return () => m.removeEventListener('change', onChange);
}

function readWide(): boolean {
  return (
    typeof window.matchMedia !== 'function' || window.matchMedia(WIDE).matches
  );
}

export function HomeFinder() {
  const router = useRouter();
  const [q, setQ] = useState('');
  const shape = detectShape(q);
  const wide = useSyncExternalStore(subscribeWide, readWide, () => true);

  function onSubmit(e: FormEvent<HTMLFormElement>) {
    e.preventDefault();
    const to = destinationFor(q);
    if (to) router.push(to);
  }

  return (
    <form
      role="search"
      onSubmit={onSubmit}
      className="relative mx-auto w-full max-w-[720px]"
    >
      <label htmlFor="home-finder" className="sr-only">
        Search by account, contract, transaction, ledger or asset
      </label>
      <Input
        id="home-finder"
        name="q"
        value={q}
        onChange={(e) => setQ(e.target.value)}
        autoComplete="off"
        spellCheck={false}
        placeholder={
          wide
            ? 'Account, contract, transaction hash, ledger or asset'
            : 'Address, tx, ledger or asset'
        }
        className={cn(
          'h-13 rounded-lg pl-4 text-base',
          shape ? 'pr-[196px] sm:pr-[228px]' : 'pr-[104px] sm:pr-[136px]',
        )}
      />
      {shape && (
        <span className="text-ink-muted border-line bg-surface-muted pointer-events-none absolute top-1/2 right-[104px] inline-block -translate-y-1/2 rounded-sm border px-1.5 py-0.5 text-[11px] font-medium tracking-wider uppercase sm:right-[136px]">
          {LABEL[shape]}
        </span>
      )}
      <Button
        type="submit"
        size="lg"
        className="absolute top-1/2 right-1.5 w-[88px] -translate-y-1/2 sm:w-[120px]"
      >
        Search
      </Button>
    </form>
  );
}
