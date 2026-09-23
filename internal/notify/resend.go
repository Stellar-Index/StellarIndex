package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// sendTimeout bounds one provider POST independently of the caller's
// context, which [ResendSender.Send] detaches from.
const sendTimeout = 10 * time.Second

// ResendSender ships transactional email through Resend's REST
// API (https://resend.com/docs/api-reference/emails/send-email).
//
// Configured via the Resend API token (resend_api_key).
// Production deployments populate it from
// `STELLARINDEX_RESEND_API_KEY`; an empty token → the constructor
// returns ErrNotConfigured, and the API binary wires
// [UnconfiguredSender] in its place, so a deployment missing the
// key fails every send loudly rather than silently dropping mail.
type ResendSender struct {
	APIKey  string
	Client  *http.Client
	BaseURL string // defaults to https://api.resend.com when empty; tests override
}

// NewResendSender constructs a sender. Validates the API key
// is non-empty so misconfigured deployments fail at boot
// rather than at the first /v1/auth/login call.
//
// The key is whitespace-trimmed: a blank-only value is no key at
// all, and a stray trailing newline (a hand-edited env file) would
// otherwise corrupt the Authorization header on every send.
func NewResendSender(apiKey string) (*ResendSender, error) {
	apiKey = strings.TrimSpace(apiKey)
	if apiKey == "" {
		return nil, fmt.Errorf("%w: Resend API key is required", ErrNotConfigured)
	}
	return &ResendSender{
		APIKey:  apiKey,
		Client:  &http.Client{Timeout: sendTimeout},
		BaseURL: "https://api.resend.com",
	}, nil
}

// MailConfigured reports whether the sender holds a key to send
// with. See [IsUnconfigured]. Safe on a nil receiver.
func (r *ResendSender) MailConfigured() bool {
	return r != nil && strings.TrimSpace(r.APIKey) != ""
}

// resendRequest mirrors the JSON body Resend accepts. Tags use
// their per-tag {name, value} shape; IdempotencyKey rides on
// the `Idempotency-Key` header (not in the body).
type resendRequest struct {
	From    string            `json:"from"`
	To      []string          `json:"to"`
	Subject string            `json:"subject"`
	HTML    string            `json:"html,omitempty"`
	Text    string            `json:"text,omitempty"`
	Tags    []resendTag       `json:"tags,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
}

type resendTag struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type resendErrorResp struct {
	Name       string `json:"name"`
	Message    string `json:"message"`
	StatusCode int    `json:"statusCode"`
}

// Send POSTs to /emails. Validates first so misconfigured
// callers get a structured error before we burn a Resend API
// call; maps 4xx → ErrProviderRejected, 5xx + network → ErrTransient.
//
// A sender with no key (a struct literal that bypassed
// NewResendSender) fails with ErrNotConfigured before the wire: a
// blank bearer can only ever be rejected, and the honest name for
// that state is "not configured", not "provider rejected".
//
// The POST runs detached from ctx's cancellation: callers send after
// their side effects are durable (token row, signup reservation), so a
// client disconnect must not abort delivery and be counted as failed.
func (r *ResendSender) Send(ctx context.Context, msg Message) error {
	if !r.MailConfigured() {
		return ErrNotConfigured
	}
	if err := validate(msg); err != nil {
		return err
	}
	req := resendRequest{
		From:    msg.From,
		To:      msg.To,
		Subject: msg.Subject,
		HTML:    msg.HTML,
		Text:    msg.Text,
	}
	for k, v := range msg.Tags {
		req.Tags = append(req.Tags, resendTag{Name: k, Value: v})
	}

	body, err := json.Marshal(req)
	if err != nil {
		// Marshal of plain string fields cannot fail in practice;
		// wrap defensively.
		return fmt.Errorf("notify: marshal: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), sendTimeout)
	defer cancel()
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		r.BaseURL+"/emails", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("notify: build request: %w", err)
	}
	httpReq.Header.Set("Authorization", "Bearer "+r.APIKey)
	httpReq.Header.Set("Content-Type", "application/json")
	if msg.IdempotencyKey != "" {
		httpReq.Header.Set("Idempotency-Key", msg.IdempotencyKey)
	}

	resp, err := r.Client.Do(httpReq)
	if err != nil {
		return errors.Join(ErrTransient, err)
	}
	defer func() { _, _ = io.Copy(io.Discard, resp.Body); _ = resp.Body.Close() }()

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}

	// Non-2xx — peek the body for the provider's error envelope.
	limited := io.LimitReader(resp.Body, 8<<10)
	raw, _ := io.ReadAll(limited)
	var perr resendErrorResp
	_ = json.Unmarshal(raw, &perr)
	detail := perr.Message
	if detail == "" {
		detail = string(raw)
	}

	if resp.StatusCode >= 400 && resp.StatusCode < 500 {
		return fmt.Errorf("%w: HTTP %d: %s", ErrProviderRejected, resp.StatusCode, detail)
	}
	return fmt.Errorf("%w: HTTP %d: %s", ErrTransient, resp.StatusCode, detail)
}
