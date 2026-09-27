'use client';

import { useQuery, useQueryClient } from '@tanstack/react-query';
import {
  ChevronDown,
  ChevronUp,
  Loader2,
  Plus,
  Trash2,
  Webhook as WebhookIcon,
} from 'lucide-react';
import { useCallback, useState } from 'react';

import {
  ApiError,
  createDashboardWebhook,
  deleteDashboardWebhook,
  listDashboardWebhooks,
  listWebhookDeliveries,
  updateDashboardWebhook,
  type CreateWebhookResponse,
  type CreateWebhookRequest,
  type DashboardWebhook,
  type WebhookDelivery,
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
  CopyButton,
  EmptyState,
  Field,
  Input,
  PageHeader,
  Section,
  Skeleton,
  Table,
  TableWrap,
  TBody,
  Td,
  Th,
  THead,
  TR,
} from '@/components/ui';
import { fmtDate } from '@/lib/account-format';

import { AccountGate } from '../AccountGate';

const EVENT_TYPES = [
  'price.alert',
  'incident.sev1',
  'incident.resolved',
  'anomaly.freeze',
  'divergence.firing',
] as const;

/**
 * /dashboard/webhooks — self-service webhook management. Same shape as
 * the API-keys and price-alerts pages (session-cookie CRUD via
 * @/api/account): a table of registered webhooks, a create form that
 * reveals the HMAC signing secret once, an enable/disable toggle
 * (PATCH), delete-with-confirm, and a per-webhook delivery log (GET
 * .../deliveries) so an operator can see why a webhook stopped firing.
 * Every account event (price alerts, incidents, anomaly freezes,
 * divergence firings) delivers ONLY to a webhook registered here.
 */
export default function WebhooksPage() {
  return <AccountGate>{() => <WebhooksBody />}</AccountGate>;
}

