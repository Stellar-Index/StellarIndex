package main

import (
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/Stellar-Index/StellarIndex/internal/config"
	"github.com/Stellar-Index/StellarIndex/internal/notify"
)

// buildDashboardSender picks the mail transport the dashboard auth flow
// (and, through the bundle, the public signup flow) sends through.
//
// An unset / empty / blank-only Resend credential wires
// notify.UnconfiguredSender, whose every Send is an error: the
// login handler refuses with 503 and a counted failure, and signup reports
// email_verification_sent:false. Configured suppressed addresses are then
// filtered by notify.SuppressingSender.
//
// This deliberately does NOT refuse to boot: the dashboard is one surface
// of the API, and a missing mail credential must not take down price
// serving with it. Passkey sign-in and existing sessions keep working.
//
// Only the NAME of the env var is ever logged — never the value, nor any
// part of it.
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
