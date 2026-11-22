// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"context"
	"errors"
	"testing"
)

// sacLookupExplorer answers every SAC-metadata lookup with err (nil: a definite "not a SAC").
type sacLookupExplorer struct {
	ExplorerReader
	err error
}

func (e sacLookupExplorer) SACClassicAssetName(context.Context, string) (string, bool, error) {
	return "", false, e.err
}

// A contract row whose SAC lookup failed may be an unfolded alias of an on-page asset: the fold must report the read unread,
// which fetchClassicUnifiedRows folds into the page's flags, and a definite "not a SAC" must not.
func TestFoldAliasTwins_FailedSACLookupIsUnread(t *testing.T) {
	for name, tc := range map[string]struct {
		err    error
		unread bool
	}{
		"lookup answered not-a-SAC": {nil, false},
		"lookup failed":             {errors.New("lake read timed out"), true},
	} {
		t.Run(name, func(t *testing.T) {
			s := &Server{}
			s.logger = discardLogger()
			s.Explorer = sacLookupExplorer{err: tc.err}
			rows := []AssetDetail{{AssetID: cetesAsset}, {AssetID: "CC2RBGYNCFBCVENIDL5BFBWPH4OUZM2UA3OD2K2N54GLMWCC4KWPVAGO"}}
			ctx, rep := withReadReport(context.Background())
			out, _ := s.foldAliasTwins(ctx, rows, false)
			if len(out) != 2 {
				t.Fatalf("instrument: want both rows served as themselves, got %+v", out)
			}
			if rep.unread.Load() != tc.unread {
				t.Errorf("unread = %v, want %v", rep.unread.Load(), tc.unread)
			}
		})
	}
}
