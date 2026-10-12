package main

import (
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/Stellar-Index/StellarIndex/internal/config"
	"github.com/Stellar-Index/StellarIndex/internal/notify"
)

// buildDashboardSender picks the dashboard auth and signup mail transport. A blank Resend credential wires notify.UnconfiguredSender
// (login 503, signup email_verification_sent:false) and deliberately does not refuse to boot; only the env var NAME is ever logged.
func buildDashboardSender(cfg config.DashboardConfig, logger *slog.Logger) (notify.Sender, error) {
	inner, err := buildDashboardTransport(cfg, logger)
	if err != nil {
		return nil, err
	}
	s, err := notify.WithSuppression(inner, cfg.SuppressedRecipientSHA256)
	if err != nil {
		return nil, fmt.Errorf("api.dashboard.suppressed_recipient_sha256: %w", err)
	}
	return s, nil
}

func buildDashboardTransport(cfg config.DashboardConfig, logger *slog.Logger) (notify.Sender, error) {
	apiKey := strings.TrimSpace(os.Getenv(cfg.ResendAPIKeyEnv))
	if apiKey == "" {
		reason := fmt.Sprintf("env %s is unset/empty", cfg.ResendAPIKeyEnv)
		logger.Error("dashboard mail transport is NOT configured — no sign-in or signup-verification "+
			"email can be delivered; POST /v1/auth/login will answer 503 until the credential is set",
			"reason", reason)
		return notify.UnconfiguredSender{Reason: reason}, nil
	}
	s, err := notify.NewResendSender(apiKey)
	if err != nil {
		return nil, fmt.Errorf("resend sender: %w", err)
	}
	logger.Info("dashboard auth using Resend sender", "from", cfg.EmailFrom)
	return s, nil
}
