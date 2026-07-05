-- Migration 0080 — price_alerts
--
-- BACKLOG #60 (RFP §6, accommodation): customer-configurable
-- price-threshold alerts. A customer registers, per account, a rule
-- "notify me when <base>/<quote> goes <above|below> <threshold>". The
-- aggregator's price-alert evaluator (internal/pricealerts) checks each
-- enabled row against the latest CLOSED 1-minute VWAP every tick and,
-- when the condition holds and the cooldown has elapsed, enqueues a
-- `price.alert` delivery into the existing customer-webhook queue
-- (webhook_deliveries) for that account's subscribed webhooks. The
-- delivery worker (internal/customerwebhook, API binary) then HMAC-signs
-- + POSTs it — no worker change was needed, the queue is event-type
-- agnostic.
--
-- Owner-scoped by account_id (same FK shape as customer_webhooks in
-- migration 0027) so the evaluator enqueues ACCOUNT-scoped deliveries and
-- one account's alerts never leak to another account's webhook
-- subscribers.
--
-- ADR-0003: `threshold` is NUMERIC (a price is an i128-derived amount
-- ratio — never bigint / double). Accepted + served as a decimal string
-- on the wire.
--
-- Additive + old-binary-safe (migrations/README rule 9): a brand-new
-- table + indexes. No released binary reads or writes it, so applying
-- this ahead of the new binaries is a no-op for them; the deploy
-- pipeline's migrate-up-before-install ordering is safe.

CREATE TABLE price_alerts (
    id               uuid PRIMARY KEY DEFAULT uuid_generate_v4(),
    account_id       uuid NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    -- Canonical wire-form asset ids (migrations/README rule 6): `native`,
    -- `CODE-ISSUER` for classic, `C…` for Soroban, `fiat:USD` for fiat.
    -- Stored verbatim; the evaluator parses them via canonical.ParseAsset.
    base_asset       text NOT NULL CHECK (length(base_asset) BETWEEN 1 AND 120),
    quote_asset      text NOT NULL CHECK (length(quote_asset) BETWEEN 1 AND 120),
    -- 'above' fires when observed >= threshold; 'below' when observed <= threshold.
    condition        text NOT NULL CHECK (condition IN ('above', 'below')),
    -- Price boundary as an arbitrary-precision decimal (ADR-0003).
    threshold        numeric NOT NULL CHECK (threshold > 0),
    -- Minimum wall-clock seconds between two fires of the same alert.
    -- 0 = re-fire every tick the condition holds (noisy; the dashboard
    -- nudges customers toward a non-zero cooldown).
    cooldown_seconds int NOT NULL DEFAULT 0 CHECK (cooldown_seconds >= 0),
    enabled          boolean NOT NULL DEFAULT true,
    -- When the alert last enqueued a delivery. NULL = never fired. The
    -- evaluator reads this + cooldown_seconds to gate re-fire eligibility.
    last_fired_at    timestamptz,
    created_at       timestamptz NOT NULL DEFAULT now(),
    updated_at       timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX price_alerts_account_idx ON price_alerts (account_id);

-- The evaluator sweeps enabled rows every tick; a partial index keeps that
-- scan tight even as disabled rows accumulate.
CREATE INDEX price_alerts_enabled_idx ON price_alerts (enabled) WHERE enabled = true;
