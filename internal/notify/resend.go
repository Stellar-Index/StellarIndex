package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
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

// ResendSender ships transactional email through Resend's REST API; an empty token makes the constructor return ErrNotConfigured and the API binary wires UnconfiguredSender, so a missing key fails every send loudly rather than dropping mail.
type ResendSender struct {
	APIKey  string
	Client  *http.Client
	BaseURL string // defaults to https://api.resend.com when empty; tests override
}

// NewResendSender constructs a sender, rejecting an empty key so a misconfigured deployment fails at boot; the key is whitespace-trimmed, since a stray newline would corrupt the Authorization header.
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

// MailConfigured reports whether the sender holds a key to send with (see IsUnconfigured); safe on a nil receiver.
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

// Send POSTs to /emails after validating first; 4xx maps to ErrProviderRejected, 429/5xx/network errors to ErrTransient (retried up to sendAttempts), a keyless sender fails ErrNotConfigured, and the POST runs detached from ctx cancellation so a client disconnect cannot abort delivery.
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
		err = r.do(ctx, http.MethodPost, "/emails", body)
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

// DisableTracking turns click and open tracking off on the sending domain. Resend has no per-send
// tracking field, so the domain setting is the only pin; click tracking rewrites sign-in links.
func (r *ResendSender) DisableTracking(ctx context.Context, domainID string) error {
	if !r.MailConfigured() {
		return ErrNotConfigured
	}
	domainID = strings.TrimSpace(domainID)
	if domainID == "" {
		return errors.Join(ErrInvalidMessage, errors.New("resend domain id is empty"))
	}
	body, err := json.Marshal(struct {
		ClickTracking bool `json:"click_tracking"`
		OpenTracking  bool `json:"open_tracking"`
	}{})
	if err != nil {
		return fmt.Errorf("notify: marshal: %w", err)
	}
	return r.do(ctx, http.MethodPatch, "/domains/"+url.PathEscape(domainID), body)
}

// do makes one provider call and classifies its outcome.
func (r *ResendSender) do(ctx context.Context, method, path string, body []byte) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), sendTimeout)
	defer cancel()
	httpReq, err := http.NewRequestWithContext(ctx, method, r.BaseURL+path, bytes.NewReader(body))
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
