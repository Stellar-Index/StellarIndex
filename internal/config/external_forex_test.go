package config_test

import (
	"slices"
	"strings"
	"testing"
	"time"

	cfg "github.com/Stellar-Index/StellarIndex/internal/config"
)

const forexBase = `
[region]
id = "r1"

[stellar]
network = "pubnet"

[storage]
postgres_dsn = "postgres://u:p@h/db"
`

func TestOpenExchangeRates_DefaultsOff(t *testing.T) {
	oxr := cfg.Default().External.OpenExchangeRates
	if oxr.Enabled || oxr.AppID != "" || oxr.Endpoint != "" {
		t.Errorf("Default() openexchangerates = %+v, want disabled with no app id or endpoint", oxr)
	}
}

func TestOpenExchangeRates_LoadsFromTOML(t *testing.T) {
	body := forexBase + `
[external.openexchangerates]
enabled = true
app_id = "toml-id"
endpoint = "http://127.0.0.1:9/api"
`
	c, err := cfg.LoadReader(strings.NewReader(body), "test.toml")
	if err != nil {
		t.Fatalf("LoadReader: %v", err)
	}
	oxr := c.External.OpenExchangeRates
	if !oxr.Enabled || oxr.AppID != "toml-id" || oxr.Endpoint != "http://127.0.0.1:9/api" {
		t.Errorf("openexchangerates = %+v", oxr)
	}
}

func TestOpenExchangeRates_EnvOverridesTOML(t *testing.T) {
	t.Setenv("OPENEXCHANGERATES_APP_ID", "env-id")
	c := cfg.Default()
	c.External.OpenExchangeRates.AppID = "toml-id"
	got := c.ApplyEnvOverrides()
	if c.External.OpenExchangeRates.AppID != "env-id" {
		t.Errorf("app_id = %q, want the env value", c.External.OpenExchangeRates.AppID)
	}
	if !slices.Contains(got, "external.openexchangerates.app_id") {
		t.Errorf("overridden = %v, want external.openexchangerates.app_id listed", got)
	}
}

func TestMassiveRefreshInterval(t *testing.T) {
	if got := cfg.Default().External.Massive.RefreshInterval; got != time.Hour {
		t.Errorf("Default() refresh_interval = %v, want 1h", got)
	}
	cases := []struct {
		in          time.Duration
		want        time.Duration
		wantClamped bool
	}{
		{0, time.Hour, false},
		{-time.Minute, time.Hour, false},
		{time.Minute, cfg.MinMassiveRefreshInterval, true},
		{cfg.MinMassiveRefreshInterval, cfg.MinMassiveRefreshInterval, false},
		{30 * time.Minute, 30 * time.Minute, false},
		{2 * time.Hour, 2 * time.Hour, false},
	}
	for _, tc := range cases {
		got, clamped := cfg.MassiveConfig{RefreshInterval: tc.in}.EffectiveRefreshInterval()
		if got != tc.want || clamped != tc.wantClamped {
			t.Errorf("EffectiveRefreshInterval(%v) = (%v, %v), want (%v, %v)", tc.in, got, clamped, tc.want, tc.wantClamped)
		}
	}

	c, err := cfg.LoadReader(strings.NewReader(forexBase+"\n[external.massive]\nrefresh_interval = \"30m\"\n"), "test.toml")
	if err != nil {
		t.Fatalf("LoadReader: %v", err)
	}
	if got := c.External.Massive.RefreshInterval; got != 30*time.Minute {
		t.Errorf("loaded refresh_interval = %v, want 30m", got)
	}
}
