'use client';

import { useQuery, useQueryClient } from '@tanstack/react-query';
import { BellRing, Loader2, Pause, Play, Plus, Trash2 } from 'lucide-react';
import { useCallback, useState } from 'react';

import {
  ApiError,
  createPriceAlert,
  deletePriceAlert,
  listPriceAlerts,
  updatePriceAlert,
  type CreatePriceAlertRequest,
  type DashboardPriceAlert,
} from '@/api/account';
import {
  Badge,
  Button,
  Callout,
  Card,
  CardBody,
  CardFooter,
  CardHeader,
  Container,
  EmptyState,
  Field,
  Input,
  PageHeader,
  Section,
  Select,
  Skeleton,
  Table,
  TableWrap,
  TBody,
  Td,
  Th,
  THead,
  TR,
} from '@/components/ui';
import { fmtInt, fmtRelative } from '@/lib/account-format';

import { AccountGate } from '../AccountGate';

const DOCS_URL = 'https://docs.stellarindex.io';

/**
 * /dashboard/price-alerts — customer-managed price alerts. Same shape as
 * the API-keys page (session-cookie CRUD via @/api/account): a table of
 * alerts, a create form, an enable/disable toggle (PATCH), and
 * delete-with-confirm. A firing alert is delivered as a `price.alert`
 * webhook event to the account's subscribed webhooks — the empty state
 * and the delivery note below make that dependency explicit.
 */
export default function PriceAlertsPage() {
  return <AccountGate>{() => <PriceAlertsBody />}</AccountGate>;
}

function PriceAlertsBody() {
  const queryClient = useQueryClient();
  const alertsQuery = useQuery<DashboardPriceAlert[], Error>({
    queryKey: ['dashboard', 'price-alerts'],
    queryFn: ({ signal }) => listPriceAlerts(signal),
  });
  const alerts = alertsQuery.data ?? null;

  const [actionError, setActionError] = useState<string | null>(null);
  const [notice, setNotice] = useState<string | null>(null);
  const [showForm, setShowForm] = useState(false);
  const [creating, setCreating] = useState(false);
  // Per-row pending state so the toggle/delete buttons on one alert
  // spin without freezing the whole table.
  const [busyId, setBusyId] = useState<string | null>(null);

  const loadError = alertsQuery.error
    ? alertsQuery.error instanceof ApiError
      ? (alertsQuery.error.detail ?? alertsQuery.error.message)
      : 'Failed to load price alerts'
    : null;
  const error = actionError ?? loadError;

  const refresh = useCallback(async () => {
    setActionError(null);
    await queryClient.invalidateQueries({
      queryKey: ['dashboard', 'price-alerts'],
    });
  }, [queryClient]);

  async function handleToggle(alert: DashboardPriceAlert) {
    setBusyId(alert.id);
    setActionError(null);
    try {
      await updatePriceAlert(alert.id, { enabled: !alert.enabled });
      setNotice(
        `Alert ${alert.enabled ? 'paused' : 'enabled'} for ${shortAsset(alert.base_asset)}/${shortAsset(alert.quote_asset)}.`,
      );
      await refresh();
    } catch (err) {
      setActionError(
        err instanceof ApiError ? (err.detail ?? err.message) : 'Update failed',
      );
    } finally {
      setBusyId(null);
    }
  }

  async function handleDelete(alert: DashboardPriceAlert) {
    if (
      !confirm(
        `Delete the ${shortAsset(alert.base_asset)}/${shortAsset(alert.quote_asset)} alert? This can't be undone.`,
      )
    ) {
      return;
    }
    setBusyId(alert.id);
    setActionError(null);
    try {
      await deletePriceAlert(alert.id);
      setNotice('Alert deleted.');
      await refresh();
    } catch (err) {
      setActionError(
        err instanceof ApiError ? (err.detail ?? err.message) : 'Delete failed',
      );
    } finally {
      setBusyId(null);
    }
  }

  const enabledCount = alerts?.filter((a) => a.enabled).length ?? 0;

  return (
    <Container>
      <Section className="space-y-6">
        <PageHeader
          eyebrow="Notifications"
          title="Price alerts"
          description="Get notified when a pair crosses a threshold — delivered to your webhooks as a price.alert event."
          actions={
            !showForm && (
              <Button onClick={() => setShowForm(true)}>
                <Plus className="h-4 w-4" />
                New alert
              </Button>
            )
          }
        />

        <DeliveryNote />

        {notice && (
          <Callout tone="ok" title="Done">
            {notice}
          </Callout>
        )}

        {showForm && (
          <CreateAlertForm
            creating={creating}
            setCreating={setCreating}
            onError={setActionError}
            onCreated={() => {
              setNotice('Price alert created.');
              setShowForm(false);
              void refresh();
            }}
            onCancel={() => setShowForm(false)}
          />
        )}

        {error && (
          <Callout tone="bad" title="Something went wrong">
            {error}
          </Callout>
        )}

        {alerts === null && !error ? (
          <Card>
            <CardBody className="space-y-3">
              {Array.from({ length: 3 }).map((_, i) => (
                <Skeleton key={i} className="h-12 w-full" />
              ))}
            </CardBody>
          </Card>
        ) : alerts && alerts.length === 0 && !showForm ? (
          <EmptyState
            icon={<BellRing className="h-5 w-5" />}
            title="No price alerts yet"
            description="Create an alert to be notified when a pair crosses a threshold. Alerts are delivered to your account's webhooks subscribed to the price.alert event."
            action={
              <Button onClick={() => setShowForm(true)}>
                <Plus className="h-4 w-4" />
                Create your first alert
              </Button>
            }
          />
        ) : alerts && alerts.length > 0 ? (
          <AlertsTable
            alerts={alerts}
            busyId={busyId}
            onToggle={handleToggle}
            onDelete={handleDelete}
          />
        ) : null}

        {alerts && alerts.length > 0 && (
          <p className="text-xs text-ink-faint">
            {fmtInt(enabledCount)} enabled{' '}
            {enabledCount === 1 ? 'alert' : 'alerts'}
            {alerts.length > enabledCount &&
              `, ${fmtInt(alerts.length - enabledCount)} paused`}
            . Paused alerts stay configured but never fire.
          </p>
        )}
      </Section>
    </Container>
  );
}