function WebhooksBody() {
  const queryClient = useQueryClient();
  const webhooksQuery = useQuery<DashboardWebhook[], Error>({
    queryKey: ['dashboard', 'webhooks'],
    queryFn: ({ signal }) => listDashboardWebhooks(signal),
  });
  const webhooks = webhooksQuery.data ?? null;

  const [actionError, setActionError] = useState<string | null>(null);
  const [notice, setNotice] = useState<string | null>(null);
  const [newWebhook, setNewWebhook] = useState<CreateWebhookResponse | null>(
    null,
  );
  const [showForm, setShowForm] = useState(false);
  const [creating, setCreating] = useState(false);
  const [busyId, setBusyId] = useState<string | null>(null);
  const [viewingId, setViewingId] = useState<string | null>(null);

  const loadError = webhooksQuery.error
    ? webhooksQuery.error instanceof ApiError
      ? (webhooksQuery.error.detail ?? webhooksQuery.error.message)
      : 'Failed to load webhooks'
    : null;
  const error = actionError ?? loadError;

  const refresh = useCallback(async () => {
    setActionError(null);
    await queryClient.invalidateQueries({
      queryKey: ['dashboard', 'webhooks'],
    });
  }, [queryClient]);

  async function handleToggle(webhook: DashboardWebhook) {
    setBusyId(webhook.id);
    setActionError(null);
    try {
      await updateDashboardWebhook(webhook.id, { enabled: !webhook.enabled });
      setNotice(`Webhook ${webhook.enabled ? 'paused' : 'enabled'}.`);
      await refresh();
    } catch (err) {
      setActionError(
        err instanceof ApiError ? (err.detail ?? err.message) : 'Update failed',
      );
    } finally {
      setBusyId(null);
    }
  }

  async function handleDelete(webhook: DashboardWebhook) {
    if (
      !confirm(`Delete the "${webhook.name}" webhook? This can't be undone.`)
    ) {
      return;
    }
    setBusyId(webhook.id);
    setActionError(null);
    try {
      await deleteDashboardWebhook(webhook.id);
      setNotice('Webhook deleted.');
      await refresh();
    } catch (err) {
      setActionError(
        err instanceof ApiError ? (err.detail ?? err.message) : 'Delete failed',
      );
    } finally {
      setBusyId(null);
    }
  }

  return (
    <Container>
      <Section className="space-y-6">
        <PageHeader
          eyebrow="Notifications"
          title="Webhooks"
          description="Register an HTTPS endpoint to receive price alerts, incidents, and anomaly events as signed JSON POSTs."
          actions={
            !showForm && (
              <Button onClick={() => setShowForm(true)}>
                <Plus className="h-4 w-4" />
                New webhook
              </Button>
            )
          }
        />

        {notice && (
          <Callout tone="ok" title="Done">
            {notice}
          </Callout>
        )}

        {newWebhook && (
          <NewWebhookReveal
            created={newWebhook}
            onDismiss={() => {
              setNewWebhook(null);
              void refresh();
            }}
          />
        )}

        {showForm && (
          <CreateWebhookForm
            creating={creating}
            setCreating={setCreating}
            onError={setActionError}
            onCreated={(resp) => {
              setNewWebhook(resp);
              setNotice(null);
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

        {webhooks === null && !error ? (
          <Card>
            <CardBody className="space-y-3">
              {Array.from({ length: 3 }).map((_, i) => (
                <Skeleton key={i} className="h-12 w-full" />
              ))}
            </CardBody>
          </Card>
        ) : webhooks && webhooks.length === 0 && !showForm ? (
          <EmptyState
            icon={<WebhookIcon className="h-5 w-5" />}
            title="No webhooks yet"
            description="Register an endpoint to start receiving price alerts and other account events."
            action={
              <Button onClick={() => setShowForm(true)}>
                <Plus className="h-4 w-4" />
                Create your first webhook
              </Button>
            }
          />
        ) : webhooks && webhooks.length > 0 ? (
          <WebhooksTable
            webhooks={webhooks}
            busyId={busyId}
            viewingId={viewingId}
            onToggle={handleToggle}
            onDelete={handleDelete}
            onToggleDeliveries={(id) =>
              setViewingId((cur) => (cur === id ? null : id))
            }
          />
        ) : null}
      </Section>
    </Container>
  );
}

// ─── Reveal banner (secret shown once) ─────────────────────────────

function NewWebhookReveal({
  created,
  onDismiss,
}: {
  created: CreateWebhookResponse;
  onDismiss: () => void;
}) {
  return (
    <Card className="border-brand-200 bg-brand-50/60">
      <CardHeader
        className="border-brand-100"
        eyebrow="New webhook created"
        title="Save this signing secret now — you won't see it again"
        description="Use it to verify the X-StellarIndex-Signature header on inbound deliveries."
      />
      <CardBody className="space-y-3">
        <div className="border-line bg-surface flex items-center gap-2 rounded-lg border px-3 py-2.5">
          <code className="text-ink min-w-0 flex-1 font-mono text-[13px] break-all">
            {created.secret}
          </code>
          <CopyButton value={created.secret} className="h-7 w-7" />
        </div>
        <p className="text-ink-muted text-xs">
          Registered at{' '}
          <code className="bg-surface-subtle rounded-sm px-1 py-0.5 font-mono break-all">
            {created.webhook.url}
          </code>
        </p>
      </CardBody>
      <CardFooter className="justify-end">
        <Button variant="primary" size="sm" onClick={onDismiss}>
          I&apos;ve saved it
        </Button>
      </CardFooter>
    </Card>
  );
}

// ─── Create form ───────────────────────────────────────────────────

function CreateWebhookForm({
  creating,
  setCreating,
  onCreated,
  onCancel,
  onError,
}: {
  creating: boolean;
  setCreating: (b: boolean) => void;
  onCreated: (r: CreateWebhookResponse) => void;
  onCancel: () => void;
  onError: (msg: string) => void;
}) {
  const [name, setName] = useState('');
  const [url, setUrl] = useState('');
  const [events, setEvents] = useState<Set<string>>(new Set(['price.alert']));
  const [nameError, setNameError] = useState<string | null>(null);
  const [urlError, setUrlError] = useState<string | null>(null);

  function toggleEvent(event: string) {
    setEvents((prev) => {
      const next = new Set(prev);
      if (next.has(event)) next.delete(event);
      else next.add(event);
      return next;
    });
  }

  async function submit(e: React.FormEvent) {
    e.preventDefault();
    if (creating) return;
    let hasError = false;
    if (!name.trim()) {
      setNameError('Give the webhook a name so you can find it later.');
      hasError = true;
    }
    if (!url.trim().startsWith('https://')) {
      setUrlError('Must be an https:// endpoint.');
      hasError = true;
    }
    if (events.size === 0) {
      onError('Select at least one event to subscribe to.');
      hasError = true;
    }
    if (hasError) return;

    setCreating(true);
    try {
      const body: CreateWebhookRequest = {
        name: name.trim(),
        url: url.trim(),
        events: [...events] as CreateWebhookRequest['events'],
      };
      const resp = await createDashboardWebhook(body);
      onCreated(resp);
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
        title="New webhook"
        description="We POST a signed JSON body to this endpoint for every subscribed event."
      />
      <form onSubmit={submit}>
        <CardBody className="space-y-5">
          <Field
            label="Name"
            htmlFor="webhook-name"
            required
            hint="Helps you identify this webhook in the list later."
            error={nameError ?? undefined}
          >
            <Input
              id="webhook-name"
              autoFocus
              value={name}
              onChange={(e) => {
                setName(e.target.value);
                if (nameError) setNameError(null);
              }}
              placeholder="production-alerts"
            />
          </Field>

          <Field
            label="Endpoint URL"
            htmlFor="webhook-url"
            required
            hint="Must be https://."
            error={urlError ?? undefined}
          >
            <Input
              id="webhook-url"
              value={url}
              onChange={(e) => {
                setUrl(e.target.value);
                if (urlError) setUrlError(null);
              }}
              placeholder="https://example.com/hooks/stellarindex"
            />
          </Field>

          <Field label="Events" htmlFor="webhook-events">
            <div id="webhook-events" className="flex flex-col gap-2">
              {EVENT_TYPES.map((event) => (
                <label
                  key={event}
                  className="text-ink-body inline-flex items-center gap-2 text-sm select-none"
                >
                  <input
                    type="checkbox"
                    checked={events.has(event)}
                    onChange={() => toggleEvent(event)}
                    className="border-line-strong text-brand-600 focus:ring-brand-500 h-3.5 w-3.5 rounded-sm"
                  />
                  <code className="bg-surface-subtle rounded-sm px-1 py-0.5 font-mono text-[12px]">
                    {event}
                  </code>
                </label>
              ))}
            </div>
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
            {creating ? 'Creating…' : 'Create webhook'}
          </Button>
        </CardFooter>
      </form>
    </Card>
  );
}

// ─── Webhooks table ────────────────────────────────────────────────

function WebhooksTable({
  webhooks,
  busyId,
  viewingId,
  onToggle,
  onDelete,
  onToggleDeliveries,
}: {
  webhooks: DashboardWebhook[];
  busyId: string | null;
  viewingId: string | null;
  onToggle: (w: DashboardWebhook) => void;
  onDelete: (w: DashboardWebhook) => void;
  onToggleDeliveries: (id: string) => void;
}) {
  return (
    <TableWrap>
      <Table>
        <THead>
          <tr>
            <Th>Name</Th>
            <Th>Endpoint</Th>
            <Th>Events</Th>
            <Th>Created</Th>
            <Th>Status</Th>
            <Th align="right">Actions</Th>
          </tr>
        </THead>
        <TBody>
          {webhooks.map((w) => {
            const busy = busyId === w.id;
            const viewing = viewingId === w.id;
            return (
              <>
                <TR
                  key={w.id}
                  className={!w.enabled ? 'opacity-60' : undefined}
                >
                  <Td>
                    <div className="text-ink font-medium">{w.name}</div>
                  </Td>
                  <Td>
                    <code className="text-ink-body max-w-64 truncate font-mono text-[13px] break-all">
                      {w.url}
                    </code>
                  </Td>
                  <Td>
                    <div className="flex flex-wrap gap-1">
                      {w.events.map((e) => (
                        <span
                          key={e}
                          className="bg-surface-subtle text-ink-muted rounded-sm px-1 py-0.5 font-mono text-[11px]"
                        >
                          {e}
                        </span>
                      ))}
                    </div>
                  </Td>
                  <Td>{fmtDate(w.created_at)}</Td>
                  <Td>
                    {w.enabled ? (
                      <Badge tone="ok" dot>
                        Enabled
                      </Badge>
                    ) : (
                      <Badge tone="warn" dot>
                        Paused
                      </Badge>
                    )}
                  </Td>
                  <Td align="right">
                    <div className="flex justify-end gap-1">
                      <Button
                        variant="ghost"
                        size="sm"
                        className="text-ink-muted"
                        onClick={() => onToggleDeliveries(w.id)}
                      >
                        {viewing ? (
                          <ChevronUp className="h-3.5 w-3.5" />
                        ) : (
                          <ChevronDown className="h-3.5 w-3.5" />
                        )}
                        Log
                      </Button>
                      <Button
                        variant="ghost"
                        size="sm"
                        className="text-ink-muted"
                        onClick={() => onToggle(w)}
                        disabled={busy}
                      >
                        {busy ? (
                          <Loader2 className="h-3.5 w-3.5 animate-spin" />
                        ) : w.enabled ? (
                          'Pause'
                        ) : (
                          'Enable'
                        )}
                      </Button>
                      <Button
                        variant="ghost"
                        size="sm"
                        className="text-ink-muted hover:bg-bad-50 hover:text-bad-700"
                        onClick={() => onDelete(w)}
                        disabled={busy}
                      >
                        <Trash2 className="h-3.5 w-3.5" />
                        Delete
                      </Button>
                    </div>
                  </Td>
                </TR>
                {viewing && (
                  <TR key={`${w.id}-deliveries`}>
                    <Td colSpan={6} className="bg-surface-subtle/40">
                      <DeliveryLog webhookId={w.id} />
                    </Td>
                  </TR>
                )}
              </>
            );
          })}
        </TBody>
      </Table>
    </TableWrap>
  );
}

// ─── Delivery log (per-webhook) ────────────────────────────────────

function deliveryStatus(d: WebhookDelivery): {
  label: string;
  tone: 'ok' | 'warn' | 'bad';
} {
  if (d.delivered_at) return { label: 'Delivered', tone: 'ok' };
  if (d.next_attempt_at) return { label: 'Retrying', tone: 'warn' };
  return { label: 'Failed', tone: 'bad' };
}

function DeliveryLog({ webhookId }: { webhookId: string }) {
  const query = useQuery<WebhookDelivery[], Error>({
    queryKey: ['dashboard', 'webhooks', webhookId, 'deliveries'],
    queryFn: ({ signal }) => listWebhookDeliveries(webhookId, signal),
  });

  if (query.isLoading) {
    return <Skeleton className="h-16 w-full" />;
  }
  if (query.isError) {
    return (
      <p className="text-bad-700 text-xs">
        {query.error instanceof ApiError
          ? (query.error.detail ?? query.error.message)
          : 'Failed to load delivery log'}
      </p>
    );
  }
  const deliveries = query.data ?? [];
  if (deliveries.length === 0) {
    return (
      <p className="text-ink-muted text-xs">
        No deliveries yet — nothing has fired to this webhook.
      </p>
    );
  }
  const delivered = deliveries.filter((d) => d.delivered_at).length;
  const successRate = Math.round((delivered / deliveries.length) * 100);

  return (
    <div className="space-y-2">
      <p className="text-ink-muted text-xs">
        {successRate}% delivered over the last {deliveries.length} attempt
        {deliveries.length === 1 ? '' : 's'}.
      </p>
      <TableWrap>
        <Table>
          <THead>
            <tr>
              <Th>Event</Th>
              <Th>Attempts</Th>
              <Th>Status</Th>
              <Th>Next retry</Th>
              <Th>Delivered</Th>
              <Th>Last error</Th>
            </tr>
          </THead>
          <TBody>
            {deliveries.map((d) => {
              const status = deliveryStatus(d);
              return (
                <TR key={d.id}>
                  <Td>
                    <code className="font-mono text-[11px]">
                      {d.event_type}
                    </code>
                  </Td>
                  <Td align="right">{d.attempt_count}</Td>
                  <Td>
                    <Badge tone={status.tone} dot>
                      {status.label}
                    </Badge>
                  </Td>
                  <Td>{fmtDate(d.next_attempt_at)}</Td>
                  <Td>{fmtDate(d.delivered_at)}</Td>
                  <Td>
                    {d.last_error ? (
                      <span className="text-bad-700 text-xs">
                        {d.last_error}
                        {d.last_response_status
                          ? ` (HTTP ${d.last_response_status})`
                          : ''}
                      </span>
                    ) : (
                      '—'
                    )}
                  </Td>
                </TR>
              );
            })}
          </TBody>
        </Table>
      </TableWrap>
    </div>
  );
}
