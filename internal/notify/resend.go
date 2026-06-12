package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

// ResendSender ships transactional email through Resend's REST
// API (https://resend.com/docs/api-reference/emails/send-email).
//
// Configured via the Resend API token (resend_api_key).
// Production deployments populate it from
// `STELLARATLAS_RESEND_API_KEY`; nil token → constructor returns
// an error so deployments missing the key fail loud rather
// than silently dropping mail.
type ResendSender struct {
	APIKey  string
	Client  *http.Client
	BaseURL string // defaults to https://api.resend.com when empty; tests override
}

// NewResendSender constructs a sender. Validates the API key
// is non-empty so misconfigured deployments fail at boot
// rather than at the first /v1/auth/login call.
func NewResendSender(apiKey string) (*ResendSender, error) {
	if apiKey == "" {
		return nil, errors.New("notify: Resend API key is required")
	}
	return &ResendSender{
		APIKey:  apiKey,
		Client:  &http.Client{Timeout: 10 * time.Second},
		BaseURL: "https://api.resend.com",
	}, nil
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
func (r *ResendSender) Send(ctx context.Context, msg Message) error {
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
