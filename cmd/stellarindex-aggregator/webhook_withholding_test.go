package main

import (
	"context"
	"encoding/json"
	"errors"
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
	keys     []string
	err      error
}

func (p *recordingPublisher) PublishOnce(_ context.Context, ev platform.WebhookEventType, key string, payload []byte) (customerwebhook.PublishResult, error) {
	p.keys = append(p.keys, key)
	p.events = append(p.events, ev)
	p.payloads = append(p.payloads, payload)
	return customerwebhook.PublishResult{}, p.err
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

// TestAnomalyFreezeHook_FirstTickFreezeOmitsFrozenValue: a pair that
// freezes on its first bucket has no price to pin, so the payload must not
// assert one (the sink's NUMERIC NOT NULL filler used to arrive as "0").
func TestAnomalyFreezeHook_FirstTickFreezeOmitsFrozenValue(t *testing.T) {
	_, native := webhookGatePairs(t)
	usd, err := canonical.NewFiatAsset("USD")
	if err != nil {
		t.Fatal(err)
	}
	pub := &recordingPublisher{}
	hook := anomalyFreezeHook(discardLogger(), pub, flaggedWithholding())
	hook(context.Background(), native, usd, "", anomaly.Decision{Reason: "test"})
	if len(pub.payloads) != 1 {
		t.Fatalf("got %d deliveries, want one anomaly.freeze", len(pub.payloads))
	}
	var body map[string]any
	if err := json.Unmarshal(pub.payloads[0], &body); err != nil {
		t.Fatal(err)
	}
	if v, ok := body["frozen_value"]; ok {
		t.Errorf("first-tick freeze delivered frozen_value=%v, want the field absent", v)
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
			if err := hook(context.Background(), pair, divergence.CachedResult{OurPrice: 0.42, Median: 0.3, ComputedAt: time.Now()}); err != nil {
				t.Fatalf("withheld hook returned %v, want nil", err)
			}
			if len(pub.payloads) != 0 {
				t.Fatalf("divergence.firing delivered %s for a directory-flagged issuer's market", pub.payloads[0])
			}
		})
	}
}

// A lost fan-out must surface as an error so the divergence worker
// releases its edge latch and retries the episode.
func TestDivergenceFiringHook_FanoutFailureIsReturned(t *testing.T) {
	_, native := webhookGatePairs(t)
	usd, err := canonical.NewFiatAsset("USD")
	if err != nil {
		t.Fatal(err)
	}
	pair, err := canonical.NewPair(native, usd)
	if err != nil {
		t.Fatal(err)
	}
	boom := errors.New("enqueue failed")
	pub := &recordingPublisher{err: boom}
	hook := divergenceFiringHook(discardLogger(), pub, flaggedWithholding())
	since := time.Unix(1700000000, 0)
	cached := divergence.CachedResult{OurPrice: 0.42, Median: 0.3, ComputedAt: since.Add(time.Minute), FiringSince: since}
	if got := hook(context.Background(), pair, cached); !errors.Is(got, boom) {
		t.Fatalf("hook err = %v, want %v", got, boom)
	}
	cached.ComputedAt = since.Add(2 * time.Minute)
	_ = hook(context.Background(), pair, cached)
	if len(pub.keys) != 2 || pub.keys[0] != pub.keys[1] {
		t.Fatalf("event keys = %v, want one stable key per episode", pub.keys)
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
	if err := hook(context.Background(), pair, divergence.CachedResult{OurPrice: 0.42, Median: 0.3, ComputedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
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
// A call through a method value (send := pub.Publish; send(...)) publishes
// too. The gate side stays call-only: a bare reference asks nothing.
func webhookPublishAndGate(body *ast.BlockStmt) (publishes, gated bool) {
	senders := publishMethodValues(body)
	ast.Inspect(body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		switch fun := call.Fun.(type) {
		case *ast.SelectorExpr:
			switch fun.Sel.Name {
			case "PriceWithheld", "PriceWithholding", "PriceWithholdingAt":
				gated = true
			case "Publish", "PublishOnce":
				publishes = publishes || passesWebhookEvent(call)
			}
		case *ast.Ident:
			publishes = publishes || (senders[fun.Name] && passesWebhookEvent(call))
		}
		return true
	})
	return publishes, gated
}

// publishMethodValues returns the locals body binds to a Publish or
// PublishOnce method value.
func publishMethodValues(body *ast.BlockStmt) map[string]bool {
	out := map[string]bool{}
	ast.Inspect(body, func(n ast.Node) bool {
		as, ok := n.(*ast.AssignStmt)
		if !ok || len(as.Lhs) != len(as.Rhs) {
			return true
		}
		for i, rhs := range as.Rhs {
			sel, ok := rhs.(*ast.SelectorExpr)
			id, isIdent := as.Lhs[i].(*ast.Ident)
			if ok && isIdent && (sel.Sel.Name == "Publish" || sel.Sel.Name == "PublishOnce") {
				out[id.Name] = true
			}
		}
		return true
	})
	return out
}

func passesWebhookEvent(call *ast.CallExpr) bool {
	for _, arg := range call.Args {
		if a, ok := arg.(*ast.SelectorExpr); ok && strings.HasPrefix(a.Sel.Name, "WebhookEvent") {
			return true
		}
	}
	return false
}

// TestWebhookPublishScanCatchesMethodValues pins that a publish is seen
// however the publisher is spelled, and that the gate is only a call.
func TestWebhookPublishScanCatchesMethodValues(t *testing.T) {
	cases := []struct {
		name                   string
		src                    string
		wantPublish, wantGated bool
	}{
		{"direct call", `pub.Publish(ctx, platform.WebhookEventAnomalyFreeze, p)`, true, false},
		{"method value", `send := pub.PublishOnce; send(ctx, platform.WebhookEventDivergenceFiring, k, p)`, true, false},
		{"publisher alias", `q := pub; q.Publish(ctx, platform.WebhookEventAnomalyFreeze, p)`, true, false},
		{"gated", `if g.PriceWithheld(ctx, a, b, "x") { return }; pub.Publish(ctx, platform.WebhookEventAnomalyFreeze, p)`, true, true},
		{"bare gate reference is not a gate", `_ = g.PriceWithheld; pub.Publish(ctx, platform.WebhookEventAnomalyFreeze, p)`, true, false},
		{"non-webhook publish", `send := stream.Publish; send(ctx, "channel", p)`, false, false},
	}
	for _, c := range cases {
		f, err := parser.ParseFile(token.NewFileSet(), "planted.go", "package p\nfunc f() {\n"+c.src+"\n}", 0)
		if err != nil {
			t.Fatalf("parse %q: %v", c.src, err)
		}
		pub, gated := webhookPublishAndGate(f.Decls[0].(*ast.FuncDecl).Body)
		if pub != c.wantPublish || gated != c.wantGated {
			t.Errorf("%s: publishes=%v gated=%v, want %v %v", c.name, pub, gated, c.wantPublish, c.wantGated)
		}
	}
}
