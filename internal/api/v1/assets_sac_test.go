// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"context"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// sacStubReader lets each test choose what the lake claims the
// contract's SAC metadata name is.
type sacStubReader struct {
	ExplorerReader // nil — only SACClassicAssetName is called by the code under test
	name           string
	found          bool
}

func (s *sacStubReader) SACClassicAssetName(context.Context, string) (string, bool, error) {
	return s.name, s.found, nil
}

// TestResolveSACToClassic_Genuine pins the happy path: the USDC SAC
// resolves to the classic USDC identity because the metadata name
// re-derives to the queried address.
func TestResolveSACToClassic_Genuine(t *testing.T) {
	s := &Server{explorer: &sacStubReader{name: "USDC:GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN", found: true}}
	got, ok := s.resolveSACToClassic(context.Background(), "CCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUZPUTHXSTZLEO7SJMI75")
	if !ok || got.Code != "USDC" {
		t.Fatalf("resolve = %+v ok=%v, want classic USDC", got, ok)
	}
}

// TestResolveSACToClassic_SpoofedName pins the defence: a contract
// whose metadata CLAIMS to be USDC but whose address does not
// re-derive from that asset must NOT redirect pricing.
func TestResolveSACToClassic_SpoofedName(t *testing.T) {
	s := &Server{explorer: &sacStubReader{name: "USDC:GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN", found: true}}
	if _, ok := s.resolveSACToClassic(context.Background(), "CAFJZQWSED6YAWZU3GWRTOCNPPCGBN32L7QV43XX5LZLFTK6JLN34DLN"); ok {
		t.Fatal("spoofed metadata name redirected pricing — derivation cross-check failed")
	}
}

func TestResolveSACToClassic_Native(t *testing.T) {
	s := &Server{explorer: &sacStubReader{name: "native", found: true}}
	got, ok := s.resolveSACToClassic(context.Background(), "CAS3J7GYLGXMF6TDJBBYYSE3HQ6BBSMLNUQ34T6TZMYMW2EVH34XOWMA")
	if !ok || got.Type != canonical.AssetNative {
		t.Fatalf("native SAC resolve = %+v ok=%v", got, ok)
	}
}
