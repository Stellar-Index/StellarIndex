package v1

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// unreadCatalogueReader fails the supply seam and the listing read on demand; any other method panics on the nil interface.
type unreadCatalogueReader struct {
	AssetsReader
	supplyErr, listErr, nativeErr error
	rows                          []timescale.AssetRow
}

func (u *unreadCatalogueReader) LatestSupplyObservations(
	context.Context, time.Duration,
) (map[string]timescale.SupplyObservation, error) {
	return nil, u.supplyErr
}

func (u *unreadCatalogueReader) ListAssetsExt(context.Context, timescale.ListAssetsOptions) ([]timescale.AssetRow, error) {
	return u.rows, u.listErr
}

func (u *unreadCatalogueReader) GetNativeAssetRow(context.Context) (timescale.AssetRow, error) {
	return timescale.AssetRow{AssetID: "native"}, u.nativeErr
}

type failingVolume struct{}

func (failingVolume) Volume24hUSDForAsset(context.Context, string) (string, bool, error) {
	return "", false, errors.New("pool timeout")
}

func unreadTestServer(o Options) *Server {
	return &Server{Options: o, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
}

// TestFillMarketCaps_UnreadPreciseSupplyIsReported: a failed observer read falls back to a lesser basis, and the caller must know.
func TestFillMarketCaps_UnreadPreciseSupplyIsReported(t *testing.T) {
	rows := []AssetDetail{{AssetID: withheldTestAsset}}
	failed := unreadTestServer(Options{AssetsReader: &unreadCatalogueReader{supplyErr: errors.New("pool timeout")}})
	if !failed.fillMarketCapsFromSupply(context.Background(), rows, nil) {
		t.Error("supply read failed: fill reported a clean read")
	}
	if !failed.rwaFillMissingSupply(context.Background(), rows) {
		t.Error("supply read failed: RWA supply fill reported a clean read")
	}
	if !failed.stablecoinFillAliasSupply(context.Background(), map[string]AssetDetail{"k": rows[0]}) {
		t.Error("supply read failed: stablecoin supply fill reported a clean read")
	}
	clean := unreadTestServer(Options{AssetsReader: &unreadCatalogueReader{}})
	if clean.fillMarketCapsFromSupply(context.Background(), rows, nil) {
		t.Error("supply read answered (empty): fill reported it unread")
	}
}

// TestLookupCatalogueTwin_UnreadIsNotAbsent: a failed listing read is not "this entry has no Stellar row".
func TestLookupCatalogueTwin_UnreadIsNotAbsent(t *testing.T) {
	boom := errors.New("pool timeout")
	cases := []struct {
		name   string
		reader *unreadCatalogueReader
		id     string
		found  bool
		unread bool
	}{
		{"listing failed", &unreadCatalogueReader{listErr: boom}, withheldTestAsset, false, true},
		{"native failed", &unreadCatalogueReader{nativeErr: boom}, "native", false, true},
		{"native has no row", &unreadCatalogueReader{nativeErr: sql.ErrNoRows}, "native", false, false},
		{"not served", &unreadCatalogueReader{}, withheldTestAsset, false, false},
		{"served", &unreadCatalogueReader{rows: []timescale.AssetRow{{AssetID: withheldTestAsset}}}, withheldTestAsset, true, false},
	}
	for _, c := range cases {
		row, unread := unreadTestServer(Options{AssetsReader: c.reader}).lookupCatalogueTwin(context.Background(), c.id)
		if (row != nil) != c.found || unread != c.unread {
			t.Errorf("%s: found=%v unread=%v, want %v %v", c.name, row != nil, unread, c.found, c.unread)
		}
	}
}

// TestAssetExtension_UnreadIsNotCached: a failed catalogue or extension read marks the detail stale and keeps it out of the cache.
func TestAssetExtension_UnreadIsNotCached(t *testing.T) {
	s := unreadTestServer(Options{})
	asset := mustAsset(t, withheldTestAsset)
	boom := errors.New("pool timeout")

	var rowFailed AssetDetail
	s.applyAssetRowToDetail(context.Background(), &rowFailed, asset, timescale.AssetRow{}, boom, asset.String())
	var extFailed AssetDetail
	s.logAssetExtensionFailures(&extFailed, asset.String(), assetExtensionResults{marketsErr: boom})
	for name, d := range map[string]*AssetDetail{"row": &rowFailed, "extension": &extFailed} {
		if !assetDetailStale(d, false, false) || !transientlyDegraded(d, false) {
			t.Errorf("%s read failed: stale=%v transient=%v, want both", name, assetDetailStale(d, false, false), transientlyDegraded(d, false))
		}
	}

	var clean AssetDetail
	s.logAssetExtensionFailures(&clean, asset.String(), assetExtensionResults{})
	if clean.extensionUnread {
		t.Error("every extension read answered: detail marked unread")
	}
}

// TestMarketCapSeriesRefused_UnreadGuardInput: a volume read that failed leaves the liquidity guard without a verdict.
func TestMarketCapSeriesRefused_UnreadGuardInput(t *testing.T) {
	wire := []HistoryPointWire{{P: "1000"}}
	base := canonical.Asset{Type: canonical.AssetNative}
	s := unreadTestServer(Options{Volume: failingVolume{}, MinMarketCapVolumeUSD: 1000})
	if _, _, unread := s.marketCapSeriesRefused(context.Background(), base, wire); !unread {
		t.Error("volume read failed: guard reported its inputs read")
	}
	if _, _, unread := unreadTestServer(Options{}).marketCapSeriesRefused(context.Background(), base, wire); unread {
		t.Error("no guard configured: nothing was read, nothing is unread")
	}
}
