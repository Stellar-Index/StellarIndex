package main

import (
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/config"
)

// A previous seal secret with no current one is a half-done rotation: starting would store new keys raw and leave
// every sealed key unopenable, so startup refuses.
func TestBuildWebhookKeySealer_RefusesPreviousWithoutCurrent(t *testing.T) {
	cfg := config.DashboardConfig{WebhookSealKeyEnv: "SI_TEST_SEAL_KEY", WebhookSealPreviousKeyEnv: "SI_TEST_SEAL_PREVIOUS_KEY"}
	t.Setenv(cfg.WebhookSealKeyEnv, "")
	t.Setenv(cfg.WebhookSealPreviousKeyEnv, strings.Repeat("p", 32)) // gitleaks:allow — fixture filler, not a credential
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	sealer, err := buildWebhookKeySealer(cfg, logger)
	if err == nil || sealer != nil || !strings.Contains(err.Error(), cfg.WebhookSealKeyEnv) {
		t.Fatalf("buildWebhookKeySealer = (%v, %v), want a refusal naming %s", sealer, err, cfg.WebhookSealKeyEnv)
	}

	t.Setenv(cfg.WebhookSealPreviousKeyEnv, "")
	if sealer, err := buildWebhookKeySealer(cfg, logger); err != nil || sealer != nil {
		t.Errorf("both unset = (%v, %v), want (nil, nil): keys stored raw", sealer, err)
	}
}
