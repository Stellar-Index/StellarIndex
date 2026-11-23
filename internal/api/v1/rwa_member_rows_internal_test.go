package v1

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// memberRowsCatalogue answers the listing like the store: Code filters exactly, Limit truncates.
type memberRowsCatalogue struct {
	AssetsReader
	rows []timescale.AssetRow
}

func (c memberRowsCatalogue) ListAssetsExt(_ context.Context, o timescale.ListAssetsOptions) ([]timescale.AssetRow, error) {
	var out []timescale.AssetRow
	for _, r := range c.rows {
		if o.Code == "" || r.Code == o.Code {
			out = append(out, r)
		}
	}
	return out[:min(len(out), o.Limit)], nil
}

func TestRWAIssuerMemberRows_fullPageReadsMembersByIdentity(t *testing.T) {
	filler := make([]string, 0, rwaAssetsPerIssuer)
	for i := range rwaAssetsPerIssuer {
		filler = append(filler, fmt.Sprintf("Z%d", i))
	}
	for name, tc := range map[string]struct {
		declared  []string
		tail      []string
		foldErr   error
		want      []string
		unchecked bool
		wantErr   bool
	}{
		"exact member past the page":       {declared: []string{"TBILL"}, tail: []string{"tbill", "TBILL"}, want: []string{"TBILL"}},
		"mis-cased member, one variant":    {declared: []string{"Tbill"}, tail: []string{"TBILL"}, want: []string{"TBILL"}},
		"mis-cased member, two variants":   {declared: []string{"Tbill"}, tail: []string{"TBILL", "tbill"}},
		"two members fold to one spelling": {declared: []string{"Tbill", "tBill"}, tail: []string{"TBILL"}},
		"exact member keeps its row":       {declared: []string{"TBILL", "tbill"}, tail: []string{"TBILL"}, want: []string{"TBILL"}},
		"variant read failed":              {declared: []string{"Tbill"}, tail: []string{"TBILL"}, foldErr: errors.New("down"), wantErr: true},
	} {
		t.Run(name, func(t *testing.T) {
			chain := append(slices.Clone(filler), tc.tail...)
			s := &Server{
				AssetsReader: memberRowsCatalogue{rows: rwaFoldRows(chain...)},
				Sep1Cache:    &foldChainStub{chain: chain, readErr: tc.foldErr},
			}
			got, err := s.rwaIssuerMemberRows(context.Background(), foldIssuer, tc.declared)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, want error %v", err, tc.wantErr)
			}
			codes := got.keepCodes()
			slices.Sort(codes)
			if !slices.Equal(codes, tc.want) || got.tailUnchecked != tc.unchecked {
				t.Errorf("rows = %v unchecked = %v, want %v %v", codes, got.tailUnchecked, tc.want, tc.unchecked)
			}
		})
	}
}