// ─── Delivery note ─────────────────────────────────────────────────

// Alerts have no delivery of their own — they enqueue a `price.alert`
// webhook to the account's subscribed webhooks. Surface that dependency
// prominently so an operator doesn't create an alert and wonder why
// nothing arrives.
function DeliveryNote() {
  return (
    <Callout tone="info" title="How alerts are delivered">
      A firing alert is sent to your account&apos;s webhooks as a{' '}
      <code className="rounded-sm bg-surface-subtle px-1 py-0.5 font-mono text-[12px]">
        price.alert
      </code>{' '}
      event. Point a webhook at that event to receive alerts —{' '}
      <a
        className="font-medium underline"
        href={`${DOCS_URL}`}
        target="_blank"
        rel="noopener noreferrer"
      >
        see the webhooks docs
      </a>
      .
    </Callout>
  );
}

// ─── Create form ───────────────────────────────────────────────────

const THRESHOLD_RE = /^\d+(\.\d+)?$/;

function CreateAlertForm({
  creating,
  setCreating,
  onCreated,
  onCancel,
  onError,
}: {
  creating: boolean;
  setCreating: (b: boolean) => void;
  onCreated: () => void;
  onCancel: () => void;
  onError: (msg: string) => void;
}) {
  const [baseAsset, setBaseAsset] = useState('native');
  const [quoteAsset, setQuoteAsset] = useState('fiat:USD');
  const [condition, setCondition] = useState<'above' | 'below'>('above');
  const [threshold, setThreshold] = useState('');
  const [cooldown, setCooldown] = useState(300);
  const [fieldError, setFieldError] = useState<{
    base?: string;
    quote?: string;
    threshold?: string;
  }>({});

  async function submit(e: React.FormEvent) {
    e.preventDefault();
    if (creating) return;
    const errs: typeof fieldError = {};
    if (!baseAsset.trim()) errs.base = 'Base asset is required.';
    if (!quoteAsset.trim()) errs.quote = 'Quote asset is required.';
    if (!THRESHOLD_RE.test(threshold.trim()) || Number(threshold) <= 0) {
      errs.threshold =
        'Enter a positive decimal (e.g. 0.15 or 1200) — no scientific notation.';
    }
    if (Object.keys(errs).length > 0) {
      setFieldError(errs);
      return;
    }
    setCreating(true);
    try {
      const body: CreatePriceAlertRequest = {
        base_asset: baseAsset.trim(),
        quote_asset: quoteAsset.trim(),
        condition,
        threshold: threshold.trim(),
        cooldown_seconds: cooldown,
      };
      await createPriceAlert(body);
      onCreated();
    } catch (err) {
      onError(
        err instanceof ApiError ? (err.detail ?? err.message) : 'Create failed',
      );
    } finally {
      setCreating(false);
    }
  }

  return (
    <Card>
      <CardHeader
        title="New price alert"
        description="Fires when the pair's price crosses your threshold."
      />
      <form onSubmit={submit}>
        <CardBody className="space-y-5">
          <div className="grid grid-cols-1 gap-5 sm:grid-cols-2">
            <Field
              label="Base asset"
              htmlFor="alert-base"
              required
              hint="Canonical id — native (XLM), a C… contract, or CODE-G… strkey."
              error={fieldError.base}
            >
              <Input
                id="alert-base"
                value={baseAsset}
                onChange={(e) => {
                  setBaseAsset(e.target.value);
                  if (fieldError.base) setFieldError((f) => ({ ...f, base: undefined }));
                }}
                placeholder="native"
                className="font-mono text-[13px]"
              />
            </Field>
            <Field
              label="Quote asset"
              htmlFor="alert-quote"
              required
              hint="Canonical id or fiat:<ISO> — e.g. fiat:USD, native."
              error={fieldError.quote}
            >
              <Input
                id="alert-quote"
                value={quoteAsset}
                onChange={(e) => {
                  setQuoteAsset(e.target.value);
                  if (fieldError.quote)
                    setFieldError((f) => ({ ...f, quote: undefined }));
                }}
                placeholder="fiat:USD"
                className="font-mono text-[13px]"
              />
            </Field>
          </div>

          <div className="grid grid-cols-1 gap-5 sm:grid-cols-2">
            <Field
              label="Condition"
              htmlFor="alert-condition"
              hint="Fires when the observed price crosses the threshold."
            >
              <Select
                id="alert-condition"
                value={condition}
                onChange={(e) =>
                  setCondition(e.target.value as 'above' | 'below')
                }
              >
                <option value="above">Price rises to or above</option>
                <option value="below">Price falls to or below</option>
              </Select>
            </Field>
            <Field
              label="Threshold"
              htmlFor="alert-threshold"
              required
              hint="Price in the quote asset (decimal)."
              error={fieldError.threshold}
            >
              <Input
                id="alert-threshold"
                inputMode="decimal"
                value={threshold}
                onChange={(e) => {
                  setThreshold(e.target.value);
                  if (fieldError.threshold)
                    setFieldError((f) => ({ ...f, threshold: undefined }));
                }}
                placeholder="0.25"
                className="tnum font-mono text-[13px]"
              />
            </Field>
          </div>

          <Field
            label="Cooldown (seconds)"
            htmlFor="alert-cooldown"
            hint="Minimum seconds between two fires. 0 re-fires every tick the condition holds."
          >
            <Input
              id="alert-cooldown"
              type="number"
              min={0}
              value={cooldown}
              onChange={(e) => setCooldown(Math.max(0, Number(e.target.value) || 0))}
              className="tnum max-w-48"
            />
          </Field>
        </CardBody>
        <CardFooter className="justify-end gap-2">
          <Button
            type="button"
            variant="secondary"
            onClick={onCancel}
            disabled={creating}
          >
            Cancel
          </Button>
          <Button type="submit" disabled={creating}>
            {creating && <Loader2 className="h-4 w-4 animate-spin" />}
            {creating ? 'Creating…' : 'Create alert'}
          </Button>
        </CardFooter>
      </form>
    </Card>
  );
}

