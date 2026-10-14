package entryhistory

import "github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"

// SponsorshipEpisode is one stretch during which an arrangement was in force. A nil Start means the arrangement
// began below the derive floor; a nil End means it is still in force.
type SponsorshipEpisode struct {
	Start *clickhouse.SponsorshipEventRow
	End   *clickhouse.SponsorshipEventRow
}

// SponsorshipEpisodes folds an arrangement's events, in apply order, into episodes and reports whether the last
// event leaves it in force. A start while an episode is open leaves that episode without an end, and an end with
// no open episode opens one without a start: both are gaps in the derivation and are served, never hidden.
func SponsorshipEpisodes(events []clickhouse.SponsorshipEventRow) (episodes []SponsorshipEpisode, inForce bool) {
	for i := range events {
		e := &events[i]
		switch e.Event {
		case SponsorshipStarted:
			episodes = append(episodes, SponsorshipEpisode{Start: e})
			inForce = true
		case SponsorshipRevoked, SponsorshipEnded:
			if n := len(episodes); n > 0 && episodes[n-1].End == nil {
				episodes[n-1].End = e
			} else {
				episodes = append(episodes, SponsorshipEpisode{End: e})
			}
			inForce = false
		}
	}
	return episodes, inForce
}

// SponsorshipProof sets the account entry's own sponsorship counters against the reserves of the in-force
// arrangements derived through the watermark. Every change to either counter also modifies the account entry,
// so an entry last modified at or below the watermark must agree with the derivation exactly.
type SponsorshipProof struct {
	NumSponsoring      uint32
	NumSponsored       uint32
	DerivedSponsoring  uint64
	DerivedSponsored   uint64
	LastModifiedLedger uint32
	ThroughLedger      uint32
	// Match is nil when the two are not comparable: the account does not exist, nothing is derived, or the entry
	// was modified after the watermark.
	Match *bool
}

// ProveSponsorships builds the proof for one account from its entry and the two sides' totals through wm.
func ProveSponsorships(st clickhouse.AccountState, wm uint32, sponsoring, sponsored clickhouse.SponsorshipTotals) SponsorshipProof {
	p := SponsorshipProof{
		NumSponsoring: st.NumSponsoring, NumSponsored: st.NumSponsored,
		DerivedSponsoring: sponsoring.InForceReserves, DerivedSponsored: sponsored.InForceReserves,
		LastModifiedLedger: st.LastModifiedLedger, ThroughLedger: wm,
	}
	if !st.Exists || wm == 0 || st.LastModifiedLedger > wm {
		return p
	}
	match := uint64(st.NumSponsoring) == sponsoring.InForceReserves && uint64(st.NumSponsored) == sponsored.InForceReserves
	p.Match = &match
	return p
}
