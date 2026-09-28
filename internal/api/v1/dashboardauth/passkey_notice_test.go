package dashboardauth

// Adding or removing a passkey changes who can sign in as the user, so the
// user must hear about it by email (not only via the dashboard, which the
// party making the change is also looking at), and the change must be counted.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/Stellar-Index/StellarIndex/internal/notify"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/platform"
)

const passkeyNoticeLabel = "Trust me, this is your own device"

// registerPasskeyVia drives begin-register + finish-register for the rig
// user from 198.51.100.7 and returns the finish response.
func registerPasskeyVia(t *testing.T, rig *passkeyRig) *httptest.ResponseRecorder {
	t.Helper()
	auth := newSoftAuthenticator(t, rig.user.ID)
	w := httptest.NewRecorder()
	rig.h.HandlePasskeyBeginRegister(w, rig.withSession(httptest.NewRequest(http.MethodPost, "/v1/auth/passkey/begin-register", nil)))
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
	req := rig.withSession(httptest.NewRequest(http.MethodPost, "/v1/auth/passkey/finish-register",
		strings.NewReader(auth.attestationBody(t, opts.PublicKey.Challenge, passkeyNoticeLabel))))
	req.AddCookie(&http.Cookie{Name: PasskeyCeremonyCookieName, Value: cookie.Value})
	req.RemoteAddr = "198.51.100.7:4242"
	req.Header.Set("User-Agent", "Example/1.0")
	w = httptest.NewRecorder()
	rig.h.HandlePasskeyFinishRegister(w, req)
	return w
}

func passkeyChangeCount(change string) float64 {
	return testutil.ToFloat64(obs.PasskeyCredentialChangesTotal.WithLabelValues(change))
}

func passkeyNoticeCount(result string) float64 {
	return testutil.ToFloat64(obs.NotifySendsTotal.WithLabelValues(obs.NotifyTemplatePasskeyChanged, result))
}

// onlyPasskeyNotice asserts exactly one mail went out, to the user, as the
// passkey-changed notice for change, and returns it.
func onlyPasskeyNotice(t *testing.T, rig *passkeyRig, change string) notify.Message {
	t.Helper()
	if n := rig.sender.SentCount(); n != 1 {
		t.Fatalf("mails sent = %d, want exactly 1 passkey notice", n)
	}
	msg, _ := rig.sender.Last()
	if len(msg.To) != 1 || msg.To[0] != rig.user.Email {
		t.Fatalf("notice To = %v, want [%s]", msg.To, rig.user.Email)
	}
	if msg.Tags["template"] != "passkey-changed" || !strings.Contains(msg.Subject, "passkey was "+change) {
		t.Fatalf("notice subject %q tags %v, want the passkey-%s notice", msg.Subject, msg.Tags, change)
	}
	for _, body := range []string{msg.Text, msg.HTML} {
		for _, want := range []string{"198.51.100.7", "Example/1.0", "5 May 2026 12:00 UTC", "https://app.stellarindex.io/dashboard/settings"} {
			if !strings.Contains(body, want) {
				t.Errorf("notice body missing %q:\n%s", want, body)
			}
		}
		if strings.Contains(body, passkeyNoticeLabel) {
			t.Errorf("notice echoes the requester-chosen passkey label:\n%s", body)
		}
	}
	return msg
}

func TestPasskeyNotice_RegisterMailsOwnerAndCounts(t *testing.T) {
	rig := newPasskeyRig(t)
	added, sent := passkeyChangeCount(obs.PasskeyChangeAdded), passkeyNoticeCount(obs.NotifySendResultSent)

	if w := registerPasskeyVia(t, rig); w.Code != http.StatusOK {
		t.Fatalf("finish-register status = %d, want 200 (%s)", w.Code, w.Body.String())
	}
	onlyPasskeyNotice(t, rig, "added")
	if got := passkeyChangeCount(obs.PasskeyChangeAdded) - added; got != 1 {
		t.Errorf("passkey_credential_changes_total{change=added} delta = %v, want 1", got)
	}
	if got := passkeyNoticeCount(obs.NotifySendResultSent) - sent; got != 1 {
		t.Errorf("notify_sends_total{template=passkey-changed,result=sent} delta = %v, want 1", got)
	}
}

// A mail outage must not undo or fail a committed registration, but the
// undelivered notice has to show on the send-failure counter.
func TestPasskeyNotice_RegisterSendFailureIsCounted(t *testing.T) {
	rig := newPasskeyRig(t)
	rig.h.cfg.Sender = stubFailSender{err: errors.New("resend: 503 service unavailable")}
	failed := passkeyNoticeCount(obs.NotifySendResultFailed)

	if w := registerPasskeyVia(t, rig); w.Code != http.StatusOK {
		t.Fatalf("finish-register status = %d, want 200 despite mail outage (%s)", w.Code, w.Body.String())
	}
	storedCredential(t, rig)
	if got := passkeyNoticeCount(obs.NotifySendResultFailed) - failed; got != 1 {
		t.Errorf("notify_sends_total{template=passkey-changed,result=failed} delta = %v, want 1", got)
	}
}

func TestPasskeyNotice_DeleteMailsOwnerAndCounts(t *testing.T) {
	rig := newPasskeyRig(t)
	mine, err := rig.passkeys.CreateWebAuthnCredential(context.Background(), platform.WebAuthnCredential{
		UserID:       rig.user.ID,
		Name:         passkeyNoticeLabel,
		CredentialID: []byte("EXAMPLE-CRED-ID-PLACEHOLDER-NOTICE"),
		PublicKey:    []byte("EXAMPLE-PUBKEY-PLACEHOLDER"),
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	removed := passkeyChangeCount(obs.PasskeyChangeRemoved)

	del := func(id string) int {
		req := rig.withSession(httptest.NewRequest(http.MethodDelete, "/v1/auth/passkey/credentials/"+id, nil))
		req.SetPathValue("id", id)
		req.RemoteAddr = "198.51.100.7:4242"
		req.Header.Set("User-Agent", "Example/1.0")
		w := httptest.NewRecorder()
		rig.h.HandlePasskeyDelete(w, req)
		return w.Code
	}
	// A refused delete changes nothing, so it is neither mailed nor counted.
	if code := del("00000000-0000-4000-8000-000000000001"); code != http.StatusNotFound || rig.sender.SentCount() != 0 {
		t.Fatalf("404 delete: status %d, mails %d — want 404 and none", code, rig.sender.SentCount())
	}
	if code := del(mine.ID.String()); code != http.StatusNoContent {
		t.Fatalf("delete status = %d, want 204", code)
	}
	onlyPasskeyNotice(t, rig, "removed")
	if got := passkeyChangeCount(obs.PasskeyChangeRemoved) - removed; got != 1 {
		t.Errorf("passkey_credential_changes_total{change=removed} delta = %v, want 1", got)
	}
}
