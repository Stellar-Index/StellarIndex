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

  it('Mono truncates a long identifier head…tail', () => {
    render(<Mono value="GABCDEFGHIJKLMNOP" truncate copy={false} />);
    expect(screen.getByText('GABCDE…MNOP')).toBeInTheDocument();
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
