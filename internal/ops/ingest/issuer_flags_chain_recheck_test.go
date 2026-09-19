package ingest

import (
	"context"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// The `issuer-flags` drain's CHAIN RE-CHECK pass (RSEC-V1 / RLT-470).
//
// `issuers.home_domain` stopped being write-once, but nothing scheduled ever
// re-read a row that already held one: the primary queue is `auth_required IS
// NULL` and the last-known re-check covers only merged accounts, so a FILLED,
// live-sourced row was invisible to both. `issuer-enrich`, the job whose whole
// purpose is to sync the column, is a manual one-shot with no timer. An anchor
// that moved domain with SetOptions and let the old name lapse therefore kept
// the lapsed name until someone ran a backfill by hand — while the hourly
// SEP-1 refresh kept fetching it, and whoever registered it next could serve a
// stellar.toml listing the anchor's issuer account back and inherit its
// verified org identity.
//
// The founding case's real keys: the ex-apay ETH issuer moved to
// ultracapital.xyz when Ultra Stellar acquired apay.io's wrapped assets.
const (
	// A filled, live-sourced row on r1 whose stored domain has lapsed.
	lapsedDomainIssuer       = "GARDNV3Q7YGT4AKSDF25LT32YSCCW4EV22Y2TV3I2PU2MMXJTEDL5T55"
	lapsedDomainIssuerLedger = uint32(64228661)
)

// onRecord builds one persisted row the way the served tier holds it.
func onRecord(g string, flags uint32, domain, source string, asOf uint32) timescale.IssuerAuthFlagsOnRecord {
	req, rev, imm, claw := flags&0x1 != 0, flags&0x2 != 0, flags&0x4 != 0, flags&0x8 != 0
	rec := timescale.IssuerAuthFlagsOnRecord{
		GStrkey:    g,
		Required:   &req,
		Revocable:  &rev,
		Immutable:  &imm,
		Clawback:   &claw,
		HomeDomain: domain,
		Source:     source,
	}
	if asOf > 0 {
		l := asOf
		rec.AsOfLedger = &l
	}
	return rec
}

// TestRunIssuerFlags_ChainRecheckCorrectsALapsedHomeDomain is the defect.
//
// The row is the exact shape the drain leaves behind: flags resolved, source
// `live`, and a home_domain that was true when it was written. The account has
// since declared a different one on-chain. A run must re-read it and write the
// chain's answer back — that is the on-chain remediation path the finding says
// has no effect.
func TestRunIssuerFlags_ChainRecheckCorrectsALapsedHomeDomain(t *testing.T) {
	store := &stubIssuerFlagsStore{
		needChainRead: []timescale.IssuerAuthFlagsOnRecord{
			onRecord(lapsedDomainIssuer, 0x1, "lapsed-former.example",
				timescale.AuthFlagsSourceLive, 64100000),
		},
	}
	reader := &stubIssuerFlagsReader{
		live: map[string]clickhouse.AccountAuthFlags{
			lapsedDomainIssuer: liveReading(lapsedDomainIssuerLedger, 0x1, "ultracapital.xyz"),
		},
	}
	if err := runIssuerFlags(context.Background(), store, reader, runOpts()); err != nil {
		t.Fatalf("runIssuerFlags: %v", err)
	}

	got, ok := store.allPersisted()[lapsedDomainIssuer]
	if !ok {
		t.Fatalf("the filled row was never re-read; persisted = %v — a lapsed domain is only "+
			"correctable on-chain if something re-offers a row the drain has already filled",
			store.allPersisted())
	}
	if got.HomeDomain != "ultracapital.xyz" {
		t.Errorf("home_domain = %q, want ultracapital.xyz — the account's own current entry, "+
			"not the copy taken before it moved", got.HomeDomain)
	}
	if got.Source != timescale.AuthFlagsSourceLive {
		t.Errorf("source = %q, want %q", got.Source, timescale.AuthFlagsSourceLive)
	}
	if got.AsOfLedger == nil || *got.AsOfLedger != lapsedDomainIssuerLedger {
		t.Errorf("as-of = %v, want %d (the entry read now, not the one on record)",
			got.AsOfLedger, lapsedDomainIssuerLedger)
	}
}

// TestRunIssuerFlags_ChainRecheckWritesOnlyWhatTheChainChanged — re-offering
// the whole filled set (r1: 49,002 rows) is only affordable because a run that
// changes nothing writes nothing. It also keeps the `written` counter meaning
// "rows the chain corrected" rather than "rows we touched".
func TestRunIssuerFlags_ChainRecheckWritesOnlyWhatTheChainChanged(t *testing.T) {
	const agreeing = "GBNZILSTVQZ4R7IKQDGHYGY2QXL5QOFJYQMXPKWRRM5PAV7Y4M67AQUA"
	store := &stubIssuerFlagsStore{
		needChainRead: []timescale.IssuerAuthFlagsOnRecord{
			onRecord(agreeing, 0x2, "aqua.network", timescale.AuthFlagsSourceLive, 64100000),
			onRecord(lapsedDomainIssuer, 0x1, "lapsed-former.example",
				timescale.AuthFlagsSourceLive, 64100000),
			// Not in the lake's current-state projection at all.
			onRecord(absentIssuer, 0, "somewhere.example", timescale.AuthFlagsSourceLive, 64100000),
		},
	}
	reader := &stubIssuerFlagsReader{
		live: map[string]clickhouse.AccountAuthFlags{
			agreeing:           liveReading(64100000, 0x2, "aqua.network"),
			lapsedDomainIssuer: liveReading(lapsedDomainIssuerLedger, 0x1, "ultracapital.xyz"),
		},
	}
	if err := runIssuerFlags(context.Background(), store, reader, runOpts()); err != nil {
		t.Fatalf("runIssuerFlags: %v", err)
	}

	got := store.allPersisted()
	if len(got) != 1 {
		t.Fatalf("persisted %d row(s), want exactly 1 — only the row the chain moved past: %v", len(got), got)
	}
	if _, ok := got[lapsedDomainIssuer]; !ok {
		t.Errorf("persisted %v, want the corrected row %s", got, lapsedDomainIssuer)
	}
	if _, ok := got[absentIssuer]; ok {
		t.Errorf("%s was rewritten, but the live reader did not answer for it — absence from the "+
			"current-state projection is a merged account AND a coverage gap, so this pass may not act on it",
			absentIssuer)
	}
}

// TestRunIssuerFlags_ChainRecheckNeverBlanksAStoredDomain keeps the pass from
// over-reaching. A live entry that declares NO domain is not a retraction: the
// lake's lookup only returns accounts that DECLARE one, so an empty value
// means "not read". Nothing about such a row has changed, so it must be
// neither blanked nor rewritten.
func TestRunIssuerFlags_ChainRecheckNeverBlanksAStoredDomain(t *testing.T) {
	store := &stubIssuerFlagsStore{
		needChainRead: []timescale.IssuerAuthFlagsOnRecord{
			onRecord(lapsedDomainIssuer, 0x1, "still-declared.example",
				timescale.AuthFlagsSourceLive, 64100000),
		},
	}
	reader := &stubIssuerFlagsReader{
		live: map[string]clickhouse.AccountAuthFlags{
			lapsedDomainIssuer: liveReading(64100000, 0x1, ""),
		},
	}
	if err := runIssuerFlags(context.Background(), store, reader, runOpts()); err != nil {
		t.Fatalf("runIssuerFlags: %v", err)
	}
	if got := store.allPersisted(); len(got) != 0 {
		t.Errorf("persisted %v, want nothing — an entry that declares no domain is not a retraction "+
			"of the one on record, and the persist statement would not change the column anyway", got)
	}
}

// TestRunIssuerFlags_ChainRecheckFillsAnUnlabelledRow — a pre-migration-0153
// row carries flags with no provenance label. Its VALUES may well agree with
// the chain, but "unknown provenance" is not the same claim as `live`, so the
// pass must still write it once and stamp it.
func TestRunIssuerFlags_ChainRecheckFillsAnUnlabelledRow(t *testing.T) {
	store := &stubIssuerFlagsStore{
		needChainRead: []timescale.IssuerAuthFlagsOnRecord{
			onRecord(lapsedDomainIssuer, 0x1, "ultracapital.xyz", "", 0),
		},
	}
	reader := &stubIssuerFlagsReader{
		live: map[string]clickhouse.AccountAuthFlags{
			lapsedDomainIssuer: liveReading(lapsedDomainIssuerLedger, 0x1, "ultracapital.xyz"),
		},
	}
	if err := runIssuerFlags(context.Background(), store, reader, runOpts()); err != nil {
		t.Fatalf("runIssuerFlags: %v", err)
	}
	got, ok := store.allPersisted()[lapsedDomainIssuer]
	if !ok {
		t.Fatalf("the unlabelled row was not stamped; persisted = %v", store.allPersisted())
	}
	if got.Source != timescale.AuthFlagsSourceLive {
		t.Errorf("source = %q, want %q — an absent label is UNKNOWN, never a claim that the "+
			"reading is current", got.Source, timescale.AuthFlagsSourceLive)
	}
}

// TestRunIssuerFlags_ChainRecheckHasItsOwnBound — the widened queue must not
// silently inherit -limit (5,000 nightly on r1), because it is ordered by
// primary key: a cap would re-read the same head every run and never reach the
// tail. It gets its own knob, defaulting to "every filled row", and the pass
// runs LAST so it cannot take budget from the primary drain either way.
func TestRunIssuerFlags_ChainRecheckHasItsOwnBound(t *testing.T) {
	store := &stubIssuerFlagsStore{}
	reader := &stubIssuerFlagsReader{}
	o := runOpts()
	o.limit = 250
	o.chainRecheckLimit = 40
	if err := runIssuerFlags(context.Background(), store, reader, o); err != nil {
		t.Fatalf("runIssuerFlags: %v", err)
	}
	if store.flagsLimit != 250 {
		t.Errorf("primary queue limit = %d, want 250", store.flagsLimit)
	}
	if store.chainRecheckLimit != 40 {
		t.Errorf("chain re-check limit = %d, want 40 — the pass must be bounded independently of -limit",
			store.chainRecheckLimit)
	}
}

// TestRunIssuerFlags_ChainRecheckDryRunWritesNothing — the write gate covers
// the third pass too; an ungated pass would be a nightly writer an operator
// could not rehearse.
func TestRunIssuerFlags_ChainRecheckDryRunWritesNothing(t *testing.T) {
	store := &stubIssuerFlagsStore{
		needChainRead: []timescale.IssuerAuthFlagsOnRecord{
			onRecord(lapsedDomainIssuer, 0x1, "lapsed-former.example",
				timescale.AuthFlagsSourceLive, 64100000),
		},
	}
	reader := &stubIssuerFlagsReader{
		live: map[string]clickhouse.AccountAuthFlags{
			lapsedDomainIssuer: liveReading(lapsedDomainIssuerLedger, 0x1, "ultracapital.xyz"),
		},
	}
	o := runOpts()
	o.dryRun = true
	if err := runIssuerFlags(context.Background(), store, reader, o); err != nil {
		t.Fatalf("runIssuerFlags: %v", err)
	}
	if len(store.persisted) != 0 {
		t.Errorf("dry run persisted %d batch(es), want 0", len(store.persisted))
	}
}
