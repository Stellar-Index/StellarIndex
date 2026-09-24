package dashboardauth

// A passkey is a first-factor credential, so adding or removing one — and
// a sign-in refused on a theft signal — must leave a durable audit_log row
// (and, for the refusals, a counter an alert can read), not just a log line.

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/platform"
)

const flagAttestedCredentialData = 0x40

func withAuditSink(rig *passkeyRig) *fakeAuditSink {
	sink := &fakeAuditSink{}
	rig.h.cfg.Audit = sink
	return sink
}

func onlyAuditEntry(t *testing.T, sink *fakeAuditSink) platform.AuditEntry {
	t.Helper()
	entries := sink.all()
	if len(entries) != 1 {
		t.Fatalf("audit rows = %d, want exactly 1 (%+v)", len(entries), entries)
	}
	return entries[0]
}

func auditMeta(t *testing.T, e platform.AuditEntry) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(e.Metadata, &m); err != nil {
		t.Fatalf("audit metadata %q: %v", e.Metadata, err)
	}
	return m
}

// storedCredential returns the rig user's single seeded passkey row.
func storedCredential(t *testing.T, rig *passkeyRig) platform.WebAuthnCredential {
	t.Helper()
	rows, err := rig.passkeys.ListWebAuthnCredentialsForUser(context.Background(), rig.user.ID)
	if err != nil || len(rows) != 1 {
		t.Fatalf("stored credentials = %d (err %v), want 1", len(rows), err)
	}
	return rows[0]
}

// attestationBody is the finish-register body a browser POSTs for a
// "none"-attestation registration of a's key under challenge.
func (a *softAuthenticator) attestationBody(t *testing.T, challenge, name string) string {
	t.Helper()
	clientData, err := json.Marshal(map[string]any{
		"type":      "webauthn.create",
		"challenge": challenge,
		"origin":    "https://" + testRPID,
	})
	if err != nil {
		t.Fatalf("marshal client data: %v", err)
	}
	authData := a.authData(flagUserPresent|flagUserVerified|flagAttestedCredentialData, 0)
	authData = append(authData, make([]byte, 16)...) // AAGUID
	authData = binary.BigEndian.AppendUint16(authData, uint16(len(a.credID)))
	authData = append(authData, a.credID...)
	authData = append(authData, a.cosePublicKey()...)
	if len(authData) > 0xff {
		t.Fatalf("authData %d bytes needs a longer CBOR length header", len(authData))
	}
	// {"fmt": "none", "attStmt": {}, "authData": bytes}
	att := []byte{0xa3, 0x63, 'f', 'm', 't', 0x64, 'n', 'o', 'n', 'e'}
	att = append(att, 0x67, 'a', 't', 't', 'S', 't', 'm', 't', 0xa0)
	att = append(att, 0x68, 'a', 'u', 't', 'h', 'D', 'a', 't', 'a', 0x58, byte(len(authData)))
	att = append(att, authData...)

	enc := base64.RawURLEncoding
	body, err := json.Marshal(map[string]any{
		"name": name,
		"credential": map[string]any{
			"id":    enc.EncodeToString(a.credID),
			"rawId": enc.EncodeToString(a.credID),
			"type":  "public-key",
			"response": map[string]any{
				"clientDataJSON":    enc.EncodeToString(clientData),
				"attestationObject": enc.EncodeToString(att),
			},
		},
	})
	if err != nil {
		t.Fatalf("marshal attestation: %v", err)
	}
	return string(body)
}

