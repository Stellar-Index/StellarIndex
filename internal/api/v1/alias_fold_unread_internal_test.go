// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// sacLookupExplorer answers every SAC-metadata lookup with err (nil: a definite "not a SAC").
type sacLookupExplorer struct {
	ExplorerReader
	err error
}

func (e sacLookupExplorer) SACClassicAssetName(context.Context, string) (string, bool, error) {
	return "", false, e.err
}

// A contract row whose SAC lookup failed may be an unfolded alias of an off-page asset: the fold must report the read unread,
// which fetchClassicUnifiedRows folds into the page's flags, and a definite "not a SAC" must not. On an unfiltered page the
// unconfirmed row is withheld, since the page holding its classic row may add its volume (counted once across pages); a
// filtered page, where no restore runs, still serves it.
func TestFoldAliasTwins_FailedSACLookupIsUnread(t *testing.T) {
	for name, tc := range map[string]struct {
		err      error
		keepLone bool
		rows     int
		unread   bool
	}{
		"lookup answered not-a-SAC":        {nil, false, 2, false},
		"lookup failed":                    {errors.New("lake read timed out"), false, 1, true},
		"lookup failed on a filtered page": {errors.New("lake read timed out"), true, 2, true},
	} {
		t.Run(name, func(t *testing.T) {
			s := &Server{}
			s.logger = discardLogger()
			s.Explorer = sacLookupExplorer{err: tc.err}
			rows := []AssetDetail{{AssetID: cetesAsset}, {AssetID: "CC2RBGYNCFBCVENIDL5BFBWPH4OUZM2UA3OD2K2N54GLMWCC4KWPVAGO"}}
			ctx, rep := withReadReport(context.Background())
			out, _ := s.foldAliasTwins(ctx, rows, tc.keepLone)
			if len(out) != tc.rows {
				t.Fatalf("want %d rows, got %+v", tc.rows, out)
			}
			if rep.unread.Load() != tc.unread {
				t.Errorf("unread = %v, want %v", rep.unread.Load(), tc.unread)
			}
		})
	}
}

// A contract row carries no code, issuer or name, so a failed SAC lookup leaves nothing to tell a plain SEP-41 token from an
// unfolded SAC: an unfiltered page withholds it too, and that omission must reach the wire flagged and uncached.
func TestClassicUnifiedPage_UnconfirmedContractRowIsWithheldFlagged(t *testing.T) {
	const sep41 = "CC2RBGYNCFBCVENIDL5BFBWPH4OUZM2UA3OD2K2N54GLMWCC4KWPVAGO"
	for name, tc := range map[string]struct {
		err      error
		withheld bool
	}{
		"lookup answered not-a-SAC": {nil, false},
		"lookup failed":             {errors.New("lake read timed out"), true},
	} {
		t.Run(name, func(t *testing.T) {
			s := &Server{Options: Options{
				AssetsReader: &classicPageReader{rows: []timescale.AssetRow{{AssetID: sep41, Slug: sep41}}},
				Explorer:     sacLookupExplorer{err: tc.err},
			}}
			s.logger = discardLogger()
			rec := httptest.NewRecorder()
			s.serveClassicUnifiedPage(rec, httptest.NewRequest(http.MethodGet, "/v1/assets?asset_class=all&cursor=classic:", nil),
				assetListFilters{}, 10, "")
			if served := strings.Contains(rec.Body.String(), sep41); served == tc.withheld {
				t.Fatalf("served = %v, want %v: %s", served, !tc.withheld, rec.Body.String())
			}
			if f := decodeFlags(t, rec); tc.withheld && (!f.Stale || !noStore(rec)) {
				t.Errorf("withheld row: stale = %v, no-store = %v; want both", f.Stale, noStore(rec))
			}
		})
	}
}
