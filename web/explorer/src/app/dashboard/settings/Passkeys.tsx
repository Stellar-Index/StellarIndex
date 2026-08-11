'use client';

import { useQuery, useQueryClient } from '@tanstack/react-query';
import { KeyRound, Loader2, Plus, Trash2 } from 'lucide-react';
import { useState } from 'react';

import {
  ApiError,
  beginPasskeyRegister,
  deletePasskey,
  finishPasskeyRegister,
  listPasskeys,
  type PasskeyCredential,
} from '@/api/account';
import {
  Badge,
  Button,
  Callout,
  Card,
  CardBody,
  CardHeader,
  EmptyState,
  Field,
  Input,
  Skeleton,
} from '@/components/ui';
import { fmtRelative } from '@/lib/account-format';
import {
  createPasskey,
  isCeremonyCancelled,
  supportsPasskeys,
  type ServerCreationOptions,
} from '@/lib/webauthn';

/**
 * Passkeys — list / add / remove WebAuthn credentials for the
 * signed-in user. Adding one runs the browser ceremony
 * (navigator.credentials.create) against /v1/auth/passkey/*; removal
 * is safe because email-code sign-in always remains as the fallback
 * door.
 */
export function Passkeys() {
  const queryClient = useQueryClient();
  const query = useQuery<PasskeyCredential[], Error>({
    queryKey: ['dashboard', 'passkeys'],
    queryFn: ({ signal }) => listPasskeys(signal),
  });
  const passkeys = query.data ?? null;

  const [name, setName] = useState('');
  const [adding, setAdding] = useState(false);
  const [removingId, setRemovingId] = useState<string | null>(null);
  const [error, setError] = useState<string | null>(null);

  const invalidate = () =>
    queryClient.invalidateQueries({ queryKey: ['dashboard', 'passkeys'] });

  async function onAdd() {
    setError(null);
    setAdding(true);
    try {
      const options = (await beginPasskeyRegister()) as ServerCreationOptions;
      const credential = await createPasskey(options);
      await finishPasskeyRegister(name.trim(), credential);
      setName('');
      await invalidate();
    } catch (err) {
      if (!isCeremonyCancelled(err)) {
        setError(
          err instanceof ApiError
            ? (err.detail ?? 'Adding the passkey failed — try again.')
            : 'Adding the passkey failed — try again.',
        );
      }
    } finally {
      setAdding(false);
    }
  }

  async function onRemove(id: string) {
    setError(null);
    setRemovingId(id);
    try {
      await deletePasskey(id);
      await invalidate();
    } catch (err) {
      setError(
        err instanceof ApiError
          ? (err.detail ?? 'Removing the passkey failed — try again.')
          : 'Removing the passkey failed — try again.',
      );
    } finally {
      setRemovingId(null);
    }
  }

  const supported = supportsPasskeys();

  return (
    <Card>
      <CardHeader
        title="Passkeys"
        description="Sign in with your device's biometrics or a security key instead of an email code."
      />
      <CardBody className="space-y-4">
        {error && <Callout tone="bad">{error}</Callout>}

        {query.isLoading ? (
          <div className="space-y-2">
            <Skeleton className="h-12 w-full" />
            <Skeleton className="h-12 w-full" />
          </div>
        ) : query.isError ? (
          <Callout tone="bad">
            Couldn&apos;t load your passkeys. Refresh to try again.
          </Callout>
        ) : passkeys && passkeys.length > 0 ? (
          <ul className="divide-y divide-line rounded-md border border-line">
            {passkeys.map((p) => (
              <li
                key={p.id}
                className="flex items-center justify-between gap-4 px-4 py-3"
              >
                <div className="flex min-w-0 items-center gap-3">
                  <KeyRound className="h-4 w-4 shrink-0 text-ink-faint" />
                  <div className="min-w-0">
                    <div className="truncate text-sm font-medium text-ink">
                      {p.name}
                    </div>
                    <div className="text-xs text-ink-muted">
                      Added {fmtRelative(p.created_at)}
                      {p.last_used_at
                        ? ` · last used ${fmtRelative(p.last_used_at)}`
                        : ' · never used'}
                    </div>
                  </div>
                  {p.backup_eligible && <Badge>synced</Badge>}
                </div>
                <Button
                  variant="ghost"
                  size="sm"
                  onClick={() => onRemove(p.id)}
                  disabled={removingId === p.id}
                  aria-label={`Remove passkey ${p.name}`}
                >
                  {removingId === p.id ? (
                    <Loader2 className="h-4 w-4 animate-spin" />
                  ) : (
                    <Trash2 className="h-4 w-4" />
                  )}
                </Button>
              </li>
            ))}
          </ul>
        ) : (
          <EmptyState
            icon={<KeyRound className="h-5 w-5" />}
            title="No passkeys yet"
            description="Add one to sign in without waiting for an email code."
          />
        )}

        {supported ? (
          <div className="flex flex-col gap-3 sm:flex-row sm:items-end">
            <Field
              label="Name"
              hint="Optional label, e.g. “MacBook Touch ID”."
              className="flex-1"
            >
              <Input
                value={name}
                maxLength={100}
                placeholder="This device"
                onChange={(e) => setName(e.target.value)}
              />
            </Field>
            <Button onClick={onAdd} disabled={adding}>
              {adding ? (
                <Loader2 className="h-4 w-4 animate-spin" />
              ) : (
                <Plus className="h-4 w-4" />
              )}
              Add a passkey
            </Button>
          </div>
        ) : (
          <p className="text-xs text-ink-muted">
            This browser doesn&apos;t support passkeys.
          </p>
        )}

        <p className="text-xs text-ink-faint">
          Email-code sign-in always remains available, so removing a
          passkey can&apos;t lock you out.
        </p>
      </CardBody>
    </Card>
  );
}
