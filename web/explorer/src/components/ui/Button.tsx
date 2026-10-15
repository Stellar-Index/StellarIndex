import Link from 'next/link';
import type { ComponentProps, ReactNode } from 'react';

import { cn } from '@/lib/cn';

export type ButtonVariant =
  'primary' | 'secondary' | 'ghost' | 'subtle' | 'danger';
export type ButtonSize = 'sm' | 'md' | 'lg';

const base =
  'inline-flex items-center justify-center gap-2 whitespace-nowrap rounded-md font-medium transition-colors duration-150 disabled:pointer-events-none disabled:opacity-50 focus-visible:outline-hidden focus-visible:ring-2 focus-visible:ring-brand-500/70 focus-visible:ring-offset-2 focus-visible:ring-offset-surface-canvas';

const variants: Record<ButtonVariant, string> = {
  // White-on-fill contrast (WCAG 1.4.3): brand-fill 10.65:1, hover 6.39:1.
  // The rest fill is muted so a primary button does not glare; hover lightens.
  primary:
    'bg-brand-fill text-white hover:bg-brand-fill-hover active:bg-brand-100',
  secondary:
    'bg-surface text-ink border border-line-strong hover:border-line-input hover:bg-surface-muted',
  ghost: 'text-ink-body hover:bg-surface-muted hover:text-ink',
  subtle: 'bg-surface-subtle text-ink hover:bg-line',
  // bad-500 IS the down red (#f6465d) the DirectionPill fix re-inked: white
  // on it is 3.53:1 (fail). Dark-on-red is 5.58:1, and 9.77:1 on bad-700.
  danger: 'bg-bad-500 text-surface-canvas hover:bg-bad-700',
};

const sizes: Record<ButtonSize, string> = {
  sm: 'h-8 px-3 text-xs',
  md: 'h-9 px-3.5 text-[13px]',
  lg: 'h-10 px-5 text-sm',
};

function buttonClass(
  variant: ButtonVariant = 'primary',
  size: ButtonSize = 'md',
  className?: string,
): string {
  return cn(base, variants[variant], sizes[size], className);
}

type ButtonProps = ComponentProps<'button'> & {
  variant?: ButtonVariant;
  size?: ButtonSize;
};

export function Button({
  variant = 'primary',
  size = 'md',
  className,
  type = 'button',
  ...props
}: ButtonProps) {
  return (
    <button
      type={type}
      className={buttonClass(variant, size, className)}
      {...props}
    />
  );
}

type ButtonLinkProps = ComponentProps<typeof Link> & {
  variant?: ButtonVariant;
  size?: ButtonSize;
  children: ReactNode;
};

/** A Link styled as a Button — for navigation actions. */
export function ButtonLink({
  variant = 'primary',
  size = 'md',
  className,
  children,
  ...props
}: ButtonLinkProps) {
  return (
    <Link className={buttonClass(variant, size, className)} {...props}>
      {children}
    </Link>
  );
}
