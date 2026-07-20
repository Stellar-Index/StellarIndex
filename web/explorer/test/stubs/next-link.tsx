import * as React from 'react';

// Hermetic next/link stub for tests — renders a plain anchor so components
// that use <Link> can render without the Next app-router runtime/context.
type LinkStubProps = Omit<React.AnchorHTMLAttributes<HTMLAnchorElement>, 'href'> & {
  href: string | { pathname?: string };
  children?: React.ReactNode;
};

export default function Link({ href, children, ...rest }: LinkStubProps) {
  const resolved = typeof href === 'string' ? href : (href?.pathname ?? '#');
  return (
    <a href={resolved} {...rest}>
      {children}
    </a>
  );
}
