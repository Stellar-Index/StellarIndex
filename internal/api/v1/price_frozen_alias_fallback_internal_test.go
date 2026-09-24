// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1

// frozenPairBase with no bucket served (`served` is the zero Asset): the
// fallback chain resolves across the whole alias set without reporting
// which leg it served, so every spelling's freeze marker governs, and a
// verdict is "checked" only when every spelling's marker was read.

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// aliasFreezeStub answers FrozenForPair per "asset/quote" key: frozen
// keys report true, failing keys return an error, anything else is a
// confirmed not-frozen.
type aliasFreezeStub struct {
	frozen  map[string]bool
	failing map[string]bool
}

func (f aliasFreezeStub) FrozenForPair(_ context.Context, asset, quote canonical.Asset) (bool, error) {
	key := asset.String() + "/" + quote.String()
	if f.failing[key] {
		return false, errors.New("freeze marker read failed")
	}
	return f.frozen[key], nil
}

func aliasFreezeServer(stub aliasFreezeStub) *Server {
	return &Server{freeze: stub, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
}

func xlmGBPFixture(t *testing.T) (native, xlm, gbp canonical.Asset, req *http.Request) {
	t.Helper()
	xlm, err := canonical.NewCryptoAsset("XLM")
	if err != nil {
		t.Fatalf("NewCryptoAsset(XLM): %v", err)
	}
	gbp, err = canonical.NewFiatAsset("GBP")
	if err != nil {
		t.Fatalf("NewFiatAsset(GBP): %v", err)
	}
	req = httptest.NewRequest(http.MethodGet, "/v1/price?asset=native&quote=fiat:GBP", nil)
	return canonical.NativeAsset(), xlm, gbp, req
}

func pairKey(a, q canonical.Asset) string { return a.String() + "/" + q.String() }

func TestFrozenPairBase_NoBucketServedConsultsEveryAliasNotJustTheLiteral(t *testing.T) {
	native, xlm, gbp, req := xlmGBPFixture(t)
	s := aliasFreezeServer(aliasFreezeStub{frozen: map[string]bool{pairKey(xlm, gbp): true}})

	governing, frozen, checked := s.frozenPairBase(req, native, canonical.Asset{}, gbp)

	if !checked || !frozen {
		t.Fatalf("checked=%v frozen=%v, want true/true — crypto:XLM/fiat:GBP is frozen and is a spelling of native", checked, frozen)
	}
	if governing.String() != xlm.String() {
		t.Errorf("governing = %q, want %q (the spelling that carries the freeze)", governing.String(), xlm.String())
	}
}

func TestFrozenPairBase_NoBucketServedStillHonoursTheLiteralWhenOnlyItIsFrozen(t *testing.T) {
	native, _, gbp, req := xlmGBPFixture(t)
	s := aliasFreezeServer(aliasFreezeStub{frozen: map[string]bool{pairKey(native, gbp): true}})

	governing, frozen, checked := s.frozenPairBase(req, native, canonical.Asset{}, gbp)

	if !checked || !frozen {
		t.Fatalf("checked=%v frozen=%v, want true/true — the literal itself is frozen", checked, frozen)
	}
	if governing.String() != native.String() {
		t.Errorf("governing = %q, want %q", governing.String(), native.String())
	}
}

func TestFrozenPairBase_NoBucketServedNoAliasFrozenReportsCheckedNotFrozen(t *testing.T) {
	native, _, gbp, req := xlmGBPFixture(t)
	s := aliasFreezeServer(aliasFreezeStub{})

	governing, frozen, checked := s.frozenPairBase(req, native, canonical.Asset{}, gbp)

	if frozen || !checked {
		t.Fatalf("checked=%v frozen=%v, want true/false — every marker was read and none is frozen", checked, frozen)
	}
	if !governing.IsZero() {
		t.Errorf("governing = %q, want zero Asset when not frozen", governing.String())
	}
}

// One unread marker makes the verdict unknown, whichever spelling it is:
// "unknown" must never be reported as "confirmed not frozen".
func TestFrozenPairBase_NoBucketServedAnyFailedMarkerReadIsUnchecked(t *testing.T) {
	native, xlm, gbp, _ := xlmGBPFixture(t)
	cases := map[string]canonical.Asset{
		"requested spelling fails": native,
		"sibling spelling fails":   xlm,
	}
	for name, failing := range cases {
		t.Run(name, func(t *testing.T) {
			_, _, _, req := xlmGBPFixture(t)
			s := aliasFreezeServer(aliasFreezeStub{failing: map[string]bool{pairKey(failing, gbp): true}})

			governing, frozen, checked := s.frozenPairBase(req, native, canonical.Asset{}, gbp)

			if checked {
				t.Fatalf("checked = true, want false — %s/%s's marker was never read", failing.String(), gbp.String())
			}
			if frozen || !governing.IsZero() {
				t.Errorf("governing=%q frozen=%v, want zero/false", governing.String(), frozen)
			}
		})
	}
}

// A frozen spelling governs even when another spelling's read failed:
// the freeze is a confirmed verdict, not an unknown one.
func TestFrozenPairBase_NoBucketServedFrozenSpellingWinsOverFailedRead(t *testing.T) {
	native, xlm, gbp, req := xlmGBPFixture(t)
	s := aliasFreezeServer(aliasFreezeStub{
		failing: map[string]bool{pairKey(native, gbp): true},
		frozen:  map[string]bool{pairKey(xlm, gbp): true},
	})

	governing, frozen, checked := s.frozenPairBase(req, native, canonical.Asset{}, gbp)

	if !checked || !frozen || governing.String() != xlm.String() {
		t.Fatalf("governing=%q frozen=%v checked=%v, want %q/true/true", governing.String(), frozen, checked, xlm.String())
	}
}
