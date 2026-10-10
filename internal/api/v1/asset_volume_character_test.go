package v1_test

// §2 volume_character — the trailing-window account-structure overlay on
// /v1/assets/{id}. Asserts the derived character + signals surface, and
// that this analytics field is orthogonal to pricing (it must not re-rank
// or suppress anything — §4 is out of scope).

import (
	"context"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

type stubVolumeCharacterReader struct {
	vc       timescale.AssetVolumeCharacter
	notFound bool
	err      error
}

func (s *stubVolumeCharacterReader) AssetVolumeCharacterRollup(_ context.Context, _ string) (timescale.AssetVolumeCharacter, bool, error) {
	return s.vc, !s.notFound, s.err
}

// scamAUDVolumeCharacter is the concentrated verdict the census produces
// for the reported scam AUD.
func scamAUDVolumeCharacter() timescale.AssetVolumeCharacter {
	return timescale.AssetVolumeCharacter{
		WindowDays:             14,
		VolumeUSD:              "2870000",
		DistinctMakers:         2,
		DistinctTakers:         1,
		TopAccountPairVolShare: 0.99,
		SelfCrossShare:         0,
		IssuerSideShare:        0.99,
		MarketStyledShare:      1.0,
		IsMarketStyled:         true,
		Character:              timescale.VolumeCharacterConcentrated,
	}
}
