import type { Metadata } from 'next';
import { Check, Minus } from 'lucide-react';

import {
  Badge,
  ButtonLink,
  Card,
  CardBody,
  Container,
} from '@/components/ui';

export const metadata: Metadata = {
  title: 'Pricing — free access, quotas, SLAs',
  description:
    'Stellar Index is free. Anonymous public reads at 6,000 req/min per IP, plus free registered accounts for per-key attribution and usage analytics — no payments, no card. Need more per-key throughput? Talk to us about a partner limit.',
  alternates: { canonical: '/pricing' },
};

interface Tier {
  name: string;
  price: string;
  priceSubtitle?: string;
  rateLimit: string;
  cta: { label: string; href: string };
  highlight?: boolean;
  description: string;
  features: string[];
  notFeatures?: string[];
}

const TIERS: Tier[] = [
  {
    name: 'Anonymous',
    price: '$0',
    priceSubtitle: 'forever',
    rateLimit: '6,000 req/min per IP',
    cta: { label: 'Read the docs', href: 'https://docs.stellarindex.io' },
    description:
      'Anonymous public reads. Same data as every tier, just rate-limited per IP. Perfect for prototyping, low-traffic embeds, and read-only integrations.',
    features: [
      'Every public endpoint',
      'No signup, no API key, no auth',
      'Same VWAP / freshness as every tier',
      '6,000 requests / minute per source IP',
    ],
    notFeatures: ['Per-key attribution', 'Usage history (30d)', 'Dedicated SLA'],
  },
  {
    name: 'Free account',
    price: '$0',
    priceSubtitle: 'self-service',
    rateLimit: '1,000 req/min per key',
    cta: { label: 'Create account', href: '/signup' },
    description:
      'Register in one curl (POST /v1/register) or sign in with magic-link, mint an API key, get per-key usage analytics and a budget that is yours rather than shared with every client on your IP. Designed for builders and agents shipping to customers.',
    highlight: true,
    features: [
      'Every public endpoint, same data',
      '1,000 requests / minute per key (a per-key budget, not a raise)',
      'One-curl onboarding: POST /v1/register',
      'Per-key usage history (30d)',
      'Mint & rotate keys at /account',
      'Email support',
    ],
    notFeatures: ['Dedicated SLA', 'Staff-set partner limits'],
  },
];

export default function PricingPage() {
  return (
    <Container className="space-y-12 py-10 sm:py-14">
      <header className="mx-auto max-w-2xl space-y-4 text-center">
        <p className="text-xs font-medium uppercase tracking-wider text-brand-600">
          Plans
        </p>
        <h1 className="text-h1 font-semibold text-ink md:text-display-sm">
          Pricing
        </h1>
        <p className="text-lg leading-relaxed text-ink-muted">
          Stellar Index is free — there are no paid plans. Anonymous
          reads work without an account; a free account (one curl:
          POST /v1/register) adds per-key usage analytics and a budget
          that is yours alone rather than shared with everything else
          on your IP. It is not a throughput upgrade — on the hosted
          deployment an anonymous IP&apos;s limit deliberately exceeds a
          single free key&apos;s. Higher partner limits are set by our
          staff on request.
        </p>
      </header>

      <div className="grid grid-cols-1 gap-5 sm:grid-cols-2">
        {TIERS.map((tier) => (
          <TierCard key={tier.name} tier={tier} />
        ))}
      </div>

      <Card className="p-6 sm:p-8">
        <div className="flex flex-col gap-5 sm:flex-row sm:items-start sm:justify-between">
          <div className="space-y-2.5">
            <Badge tone="brand">Partner limits</Badge>
            <h2 className="text-h3 font-semibold text-ink">
              Need a bigger per-key budget?
            </h2>
            <p className="max-w-2xl text-sm text-ink-muted">
              A free key is stamped at 1,000 req/min. Wallets,
              exchanges, and redistributors that need more get a
              staff-set partner limit — there is nothing to buy. Email
              us with your use-case and scale and we&apos;ll raise your
              key&apos;s limit.
            </p>
          </div>
          <ButtonLink
            href="mailto:sales@stellarindex.io"
            variant="primary"
            className="shrink-0"
          >
            Talk to us
          </ButtonLink>
        </div>
      </Card>

      <Card className="p-6 sm:p-8">
        <h2 className="text-h3 font-semibold text-ink">Honest notes</h2>
        <ul className="mt-4 space-y-2.5 text-sm text-ink-body">
          <li>
            <strong className="text-ink-body">
              Free is not a trial.
            </strong>{' '}
            Anonymous reads are a permanent commitment — open,
            public-tier access is core to what Stellar Index is.
          </li>
          <li>
            <strong className="text-ink-body">
              Same data, every tier.
            </strong>{' '}
            We do not gate endpoints, freshness, or precision behind
            any tier. The difference is attribution, usage reporting,
            support model, and SLA — never the data itself.
          </li>
          <li>
            <strong className="text-ink-body">
              No card, ever.
            </strong>{' '}
            Magic-link account, mint a key, ship. There is no billing
            integration and no payment flow anywhere on the platform.
          </li>
          <li>
            <strong className="text-ink-body">
              Higher limits are a conversation, not a checkout.
            </strong>{' '}
            Partner rate limits are set by our staff.{' '}
            <a
              href="mailto:sales@stellarindex.io"
              className="text-brand-600 hover:underline"
            >
              sales@stellarindex.io
            </a>{' '}
            is the fastest way to arrange one.
          </li>
        </ul>
      </Card>
    </Container>
  );
}

function TierCard({ tier }: { tier: Tier }) {
  const ctaVariant = tier.highlight ? 'primary' : 'secondary';
  return (
    <Card className={tier.highlight ? 'ring-1 ring-brand-500/40' : undefined}>
      <CardBody className="flex h-full flex-col">
        <div className="mb-3 flex items-center justify-between gap-2">
          <h2 className="text-h3 font-semibold text-ink">{tier.name}</h2>
          {tier.highlight && <Badge tone="brand">Self-service</Badge>}
        </div>
        <div>
          <div className="font-mono text-3xl font-semibold tnum text-ink">
            {tier.price}
          </div>
          {tier.priceSubtitle && (
            <div className="text-xs text-ink-muted">{tier.priceSubtitle}</div>
          )}
        </div>
        <div className="mt-3 rounded-md bg-surface-muted px-3 py-2 font-mono text-xs text-ink-body">
          {tier.rateLimit}
        </div>
        <p className="mt-3 text-sm text-ink-muted">{tier.description}</p>

        <ul className="mt-4 space-y-1.5 text-sm">
          {tier.features.map((f) => (
            <li key={f} className="flex items-start gap-2 text-ink-body">
              <Check className="mt-0.5 h-4 w-4 shrink-0 text-up" />
              <span>{f}</span>
            </li>
          ))}
          {tier.notFeatures?.map((f) => (
            <li key={f} className="flex items-start gap-2 text-ink-faint">
              <Minus className="mt-0.5 h-4 w-4 shrink-0" />
              <span>{f}</span>
            </li>
          ))}
        </ul>

        <div className="mt-auto pt-5">
          {tier.cta.href.startsWith('http') ? (
            <ButtonLink
              href={tier.cta.href}
              target="_blank"
              rel="noreferrer noopener"
              variant={ctaVariant}
              className="w-full"
            >
              {tier.cta.label}
            </ButtonLink>
          ) : (
            <ButtonLink
              href={tier.cta.href}
              variant={ctaVariant}
              className="w-full"
            >
              {tier.cta.label}
            </ButtonLink>
          )}
        </div>
      </CardBody>
    </Card>
  );
}
