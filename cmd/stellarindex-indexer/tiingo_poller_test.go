package main

import (
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/config"
	"github.com/Stellar-Index/StellarIndex/internal/rwa"
	externaltiingo "github.com/Stellar-Index/StellarIndex/internal/sources/external/tiingo"
)

func TestNewTiingoPoller_TickersFromBindings(t *testing.T) {
	t.Parallel()

	p, err := newTiingoPoller(config.TiingoVenueConfig{APIKey: "k"})
	if err != nil {
		t.Fatalf("newTiingoPoller: %v", err)
	}
	if !slices.Equal(p.Tickers, rwa.FundNAVTickers()) {
		t.Errorf("Tickers = %v, want the fund bindings' %v", p.Tickers, rwa.FundNAVTickers())
	}
	if got, want := p.PollInterval(), time.Hour; got != want {
		t.Errorf("PollInterval() = %v, want connector default %v", got, want)
	}

	p, err = newTiingoPoller(config.TiingoVenueConfig{APIKey: "k", PollInterval: 2 * time.Hour})
	if err != nil {
		t.Fatalf("newTiingoPoller: %v", err)
	}
	if got, want := p.PollInterval(), 2*time.Hour; got != want {
		t.Errorf("PollInterval() = %v, want override %v", got, want)
	}
}

func TestNewTiingoPoller_RefusesMissingKey(t *testing.T) {
	t.Parallel()

	if _, err := newTiingoPoller(config.TiingoVenueConfig{Enabled: true}); !errors.Is(err, externaltiingo.ErrAPIKeyRequired) {
		t.Errorf("err = %v, want ErrAPIKeyRequired", err)
	}
}
