package main

import (
	"context"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"strings"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/aggregate/anomaly"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/customerwebhook"
	"github.com/Stellar-Index/StellarIndex/internal/divergence"
	"github.com/Stellar-Index/StellarIndex/internal/platform"
	"github.com/Stellar-Index/StellarIndex/internal/pricingguard"
)

// The anomaly.freeze and divergence.firing webhooks push the pair's
// aggregated price (frozen_value / our_price) to customers, signed. They
// must consult the same withholding decision /v1/price serves under, on
// BOTH legs, or a flagged issuer's price is delivered that the API refuses
// to publish.

type recordingPublisher struct {
	events   []platform.WebhookEventType
	payloads [][]byte
}

func (p *recordingPublisher) Publish(_ context.Context, ev platform.WebhookEventType, payload []byte) (customerwebhook.PublishResult, error) {
	p.events = append(p.events, ev)
	p.payloads = append(p.payloads, payload)
	return customerwebhook.PublishResult{}, nil
}

func webhookGatePairs(t *testing.T) (flagged, native canonical.Asset) {
	t.Helper()
	flagged, err := canonical.NewClassicAsset("RIO", alertScamIssuer)
	if err != nil {
		t.Fatalf("build flagged classic: %v", err)
	}
	return flagged, canonical.NativeAsset()
}

func flaggedWithholding() pricingguard.Gate {
	dir := &alertScamDirectory{flagged: map[string]bool{alertScamIssuer: true}}
	return pricingguard.Gate{Scam: pricingguard.NewScamGate(dir, pricingguard.ScamGateOptions{})}
}

func TestAnomalyFreezeHook_WithholdsFlaggedIssuer(t *testing.T) {
	flagged, native := webhookGatePairs(t)
	for _, tc := range []struct {
		name        string
		base, quote canonical.Asset
	}{
		{"flagged base", flagged, native},
		{"flagged quote", native, flagged},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pub := &recordingPublisher{}
			hook := anomalyFreezeHook(discardLogger(), pub, flaggedWithholding())
			hook(context.Background(), tc.base, tc.quote, "0.4200000", anomaly.Decision{Reason: "test"})
			if len(pub.payloads) != 0 {
				t.Fatalf("anomaly.freeze delivered %s for a directory-flagged issuer's market", pub.payloads[0])
			}
		})
	}
}

func TestAnomalyFreezeHook_UnflaggedPairStillDelivers(t *testing.T) {
	_, native := webhookGatePairs(t)
	usd, err := canonical.NewFiatAsset("USD")
	if err != nil {
		t.Fatal(err)
	}
	pub := &recordingPublisher{}
	hook := anomalyFreezeHook(discardLogger(), pub, flaggedWithholding())
	hook(context.Background(), native, usd, "0.4200000", anomaly.Decision{Reason: "test"})
	if len(pub.payloads) != 1 || pub.events[0] != platform.WebhookEventAnomalyFreeze {
		t.Fatalf("got %d deliveries (%v), want one anomaly.freeze", len(pub.payloads), pub.events)
	}
	var got anomalyFreezeWebhookPayload
	if err := json.Unmarshal(pub.payloads[0], &got); err != nil {
		t.Fatal(err)
	}
	if got.FrozenValue != "0.4200000" || got.Asset != native.String() || got.Quote != usd.String() {
		t.Errorf("payload = %+v, want frozen_value 0.4200000 for %s/%s", got, native, usd)
	}
}

func TestDivergenceFiringHook_WithholdsFlaggedIssuer(t *testing.T) {
	flagged, native := webhookGatePairs(t)
	for _, tc := range []struct {
		name        string
		base, quote canonical.Asset
	}{
		{"flagged base", flagged, native},
		{"flagged quote", native, flagged},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pair, err := canonical.NewPair(tc.base, tc.quote)
			if err != nil {
				t.Fatal(err)
			}
			pub := &recordingPublisher{}
			hook := divergenceFiringHook(discardLogger(), pub, flaggedWithholding())
			hook(context.Background(), pair, divergence.CachedResult{OurPrice: 0.42, Median: 0.3, ComputedAt: time.Now()})
			if len(pub.payloads) != 0 {
				t.Fatalf("divergence.firing delivered %s for a directory-flagged issuer's market", pub.payloads[0])
			}
		})
	}
}

func TestDivergenceFiringHook_UnflaggedPairStillDelivers(t *testing.T) {
	_, native := webhookGatePairs(t)
	usd, err := canonical.NewFiatAsset("USD")
	if err != nil {
		t.Fatal(err)
	}
	pair, err := canonical.NewPair(native, usd)
	if err != nil {
		t.Fatal(err)
	}
	pub := &recordingPublisher{}
	hook := divergenceFiringHook(discardLogger(), pub, flaggedWithholding())
	hook(context.Background(), pair, divergence.CachedResult{OurPrice: 0.42, Median: 0.3, ComputedAt: time.Now()})
	if len(pub.payloads) != 1 || pub.events[0] != platform.WebhookEventDivergenceFiring {
		t.Fatalf("got %d deliveries (%v), want one divergence.firing", len(pub.payloads), pub.events)
	}
	var got divergenceFiringWebhookPayload
	if err := json.Unmarshal(pub.payloads[0], &got); err != nil {
		t.Fatal(err)
	}
	if got.OurPrice != "0.42" || got.Pair != pair.String() {
		t.Errorf("payload = %+v, want our_price 0.42 for %s", got, pair)
	}
}

// TestWebhookPublishSitesAreGated fails when a customer-webhook Publish in
// this binary's non-test sources is not preceded, in the same function
// literal, by the withholding decision — so a new push surface cannot
// deliver a price the API withholds.
func TestWebhookPublishSitesAreGated(t *testing.T) {
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi fs.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatalf("parse package: %v", err)
	}
	sites := 0
	for _, pkg := range pkgs {
		for name, file := range pkg.Files {
			ast.Inspect(file, func(n ast.Node) bool {
				lit, ok := n.(*ast.FuncLit)
				if !ok {
					return true
				}
				publishes, gated := webhookPublishAndGate(lit.Body)
				if publishes {
					sites++
					if !gated {
						t.Errorf("%s: %s publishes a customer webhook without consulting pricingguard.Gate",
							name, fset.Position(lit.Pos()))
					}
				}
				return true
			})
		}
	}
	if sites < 2 {
		t.Fatalf("found %d gated webhook publish sites, want >= 2 (anomaly.freeze, divergence.firing) — the guard lost its subject", sites)
	}
}

// webhookPublishAndGate reports whether body calls X.Publish with a
// platform.WebhookEvent* argument, and whether it asks a pricingguard.Gate.
func webhookPublishAndGate(body *ast.BlockStmt) (publishes, gated bool) {
	ast.Inspect(body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		switch sel.Sel.Name {
		case "PriceWithheld", "PriceWithholding", "PriceWithholdingAt":
			gated = true
		case "Publish":
			for _, arg := range call.Args {
				if a, ok := arg.(*ast.SelectorExpr); ok && strings.HasPrefix(a.Sel.Name, "WebhookEvent") {
					publishes = true
				}
			}
		}
		return true
	})
	return publishes, gated
}