// TestPasskeyFinishRegister_WritesAuditRow drives a real registration and
// requires the passkey.register row: who added it, to which account, from
// where, in which session.
func TestPasskeyFinishRegister_WritesAuditRow(t *testing.T) {
	rig := newPasskeyRig(t)
	sink := withAuditSink(rig)
	auth := newSoftAuthenticator(t, rig.user.ID)
	sessionID := uuid.New()
	withSess := func(req *http.Request) *http.Request {
		return req.WithContext(WithSession(req.Context(), SessionContext{
			Session: platform.Session{ID: sessionID, UserID: rig.user.ID},
			User:    rig.user,
			Account: rig.account,
		}))
	}

	w := httptest.NewRecorder()
	rig.h.HandlePasskeyBeginRegister(w, withSess(httptest.NewRequest(http.MethodPost, "/v1/auth/passkey/begin-register", nil)))
	if w.Code != http.StatusOK {
		t.Fatalf("begin-register status = %d (%s)", w.Code, w.Body.String())
	}
	var opts struct {
		PublicKey struct {
			Challenge string `json:"challenge"`
		} `json:"publicKey"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &opts); err != nil {
		t.Fatalf("unmarshal options: %v", err)
	}
	cookie := ceremonyCookie(t, w)

	req := withSess(httptest.NewRequest(http.MethodPost, "/v1/auth/passkey/finish-register",
		strings.NewReader(auth.attestationBody(t, opts.PublicKey.Challenge, "Laptop"))))
	req.AddCookie(&http.Cookie{Name: PasskeyCeremonyCookieName, Value: cookie.Value})
	req.RemoteAddr = "198.51.100.7:4242"
	req.Header.Set("User-Agent", "Example/1.0")
	w = httptest.NewRecorder()
	rig.h.HandlePasskeyFinishRegister(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("finish-register status = %d, want 200 (%s)", w.Code, w.Body.String())
	}
	stored := storedCredential(t, rig)

	e := onlyAuditEntry(t, sink)
	if e.Action != AuditActionPasskeyRegister || e.ActorKind != platform.ActorUser ||
		e.ActorUserID != rig.user.ID || e.AccountID != rig.account.ID ||
		e.TargetKind != "webauthn_credential" || e.TargetID != stored.ID.String() {
		t.Fatalf("audit row = %+v, want passkey.register by the session user on credential %s", e, stored.ID)
	}
	if e.IP.String() != "198.51.100.7" || e.UserAgent != "Example/1.0" || e.Timestamp.IsZero() {
		t.Fatalf("audit row ip=%v ua=%q ts=%v, want request-derived provenance", e.IP, e.UserAgent, e.Timestamp)
	}
	meta := auditMeta(t, e)
	if meta["session_id"] != sessionID.String() || meta["name"] != "Laptop" {
		t.Fatalf("audit metadata = %v, want session_id and name", meta)
	}
}

// TestPasskeyDelete_WritesAuditRow — removing a first factor is at least
// as consequential as adding one.
func TestPasskeyDelete_WritesAuditRow(t *testing.T) {
	rig := newPasskeyRig(t)
	sink := withAuditSink(rig)
	mine, err := rig.passkeys.CreateWebAuthnCredential(context.Background(), platform.WebAuthnCredential{
		UserID:       rig.user.ID,
		Name:         "My key",
		CredentialID: []byte("EXAMPLE-CRED-ID-PLACEHOLDER-04"),
		PublicKey:    []byte("EXAMPLE-PUBKEY-PLACEHOLDER"),
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	// A refused delete (not mine) writes nothing.
	other := uuid.New()
	req := rig.withSession(httptest.NewRequest(http.MethodDelete, "/v1/auth/passkey/credentials/"+other.String(), nil))
	req.SetPathValue("id", other.String())
	w := httptest.NewRecorder()
	rig.h.HandlePasskeyDelete(w, req)
	if w.Code != http.StatusNotFound || len(sink.all()) != 0 {
		t.Fatalf("404 delete: status %d, audit rows %d — want 404 and none", w.Code, len(sink.all()))
	}

	req = rig.withSession(httptest.NewRequest(http.MethodDelete, "/v1/auth/passkey/credentials/"+mine.ID.String(), nil))
	req.SetPathValue("id", mine.ID.String())
	w = httptest.NewRecorder()
	rig.h.HandlePasskeyDelete(w, req)
	if w.Code != http.StatusNoContent {
		t.Fatalf("delete status = %d, want 204", w.Code)
	}
	e := onlyAuditEntry(t, sink)
	if e.Action != AuditActionPasskeyDelete || e.ActorKind != platform.ActorUser ||
		e.ActorUserID != rig.user.ID || e.AccountID != rig.account.ID || e.TargetID != mine.ID.String() {
		t.Fatalf("audit row = %+v, want passkey.delete by the session user on %s", e, mine.ID)
	}
}

// TestPasskeyDelete_AuditSinkFailureIsCounted — the delete stands (it has
// committed) but the lost row must be visible on the shared counter.
func TestPasskeyDelete_AuditSinkFailureIsCounted(t *testing.T) {
	rig := newPasskeyRig(t)
	rig.h.cfg.Audit = &fakeAuditSink{err: errors.New("audit store down")}
	mine, err := rig.passkeys.CreateWebAuthnCredential(context.Background(), platform.WebAuthnCredential{
		UserID:       rig.user.ID,
		CredentialID: []byte("EXAMPLE-CRED-ID-PLACEHOLDER-05"),
		PublicKey:    []byte("EXAMPLE-PUBKEY-PLACEHOLDER"),
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	failures := obs.AdminAuditWriteFailuresTotal.WithLabelValues("passkey_delete")
	before := testutil.ToFloat64(failures)

	req := rig.withSession(httptest.NewRequest(http.MethodDelete, "/v1/auth/passkey/credentials/"+mine.ID.String(), nil))
	req.SetPathValue("id", mine.ID.String())
	w := httptest.NewRecorder()
	rig.h.HandlePasskeyDelete(w, req)
	if w.Code != http.StatusNoContent {
		t.Fatalf("delete status = %d, want 204 despite the audit outage", w.Code)
	}
	if got := testutil.ToFloat64(failures) - before; got != 1 {
		t.Fatalf("passkey_delete audit write failures rose by %v, want 1", got)
	}
}

// TestPasskeyFinishLogin_CloneWarningRefusedAuditedAndCounted — a
// sign-counter regression is WebAuthn's one signal that the private key
// exists twice. The login must be refused, and the refusal must leave a
// durable row and move the counter the clone-warning alert reads.
func TestPasskeyFinishLogin_CloneWarningRefusedAuditedAndCounted(t *testing.T) {
	rig, auth, _ := newLiveClockPasskeyRig(t)
	sink := withAuditSink(rig)
	stored := storedCredential(t, rig)
	// The genuine authenticator has already signed at counter 5.
	if err := rig.passkeys.UpdateWebAuthnCredentialSignCount(context.Background(), stored.ID, 5, rig.now()); err != nil {
		t.Fatalf("advance stored sign count: %v", err)
	}
	refusals := obs.PasskeyLoginRefusalsTotal.WithLabelValues(obs.PasskeyRefusalCloneWarning)
	before := testutil.ToFloat64(refusals)

	cookie, challenge := beginLogin(t, rig)
	// A copy of the key signs at counter 3 — behind the stored 5.
	w := finishLogin(t, rig, cookie, auth.assertionBody(t, challenge, flagUserPresent|flagUserVerified, 3))
	if w.Code != http.StatusBadRequest || sessionCookieSet(w) {
		t.Fatalf("clone-warning login: status %d, session minted %v — want 400 and no session", w.Code, sessionCookieSet(w))
	}
	if got := testutil.ToFloat64(refusals) - before; got != 1 {
		t.Fatalf("clone_warning refusals rose by %v, want 1", got)
	}
	e := onlyAuditEntry(t, sink)
	if e.Action != AuditActionPasskeyCloneWarning || e.ActorKind != platform.ActorSystem ||
		e.ActorUserID != uuid.Nil || e.AccountID != rig.user.AccountID || e.TargetID != stored.ID.String() {
		t.Fatalf("audit row = %+v, want a system passkey.clone_warning on %s with no actor user", e, stored.ID)
	}
	meta := auditMeta(t, e)
	if meta["credential_owner_user_id"] != rig.user.ID.String() ||
		meta["stored_sign_count"] != float64(5) || meta["presented_sign_count"] != float64(3) {
		t.Fatalf("audit metadata = %v, want owner and both sign counts", meta)
	}
}

// TestPasskeyFinishLogin_ReplayIsAuditedAndCounted — a captured
// finish-login request presented twice is refused; the refusal is
// recorded against the credential it tried to use.
func TestPasskeyFinishLogin_ReplayIsAuditedAndCounted(t *testing.T) {
	rig, auth, _ := newLiveClockPasskeyRig(t)
	sink := withAuditSink(rig)
	stored := storedCredential(t, rig)
	refusals := obs.PasskeyLoginRefusalsTotal.WithLabelValues(obs.PasskeyRefusalCeremonyReplay)

	cookie, challenge := beginLogin(t, rig)
	body := auth.assertionBody(t, challenge, flagUserPresent|flagUserVerified, 0)
	if first := finishLogin(t, rig, cookie, body); first.Code != http.StatusOK {
		t.Fatalf("first finish-login status = %d, want 200", first.Code)
	}
	if n := len(sink.all()); n != 0 {
		t.Fatalf("a successful sign-in wrote %d refusal rows, want 0", n)
	}
	before := testutil.ToFloat64(refusals)
	if second := finishLogin(t, rig, cookie, body); second.Code != http.StatusBadRequest {
		t.Fatalf("replay status = %d, want 400", second.Code)
	}
	if got := testutil.ToFloat64(refusals) - before; got != 1 {
		t.Fatalf("ceremony_replay refusals rose by %v, want 1", got)
	}
	e := onlyAuditEntry(t, sink)
	if e.Action != AuditActionPasskeyLoginReplay || e.ActorKind != platform.ActorSystem ||
		e.AccountID != rig.user.AccountID || e.TargetID != stored.ID.String() {
		t.Fatalf("audit row = %+v, want a system passkey.login_replay on %s", e, stored.ID)
	}
}
