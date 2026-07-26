// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1_test

import (
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus/testutil"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/auth"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/platform"
)

// C3-067 (audit-2026-07-23).
//
// Every privileged mutation on this surface appends its audit row
// best-effort: the mutation is already committed when the append runs, so a
// failure is swallowed with a bare `logger.Warn(... "(best-effort)")`. That
// choice is right — un-doing a committed tier change because the audit DB
// blipped would be worse — but it means an account tier, an API credential,
// or a public status notice can change with NO durable record of who did it
// or why, and the only trace is one log line.
//
// These tests pin the observable signal that closes that hole: each surface
// increments `stellarindex_admin_audit_write_failures_total` with ITS OWN
// label when (and only when) its audit append fails. The label matters as
// much as the count — an operator reconstructing the missing rows from
// application logs needs to know which mutation class to go looking for.
//
// Deliberately asserted per-surface rather than in aggregate: a single
// increment site wired into a shared helper would satisfy a total-only
// assertion while leaving five surfaces silent.

func auditFailureCount(t *testing.T, surface string) float64 {
	t.Helper()
	return testutil.ToFloat64(obs.AdminAuditWriteFailuresTotal.WithLabelValues(surface))
}

// TestAdminAuditWriteFailure_KeyMint — an operator mint whose audit row is
// lost leaves a LIVE credential with no record of who minted it.
func TestAdminAuditWriteFailure_KeyMint(t *testing.T) {
	before := auditFailureCount(t, "key_mint")

	store := &fakeAccountStore{
		rec: auth.APIKeyRecord{
			KeyID:     "kid_minted01",
			KeyPrefix: "sip_deadbeef",
			Label:     "partner-integration",
			Scopes:    []string{"read"},
			CreatedAt: time.Unix(1751000000, 0).UTC(),
		},
		plain: "sip_deadbeefcafe",
	}
	sink := &recordingAuditSink{err: errors.New("simulated audit-db blip")}
	ts := newAdminTestServer(t, operatorSubject(), store, sink)

	resp := postJSON(t, ts.URL+"/v1/admin/keys",
		`{"identifier":"acct:partner-co","label":"partner-integration","scopes":["read"],"rate_limit_per_min":5000}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (an audit failure must not block the mint)", resp.StatusCode)
	}

	if got, want := auditFailureCount(t, "key_mint"), before+1; got != want {
		t.Errorf("admin_audit_write_failures_total{surface=\"key_mint\"} = %v, want %v", got, want)
	}
}

// TestAdminAuditWriteFailure_KeyMint_NotCountedWhenAuditSucceeds — the
// counter must be silent on the healthy path, or the alert built on it is
// permanently firing and therefore useless.
func TestAdminAuditWriteFailure_KeyMint_NotCountedWhenAuditSucceeds(t *testing.T) {
	before := auditFailureCount(t, "key_mint")

	store := &fakeAccountStore{
		rec:   auth.APIKeyRecord{KeyID: "kid_ok", KeyPrefix: "sip_ok", CreatedAt: time.Unix(1751000000, 0).UTC()},
		plain: "sip_okokok",
	}
	sink := &recordingAuditSink{} // no error
	ts := newAdminTestServer(t, operatorSubject(), store, sink)

	resp := postJSON(t, ts.URL+"/v1/admin/keys", `{"identifier":"acct:ok","label":"l"}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201", resp.StatusCode)
	}
	if len(sink.entries) != 1 {
		t.Fatalf("audit entries = %d, want 1 (precondition: the append ran)", len(sink.entries))
	}
	if got := auditFailureCount(t, "key_mint"); got != before {
		t.Errorf("successful audit append moved the failure counter: %v → %v", before, got)
	}
}

