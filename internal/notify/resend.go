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

	"github.com/Stellar-Index/StellarIndex/internal/httpx"
)

// sendTimeout bounds one provider POST independently of the caller's
// context, which [ResendSender.Send] detaches from.
const sendTimeout = 10 * time.Second

// sendAttempts and retryBaseDelay bound the retry of a transient
// failure so a provider outage cannot pin a handler goroutine.
const (
	sendAttempts   = 3
	retryBaseDelay = 200 * time.Millisecond
)

// ResendSender ships transactional email through Resend's REST
// API (https://resend.com/docs/api-reference/emails/send-email).
//
// Configured via the Resend API token (`STELLARINDEX_RESEND_API_KEY` in production). An empty
// token makes the constructor return ErrNotConfigured and the API binary wires
// [UnconfiguredSender] instead, so a missing key fails every send loudly rather than dropping mail.
type ResendSender struct {
	APIKey  string
	Client  *http.Client
	BaseURL string // defaults to https://api.resend.com when empty; tests override
}

// NewResendSender constructs a sender, rejecting an empty key so a misconfigured deployment
// fails at boot rather than at the first login email.
//
// The key is whitespace-trimmed: a blank-only value is no key, and a stray trailing newline
// would corrupt the Authorization header on every send.
func NewResendSender(apiKey string) (*ResendSender, error) {
	apiKey = strings.TrimSpace(apiKey)
	if apiKey == "" {
		return nil, fmt.Errorf("%w: Resend API key is required", ErrNotConfigured)
	}
	return &ResendSender{
		APIKey:  apiKey,
		Client:  httpx.NewKeyedClient("resend", sendTimeout),
		BaseURL: "https://api.resend.com",
	}, nil
}

// MailConfigured reports whether the sender holds a key to send
// with. See [IsUnconfigured]. Safe on a nil receiver.
func (r *ResendSender) MailConfigured() bool {
	return r != nil && strings.TrimSpace(r.APIKey) != ""
}

// resendRequest mirrors the JSON body Resend accepts. Tags use
// their per-tag {name, value} shape.
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

// Send POSTs to /emails. It validates first so bad callers get a structured error before a
// provider call, and maps 4xx to ErrProviderRejected and 429, 5xx and network errors to
// ErrTransient. A transient attempt is retried up to [sendAttempts] times with doubling backoff;
// a cancelled ctx stops further retries.
//
// A sender with no key (a struct literal that bypassed NewResendSender) fails with
// ErrNotConfigured before the wire: a blank bearer is "not configured", not "provider rejected".
//
// The POST runs detached from ctx's cancellation: callers send after their side effects are
// durable (token row, signup reservation), so a client disconnect must not abort delivery and be
// counted as failed.
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

	delay := retryBaseDelay
	for attempt := 1; ; attempt++ {
		err = r.post(ctx, body)
		if err == nil || !errors.Is(err, ErrTransient) || attempt == sendAttempts {
			return err
		}
		select {
		case <-ctx.Done():
			return err
		case <-time.After(delay):
		}
		delay *= 2
	}
}

// post makes one delivery attempt and classifies its outcome.
func (r *ResendSender) post(ctx context.Context, body []byte) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), sendTimeout)
	defer cancel()
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		r.BaseURL+"/emails", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("notify: build request: %w", err)
	}
	httpReq.Header.Set("Authorization", "Bearer "+r.APIKey)
	httpReq.Header.Set("Content-Type", "application/json")

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

	// 429 is the provider's rate limit, not a verdict on the message.
	if resp.StatusCode >= 400 && resp.StatusCode < 500 && resp.StatusCode != http.StatusTooManyRequests {
		return fmt.Errorf("%w: HTTP %d: %s", ErrProviderRejected, resp.StatusCode, detail)
	}
	return fmt.Errorf("%w: HTTP %d: %s", ErrTransient, resp.StatusCode, detail)
}
