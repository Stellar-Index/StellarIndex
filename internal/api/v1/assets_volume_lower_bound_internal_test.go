// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// A listing or /v1/contracts row whose rollup counted unpriced trades must
// serve its volume as a lower bound; one with none must not.
func TestAssetDetailFromAssetRow_CarriesVolumeLowerBound(t *testing.T) {
	t.Parallel()
	flagged := assetDetailFromAssetRow(timescale.AssetRow{
		AssetID: "native", Volume24hUSD: strp("100"), VolumeLowerBound: true,
	})
	if !flagged.VolumeLowerBound {
		t.Error("row with unpriced trades: volume_lower_bound = false, want true")
	}
	clean := assetDetailFromAssetRow(timescale.AssetRow{AssetID: "native", Volume24hUSD: strp("100")})
	if clean.VolumeLowerBound {
		t.Error("fully priced row: volume_lower_bound = true, want false")
	}
}

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

// A catalogue row that takes its twin's volume takes the twin's flag with it;
// one with its own volume keeps its own.
func TestMergeTwinStats_CarriesLowerBoundWithVolume(t *testing.T) {
	t.Parallel()
	dst := AssetDetail{}
	mergeTwinStats(&dst, AssetDetail{VolumeUSD24h: strp("7"), VolumeLowerBound: true})
	if !dst.VolumeLowerBound {
		t.Error("borrowed twin volume lost its lower-bound flag")
	}
	own := AssetDetail{VolumeUSD24h: strp("9")}
	mergeTwinStats(&own, AssetDetail{VolumeUSD24h: strp("7"), VolumeLowerBound: true})
	if own.VolumeLowerBound {
		t.Error("row kept its own volume but took the twin's flag")
	}
}

type lowerBoundVolumeReader struct{ lowerBound bool }

func (r lowerBoundVolumeReader) Volume24hUSDForAsset(context.Context, string) (string, bool, error) {
	return "42", r.lowerBound, nil
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