// TestAdminAuditWriteFailure_AccountOverride — a tier/quota override whose
// audit row is lost leaves no record of the operator, the reason, or the
// previous values.
func TestAdminAuditWriteFailure_AccountOverride(t *testing.T) {
	before := auditFailureCount(t, "account_override")

	acct := seededAccount()
	store := newFakePlatformAccountStore(acct)
	sink := &recordingAuditSink{err: errors.New("simulated audit-db blip")}
	ts := newAdminAccountServer(t, operatorSubject(), store, sink)

	resp := patchJSON(t, ts.URL+"/v1/admin/accounts/"+acct.ID.String(), "enterprise comp",
		`{"tier":"enterprise","rate_limit_per_min_override":50000}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (an audit failure must not block the override)", resp.StatusCode)
	}
	if store.updateCalls != 1 {
		t.Fatalf("Update called %d times, want 1 (precondition: the mutation committed)", store.updateCalls)
	}

	if got, want := auditFailureCount(t, "account_override"), before+1; got != want {
		t.Errorf("admin_audit_write_failures_total{surface=\"account_override\"} = %v, want %v", got, want)
	}
}

// TestAdminAuditWriteFailure_StatusNotice — a public status notice changed
// with no record of who published it.
func TestAdminAuditWriteFailure_StatusNotice(t *testing.T) {
	before := auditFailureCount(t, "status_notice")

	store := newFakeStatusNoticeStore()
	sink := &recordingAuditSink{err: errors.New("simulated audit-db blip")}
	ts := newStatusNoticeServer(t, operatorSubject(), store, sink)

	resp := postJSONWithReason(t, ts.URL+"/v1/admin/status-notices", "planned maintenance",
		`{"title":"Scheduled maintenance","body":"02:00-03:00 UTC aggregator restart","severity":"maintenance"}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (an audit failure must not block the notice)", resp.StatusCode)
	}

	if got, want := auditFailureCount(t, "status_notice"), before+1; got != want {
		t.Errorf("admin_audit_write_failures_total{surface=\"status_notice\"} = %v, want %v", got, want)
	}
}

// TestAdminAuditWriteFailure_StripePlanUpgrade — a paid account's plan
// changed with no audit row tying it to the Stripe event that caused it.
// The webhook still 200s (a retry storm would be worse), which is precisely
// why the counter is the only signal.
func TestAdminAuditWriteFailure_StripePlanUpgrade(t *testing.T) {
	before := auditFailureCount(t, "stripe_plan_upgrade")

	now := time.Now().UTC()
	mgr := &fakeStripeManager{
		keys: map[string][]auth.APIKeyRecord{
			"signup-audit-fail": {{KeyID: "kid_af", Identifier: "signup-audit-fail", Tier: auth.TierAPIKey, RateLimitPerMin: 1000}},
		},
	}
	audit := &recordingAuditSink{err: errors.New("simulated audit-db blip")}
	ts := newStripeTestServerWithOptions(t, mgr, nil, audit, now)
	body := `{"id":"evt_audit_fail","type":"checkout.session.completed","data":{"object":{"id":"cs_af","client_reference_id":"signup-audit-fail","payment_status":"paid","metadata":{"tier":"pro"}}}}`
	sig := stripeSign(t, body, testStripeSecret, now)

	resp := postStripe(t, ts, body, sig)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (audit failure must not block the ack)", resp.StatusCode)
	}

	if got, want := auditFailureCount(t, "stripe_plan_upgrade"), before+1; got != want {
		t.Errorf("admin_audit_write_failures_total{surface=\"stripe_plan_upgrade\"} = %v, want %v", got, want)
	}
}

