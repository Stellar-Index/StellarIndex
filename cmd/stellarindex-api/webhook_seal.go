package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/config"
	"github.com/Stellar-Index/StellarIndex/internal/platform"
	"github.com/Stellar-Index/StellarIndex/internal/platform/postgresstore"
)

// buildWebhookKeySealer reads the webhook seal secret, and during a
// rotation the previous one, from the env vars cfg names. Unset returns
// nil (keys stored raw); a too-short value is a startup error rather than
// a weak key.
func buildWebhookKeySealer(cfg config.DashboardConfig, logger *slog.Logger) (*platform.WebhookKeySealer, error) {
	secret := os.Getenv(cfg.WebhookSealKeyEnv)
	if secret == "" {
		logger.Warn("webhook seal key env unset — new customer-webhook signing keys are stored unsealed, "+
			"and deliveries to webhooks whose key is already sealed wait until it is set",
			"env", cfg.WebhookSealKeyEnv)
		return nil, nil
	}
	sealer, err := platform.NewWebhookKeySealer([]byte(secret))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", cfg.WebhookSealKeyEnv, err)
	}
	if cfg.WebhookSealPreviousKeyEnv == "" {
		return sealer, nil
	}
	previous := os.Getenv(cfg.WebhookSealPreviousKeyEnv)
	if previous == "" {
		return sealer, nil
	}
	if previous == secret {
		return nil, fmt.Errorf("%s: equals %s; set it only to the secret being rotated out",
			cfg.WebhookSealPreviousKeyEnv, cfg.WebhookSealKeyEnv)
	}
	if sealer, err = sealer.WithPrevious([]byte(previous)); err != nil {
		return nil, fmt.Errorf("%s: %w", cfg.WebhookSealPreviousKeyEnv, err)
	}
	logger.Info("webhook seal key rotation in progress: keys sealed under the previous secret still open and are re-sealed",
		"env", cfg.WebhookSealPreviousKeyEnv)
	return sealer, nil
}

// resealWebhookKeys moves keys sealed under the previous seal secret to
// the current one. It runs off the boot path so a slow database cannot
// stall startup; a key it misses still opens until the next start.
func resealWebhookKeys(store *postgresstore.WebhookStore, logger *slog.Logger) {
	defer recoverBackgroundWorker(logger, "webhook-key-reseal")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	n, unopenable, err := store.ResealSigningKeys(ctx)
	if err != nil {
		logger.Error("re-seal customer-webhook signing keys under the current seal key",
			"err", err, "resealed", n, "unopenable", unopenable)
		return
	}
	logger.Info("re-sealed customer-webhook signing keys under the current seal key",
		"resealed", n, "unopenable", unopenable)
}