// ─── Alerts table ──────────────────────────────────────────────────

function AlertsTable({
  alerts,
  busyId,
  onToggle,
  onDelete,
}: {
  alerts: DashboardPriceAlert[];
  busyId: string | null;
  onToggle: (a: DashboardPriceAlert) => void;
  onDelete: (a: DashboardPriceAlert) => void;
}) {
  return (
    <TableWrap>
      <Table>
        <THead>
          <tr>
            <Th>Pair</Th>
            <Th>Condition</Th>
            <Th>Cooldown</Th>
            <Th>Last fired</Th>
            <Th>Status</Th>
            <Th align="right">Actions</Th>
          </tr>
        </THead>
        <TBody>
          {alerts.map((a) => {
            const busy = busyId === a.id;
            return (
              <TR key={a.id} className={a.enabled ? undefined : 'opacity-60'}>
                <Td>
                  <span className="font-mono text-[13px] text-ink">
                    {shortAsset(a.base_asset)}
                    <span className="text-ink-faint">/</span>
                    {shortAsset(a.quote_asset)}
                  </span>
                </Td>
                <Td>
                  <span className="tnum">
                    {a.condition === 'above' ? '≥' : '≤'} {a.threshold}{' '}
                    <span className="text-ink-muted">
                      {shortAsset(a.quote_asset)}
                    </span>
                  </span>
                </Td>
                <Td>{fmtCooldown(a.cooldown_seconds)}</Td>
                <Td>
                  <span title={a.last_fired_at ?? undefined}>
                    {a.last_fired_at ? fmtRelative(a.last_fired_at) : 'never'}
                  </span>
                </Td>
                <Td>
                  {a.enabled ? (
                    <Badge tone="ok" dot>
                      Enabled
                    </Badge>
                  ) : (
                    <Badge tone="neutral" dot>
                      Paused
                    </Badge>
                  )}
                </Td>
                <Td align="right">
                  <div className="flex items-center justify-end gap-1">
                    <Button
                      variant="ghost"
                      size="sm"
                      className="text-ink-muted"
                      onClick={() => onToggle(a)}
                      disabled={busy}
                    >
                      {busy ? (
                        <Loader2 className="h-3.5 w-3.5 animate-spin" />
                      ) : a.enabled ? (
                        <Pause className="h-3.5 w-3.5" />
                      ) : (
                        <Play className="h-3.5 w-3.5" />
                      )}
                      {a.enabled ? 'Pause' : 'Enable'}
                    </Button>
                    <Button
                      variant="ghost"
                      size="sm"
                      className="text-ink-muted hover:bg-bad-50 hover:text-bad-700"
                      onClick={() => onDelete(a)}
                      disabled={busy}
                    >
                      <Trash2 className="h-3.5 w-3.5" />
                      Delete
                    </Button>
                  </div>
                </Td>
              </TR>
            );
          })}
        </TBody>
      </Table>
    </TableWrap>
  );
}

// ─── Helpers ───────────────────────────────────────────────────────

// shortAsset renders a canonical asset id compactly for the table:
// native → XLM, fiat:USD → USD, crypto:BTC → BTC, CODE-G… → CODE, a
// bare C… contract → C…4-char…4-char.
function shortAsset(id: string): string {
  if (id === 'native') return 'XLM';
  if (id.startsWith('fiat:')) return id.slice(5);
  if (id.startsWith('crypto:')) return id.slice(7);
  const dash = id.indexOf('-');
  if (dash > 0) return id.slice(0, dash);
  if (id.length > 12) return `${id.slice(0, 4)}…${id.slice(-4)}`;
  return id;
}

// fmtCooldown renders the cooldown seconds as a coarse human string.
function fmtCooldown(seconds: number): string {
  if (!seconds || seconds <= 0) return 'every tick';
  if (seconds < 60) return `${seconds}s`;
  const min = Math.round(seconds / 60);
  if (min < 60) return `${min}m`;
  const hr = Math.round(min / 60);
  if (hr < 24) return `${hr}h`;
  const day = Math.round(hr / 24);
  return `${day}d`;
}
