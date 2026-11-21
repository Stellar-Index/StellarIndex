// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// Folding a SAC twin's volume onto its classic row adds the twin's
// unpriced exclusions too, so the merged sum is flagged.
func TestMergeAliasVolume_ORsLowerBound(t *testing.T) {
	t.Parallel()
	dst := AssetDetail{AssetID: "USDC-G", VolumeUSD24h: strp("10")}
	mergeAliasVolume(&dst, AssetDetail{AssetID: "CSAC", VolumeUSD24h: strp("5"), VolumeLowerBound: true})
	if dst.VolumeUSD24h == nil || *dst.VolumeUSD24h != "15" || !dst.VolumeLowerBound {
		t.Errorf("merged = %v lowerBound=%v, want 15 flagged", dst.VolumeUSD24h, dst.VolumeLowerBound)
	}
	mergeAliasVolume(&dst, AssetDetail{AssetID: "CSAC2", VolumeUSD24h: strp("1")})
	if !dst.VolumeLowerBound {
		t.Error("a clean alias must not clear an earlier flag")
	}
}

type lowerBoundVolumeReader struct{ lowerBound bool }

func (r lowerBoundVolumeReader) Volume24hUSDForAsset(context.Context, string) (string, bool, error) {
	return "42", r.lowerBound, nil
}

// keyedVolumeReader answers per asset key: plain is the classic read, soroban the XLM-anchored one.
type keyedVolumeReader struct{ plain, soroban map[string]string }

func (r keyedVolumeReader) Volume24hUSDForAsset(_ context.Context, key string) (string, bool, error) {
	if v, ok := r.plain[key]; ok {
		return v, false, nil
	}
	return "0", false, nil
}

func (r keyedVolumeReader) SorobanVolume24hUSDForAsset(_ context.Context, key string) (string, bool, error) {
	if v, ok := r.soroban[key]; ok {
		return v, false, nil
	}
	return "0", false, nil
}

// A classic asset's detail volume must include what traded under its lake-confirmed derived SAC, and a SAC the lake could not
// confirm leaves the classic figure flagged a floor rather than served as the total.
func TestPopulateVolume24h_AddsConfirmedDerivedSAC(t *testing.T) {
	t.Parallel()
	asset, err := canonical.NewClassicAsset("TWINX", "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")
	if err != nil {
		t.Fatal(err)
	}
	sac, err := asset.SacContractID()
	if err != nil {
		t.Fatal(err)
	}
	reader := keyedVolumeReader{
		plain:   map[string]string{asset.String(): "100.5"},
		soroban: map[string]string{sac: "40.25"},
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	s := &Server{logger: logger, Options: Options{Volume: reader, Explorer: &sacStubReader{name: asset.Code + ":" + asset.Issuer, found: true}}}
	d := &AssetDetail{}
	if s.populateVolume24h(context.Background(), d, asset) {
		t.Fatal("populateVolume24h reported failure")
	}
	if d.VolumeUSD24h == nil || *d.VolumeUSD24h != "140.75" || d.VolumeLowerBound {
		t.Errorf("confirmed SAC: volume = %v lowerBound=%v, want 140.75 exact", d.VolumeUSD24h, d.VolumeLowerBound)
	}

	s = &Server{logger: logger, Options: Options{Volume: reader, Explorer: errSACReader{}}}
	d = &AssetDetail{}
	s.populateVolume24h(context.Background(), d, asset)
	if d.VolumeUSD24h == nil || *d.VolumeUSD24h != "100.5" || !d.VolumeLowerBound {
		t.Errorf("unread SAC: volume = %v lowerBound=%v, want 100.5 flagged", d.VolumeUSD24h, d.VolumeLowerBound)
	}

	s = &Server{logger: logger, Options: Options{Volume: reader, Explorer: &sacStubReader{}}}
	d = &AssetDetail{}
	s.populateVolume24h(context.Background(), d, asset)
	if d.VolumeUSD24h == nil || *d.VolumeUSD24h != "100.5" || d.VolumeLowerBound {
		t.Errorf("no SAC deployed: volume = %v lowerBound=%v, want 100.5 exact", d.VolumeUSD24h, d.VolumeLowerBound)
	}
}

// The classic detail page reads the plain reader; its flag must reach the
// response, or listing and detail disagree about the same asset.
func TestPopulateVolume24h_ClassicCarriesReaderLowerBound(t *testing.T) {
	t.Parallel()
	asset, err := canonical.NewClassicAsset("USDC", "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []bool{true, false} {
		s := &Server{Volume: lowerBoundVolumeReader{lowerBound: want}, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
		d := &AssetDetail{}
		if s.populateVolume24h(context.Background(), d, asset) {
			t.Fatal("populateVolume24h reported failure")
		}
		if d.VolumeLowerBound != want {
			t.Errorf("reader lowerBound=%v: detail volume_lower_bound = %v", want, d.VolumeLowerBound)
		}
	}
}