// TestAdminKeyBudgetClampCounter is the queued admin-clamp metric
// (audit-2026-07-23 one-liner).
//
// `clampKeyBudgetsToTier` silently lowers every credential an account can
// still authenticate with. The clamp is correct — the account no longer pays
// for the throughput — but from the customer's side an unchanged key simply
// starts 429-ing sooner, and the only prior trace was one INFO line per
// account. Deliberately NOT alerted (downgrades are routine); the point is
// that the rate is visible, so support can tie "my key started rate-limiting"
// to a billing event and a mass-clamp from a bad tier map is not invisible.
//
// Asserts it counts CREDENTIALS, not clamp calls: this demotion lowers one
// Postgres-backed and one Redis-backed key, so the counter must move by 2.
func TestAdminKeyBudgetClampCounter(t *testing.T) {
	before := testutil.ToFloat64(obs.AdminKeyBudgetClampsTotal.WithLabelValues("lowered"))
	failedBefore := testutil.ToFloat64(obs.AdminKeyBudgetClampsTotal.WithLabelValues("failed"))

	acctID := uuid.MustParse("33333333-3333-3333-3333-333333333333")
	acct := platform.Account{
		ID:     acctID,
		Name:   "Clamp Metric Corp",
		Slug:   "clampmetric",
		Tier:   platform.TierPro,
		Status: platform.AccountActive,
	}
	accounts := newFakePlatformAccountStore(acct)
	pgKeys := &fakePlatformAPIKeysForBridge{byAcct: map[uuid.UUID][]platform.APIKey{
		acctID: {
			{ID: "pg_hot", AccountID: acctID, RateLimitPerMin: 10_000, KeyHash: []byte{0xab}},
			{ID: "pg_low", AccountID: acctID, RateLimitPerMin: 30}, // already under the ceiling
		},
	}}
	ident := auth.AccountIdentifier("clampmetric")
	redisKeys := &fakeStripeManager{keys: map[string][]auth.APIKeyRecord{
		ident: {{KeyID: "rk_hot", Identifier: ident, RateLimitPerMin: 10_000}},
	}}

	ts := newAdminClampServer(t, accounts, v1.APIKeyBudgetStores{
		Platform: pgKeys,
		Redis:    redisKeys,
	}, &recordingAuditSink{})

	resp := patchJSON(t, ts.URL+"/v1/admin/accounts/"+acctID.String(),
		"abuse: demote to free", `{"tier":"free"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	if got, want := testutil.ToFloat64(obs.AdminKeyBudgetClampsTotal.WithLabelValues("lowered")), before+2; got != want {
		t.Errorf("admin_key_budget_clamps_total{outcome=\"lowered\"} = %v, want %v "+
			"(one per CREDENTIAL lowered: pg_hot + rk_hot; pg_low was already under the ceiling)", got, want)
	}
	if got := testutil.ToFloat64(obs.AdminKeyBudgetClampsTotal.WithLabelValues("failed")); got != failedBefore {
		t.Errorf("a clean clamp moved the failed counter: %v → %v", failedBefore, got)
	}
}

// TestAdminAuditWriteFailure_KeyRevoke — a revoked credential with no record
// of who killed it or why. The kill-switch surface is the one an incident
// responder reaches for, so its audit row is the one a post-mortem needs.
func TestAdminAuditWriteFailure_KeyRevoke(t *testing.T) {
	before := auditFailureCount(t, "key_revoke")

	store := &recordingRevokeStore{}
	sink := &recordingAuditSink{err: errors.New("simulated audit-db blip")}
	ts := newAdminKeyServer(t, auth.Subject{
		Identifier: "ops", Tier: auth.TierOperator, KeyID: "kid_ops",
	}, store, sink)

	resp := adminDelete(t, ts.URL+"/v1/admin/keys/kid_leaked?identifier=signup-victim",
		"key posted to a public gist")
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204 (an audit failure must not block the revoke)", resp.StatusCode)
	}
	if store.calls != 1 {
		t.Fatalf("RevokeKeyByID calls = %d, want 1 (precondition: the mutation committed)", store.calls)
	}

	if got, want := auditFailureCount(t, "key_revoke"), before+1; got != want {
		t.Errorf("admin_audit_write_failures_total{surface=\"key_revoke\"} = %v, want %v", got, want)
	}
}

// TestAdminAuditWriteFailure_StripeDeadLetter — the worst one. Money landed,
// nothing was provisioned, and the audit row recording THAT conclusion is
// also missing. The durable dead-letter row still lands (that is C3-016's
// job); this counter covers the accountability trail on top of it.
func TestAdminAuditWriteFailure_StripeDeadLetter(t *testing.T) {
	before := auditFailureCount(t, "stripe_dead_letter")

	now := time.Now().UTC()
	mgr := &fakeStripeManager{keys: map[string][]auth.APIKeyRecord{}} // identifier holds nothing
	events := newFakeStripeEventStore()
	audit := &recordingAuditSink{err: errors.New("simulated audit-db blip")}
	ts := newStripeTestServerWithOptions(t, mgr, events, audit, now)

	body := `{"id":"evt_dl_auditfail","type":"checkout.session.completed","data":{"object":` +
		`{"id":"cs_dl","client_reference_id":"signup-ghost","payment_status":"paid","metadata":{"tier":"pro"}}}}`
	resp := postStripe(t, ts, body, stripeSign(t, body, testStripeSecret, now))
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	// Precondition: this really is the dead-letter path.
	if _, ok := events.openDeadLetters()["evt_dl_auditfail"]; !ok {
		t.Fatalf("event was not dead-lettered; the test is not exercising the intended path")
	}

	if got, want := auditFailureCount(t, "stripe_dead_letter"), before+1; got != want {
		t.Errorf("admin_audit_write_failures_total{surface=\"stripe_dead_letter\"} = %v, want %v", got, want)
	}
}
