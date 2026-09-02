import { describe, it, expect } from 'vitest';
import { render, screen } from '@testing-library/react';

import {
  Badge,
  Button,
  ButtonLink,
  Card,
  CardHeader,
  CardBody,
  CardFooter,
  Stat,
  StatGrid,
  StatCell,
  Mono,
  TableWrap,
  Table,
  THead,
  TBody,
  TR,
  Th,
  Td,
  Container,
  Section,
  PageHeader,
  Breadcrumbs,
  EmptyState,
  Skeleton,
  Callout,
  Input,
  Field,
} from '@/components/ui';

// These assert behaviour and semantics (text, roles, element type) — NOT
// Tailwind classes, which a redesign is expected to change. The net's job is
// to catch a primitive's *contract* breaking during a restructure, not to
// freeze its look.
describe('ui primitives — render + semantics', () => {
  it('Badge renders its children', () => {
    render(<Badge tone="up">Live</Badge>);
    expect(screen.getByText('Live')).toBeInTheDocument();
  });

  it('Button is a real <button>', () => {
    render(<Button>Go</Button>);
    expect(screen.getByRole('button', { name: 'Go' }).tagName).toBe('BUTTON');
  });

  it('ButtonLink is an <a> carrying its href', () => {
    render(<ButtonLink href="/assets">Explore</ButtonLink>);
    expect(screen.getByRole('link', { name: 'Explore' })).toHaveAttribute('href', '/assets');
  });

  it('Stat shows its label and value', () => {
    render(<Stat label="24h Volume" value="$1.2M" />);
    expect(screen.getByText('24h Volume')).toBeInTheDocument();
    expect(screen.getByText('$1.2M')).toBeInTheDocument();
  });

  it('StatGrid/StatCell compose their children', () => {
    render(
      <StatGrid cols={2}>
        <StatCell>alpha</StatCell>
        <StatCell>beta</StatCell>
      </StatGrid>,
    );
    expect(screen.getByText('alpha')).toBeInTheDocument();
    expect(screen.getByText('beta')).toBeInTheDocument();
  });

  it('Card sections all render', () => {
    render(
      <Card>
        <CardHeader title="Head" />
        <CardBody>Body</CardBody>
        <CardFooter>Foot</CardFooter>
      </Card>,
    );
    for (const t of ['Head', 'Body', 'Foot']) {
      expect(screen.getByText(t)).toBeInTheDocument();
    }
  });

  // #335 F5 (WCAG 1.3.1 / axe heading-order): CardHeader hardcoded <h3>
  // while ui/Page's SectionHeader is <h2>, so a card used as a page's
  // top-level section produced h1 → h3 and skipped a level. The rank is
  // now caller-selectable; the visual style is unchanged either way.
  it('CardHeader titles default to h3 and honour headingLevel', () => {
    const { unmount } = render(<CardHeader title="Default rank" />);
    expect(
      screen.getByRole('heading', { level: 3, name: 'Default rank' }),
    ).toBeInTheDocument();
    unmount();

    render(<CardHeader title="Top-level section" headingLevel={2} />);
    const h2 = screen.getByRole('heading', { level: 2, name: 'Top-level section' });
    expect(h2.tagName).toBe('H2');
    // The rank moved, the styling did not.
    expect(h2).toHaveClass('truncate');
    expect(
      screen.queryByRole('heading', { level: 3 }),
    ).not.toBeInTheDocument();
  });

  it('Mono truncates a long identifier head…tail and keeps the full value on hover', () => {
    render(<Mono value="GABCDEFGHIJKLMNOP" truncate copy={false} />);
    const elided = screen.getByText('GABCDE…MNOP');
    expect(elided).toBeInTheDocument();
    // #356: a truncated identifier must never be the only copy of itself
    // on the page — the full value round-trips through the title.
    expect(elided).toHaveAttribute('title', 'GABCDEFGHIJKLMNOP');
  });

  it('Mono adds no title when nothing was elided', () => {
    render(<Mono value="GABCD" truncate copy={false} />);
    expect(screen.getByText('GABCD')).not.toHaveAttribute('title');
  });

  it('Table primitives render a semantic table', () => {
    render(
      <TableWrap>
        <Table>
          <THead>
            <TR>
              <Th>Asset</Th>
            </TR>
          </THead>
          <TBody>
            <TR>
              <Td>XLM</Td>
            </TR>
          </TBody>
        </Table>
      </TableWrap>,
    );
    expect(screen.getByRole('table')).toBeInTheDocument();
    expect(screen.getByRole('columnheader', { name: 'Asset' })).toBeInTheDocument();
    expect(screen.getByRole('cell', { name: 'XLM' })).toBeInTheDocument();
  });

  it('Page scaffold renders the h1 title', () => {
    render(
      <Container>
        <Section>
          <PageHeader title="Markets" />
        </Section>
      </Container>,
    );
    expect(screen.getByRole('heading', { level: 1, name: 'Markets' })).toBeInTheDocument();
  });

  it('Breadcrumbs render each crumb (linked + current)', () => {
    render(<Breadcrumbs items={[{ label: 'Home', href: '/' }, { label: 'Markets' }]} />);
    expect(screen.getByRole('link', { name: 'Home' })).toHaveAttribute('href', '/');
    expect(screen.getByText('Markets')).toBeInTheDocument();
  });

  it('Breadcrumbs marks the current (last, unlinked) crumb with aria-current="page"', () => {
    // ACC-03: only the non-linked crumb is "current" — the linked ones are
    // not the current page and must not carry aria-current.
    render(<Breadcrumbs items={[{ label: 'Home', href: '/' }, { label: 'Markets' }]} />);
    expect(screen.getByText('Markets')).toHaveAttribute('aria-current', 'page');
    expect(screen.getByRole('link', { name: 'Home' })).not.toHaveAttribute('aria-current');
  });

  it('ACC-12: Field associates its error text with the control via aria-describedby/aria-invalid', () => {
    render(
      <Field label="Name" htmlFor="key-name" error="Name is required">
        <Input id="key-name" />
      </Field>,
    );
    const input = screen.getByRole('textbox');
    const errorText = screen.getByText('Name is required');
    expect(input).toHaveAttribute('aria-invalid', 'true');
    expect(input.getAttribute('aria-describedby')).toBe(errorText.id);
    expect(errorText.id).toBeTruthy();
  });

  it('ACC-12: Field associates hint text (no error) without aria-invalid', () => {
    render(
      <Field label="Description" htmlFor="key-desc" hint="Optional context">
        <Input id="key-desc" />
      </Field>,
    );
    const input = screen.getByRole('textbox');
    const hintText = screen.getByText('Optional context');
    expect(input).not.toHaveAttribute('aria-invalid', 'true');
    expect(input.getAttribute('aria-describedby')).toBe(hintText.id);
  });

  it('Feedback: EmptyState + Callout render content and role', () => {
    render(<EmptyState title="No data yet" />);
    expect(screen.getByText('No data yet')).toBeInTheDocument();

    render(
      <Callout tone="warn" title="Careful">
        body text
      </Callout>,
    );
    // warn/bad callouts announce assertively via role=alert
    expect(screen.getByRole('alert')).toHaveTextContent('Careful');
    expect(screen.getByText('body text')).toBeInTheDocument();
  });

  it('Skeleton renders a placeholder element', () => {
    const { container } = render(<Skeleton />);
    expect(container.firstChild).not.toBeNull();
  });

  it('Input forwards native props (placeholder)', () => {
    render(<Input placeholder="Search assets" />);
    expect(screen.getByPlaceholderText('Search assets')).toBeInTheDocument();
  });
});
