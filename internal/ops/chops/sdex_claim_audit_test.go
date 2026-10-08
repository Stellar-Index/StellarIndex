// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package chops

import (
	"errors"
	"strings"
	"testing"

	"github.com/stellar/go-stellar-sdk/xdr"
)

// unreadableLedgerLCM builds a ledger whose SDK reader fails to construct
// at all: protocol version < 10, TxApplyProcessing.V < 2 and non-empty
// FeeProcessing together trip the reader's badMetaVersionErr
// (NewLedgerTransactionReaderFromLedgerCloseMeta -> storeTransactions).
// This is the "whole ledger unreadable" case sdexClaimAudit's reader-error
// branch exists for.
func unreadableLedgerLCM(t *testing.T) xdr.LedgerCloseMeta {
	t.Helper()
	env, proc := gateTx(t, 0x44, 0, false)
	proc.TxApplyProcessing = xdr.TransactionMeta{V: 0}
	proc.FeeProcessing = xdr.LedgerEntryChanges{{}}
	lcm := gateLCM([]xdr.TransactionEnvelope{env}, []xdr.TransactionResultMeta{proc})
	lcm.V1.LedgerHeader.Header.LedgerVersion = 9
	return lcm
}

// claimAuditFailedTx builds an ordinary failed transaction (0 ops):
// tx.Result.Successful() is false, but the SDK reads it fine. This must
// stay silently skipped — it legitimately has no claims — and must NOT be
// counted as a read failure.
func claimAuditFailedTx(t *testing.T, seed byte) (xdr.TransactionEnvelope, xdr.TransactionResultMeta) {
	t.Helper()
	env, proc := gateTx(t, seed, 0, false)
	proc.Result.Result.Result = xdr.TransactionResultResult{Code: xdr.TransactionResultCodeTxFailed}
	return env, proc
}

func TestAuditLedgerClaims_ReaderFailureReported(t *testing.T) {
	lcm := unreadableLedgerLCM(t)

	tally := auditLedgerClaims(lcm, gateTestPassphrase, false, 20)
	if tally.readerErr == nil {
		t.Fatal("auditLedgerClaims: readerErr = nil, want the badMetaVersionErr from a ledger " +
			"the SDK reader cannot even construct")
	}
	if tally.txReadFailures != 0 || tally.claims != 0 || tally.drops != 0 {
		t.Errorf("tally = %+v, want everything else zero when the reader never opened", tally)
	}
}

func TestAuditLedgerClaims_TxReadFailureDistinctFromFailedTx(t *testing.T) {
	goodEnv, goodProc := gateTx(t, 0x11, 0, false)
	unreadableEnv, unreadableProc := gateTx(t, 0x22, 0, true)
	failedEnv, failedProc := claimAuditFailedTx(t, 0x33)

	lcm := gateLCM(
		[]xdr.TransactionEnvelope{goodEnv, unreadableEnv, failedEnv},
		[]xdr.TransactionResultMeta{goodProc, unreadableProc, failedProc},
	)

	tally := auditLedgerClaims(lcm, gateTestPassphrase, false, 20)
	if tally.readerErr != nil {
		t.Fatalf("auditLedgerClaims: readerErr = %v, want nil — the reader itself opens fine", tally.readerErr)
	}
	if tally.txReadFailures != 1 {
		t.Errorf("txReadFailures = %d, want 1 (only the tampered-hash tx is an SDK read error; "+
			"the ordinary failed tx must stay silently skipped, not counted here)", tally.txReadFailures)
	}
}

// TestClaimAuditVerdict_SilentDropsFailTheRun pins the fix itself: a full,
// requested-range-covered walk (coverageErr == nil, as walkCoverage returns
// when delivered == requested) that nonetheless silently excluded a ledger
// or a transaction must not exit clean. A verdict that ignored
// these counts would return nil for this exact combination, certifying the
// headline total complete for a range where the reader or SDK read had
// silently failed.
func TestClaimAuditVerdict_SilentDropsFailTheRun(t *testing.T) {
	t.Run("no failures, full coverage: passes", func(t *testing.T) {
		if err := claimAuditVerdict(nil, 0, 0, 1000); err != nil {
			t.Fatalf("claimAuditVerdict = %v, want nil when nothing was dropped", err)
		}
	})

	t.Run("coverage error alone still surfaces", func(t *testing.T) {
		coverageErr := errors.New("sdex-claim-audit walked 0 ledgers")
		err := claimAuditVerdict(coverageErr, 0, 0, 0)
		if !errors.Is(err, coverageErr) {
			t.Fatalf("claimAuditVerdict = %v, want it to wrap the coverage error", err)
		}
	})

	t.Run("reader failures fail the run even with full coverage", func(t *testing.T) {
		err := claimAuditVerdict(nil, 2, 0, 1000)
		if err == nil {
			t.Fatal("claimAuditVerdict = nil despite 2 reader failures — this is exactly the " +
				"bug: a fully-covered walk that silently dropped 2 ledgers from the Hubble-" +
				"comparable total must not certify as clean")
		}
		if !strings.Contains(err.Error(), "2 ledger(s)") || !strings.Contains(err.Error(), "1000") {
			t.Errorf("err = %q, want the ledger-failure count and the claim total named", err.Error())
		}
	})

	t.Run("tx read failures fail the run even with full coverage", func(t *testing.T) {
		err := claimAuditVerdict(nil, 0, 3, 500)
		if err == nil {
			t.Fatal("claimAuditVerdict = nil despite 3 tx read failures — the total is an " +
				"undercount and must not be certified as the Hubble-comparable figure")
		}
		if !strings.Contains(err.Error(), "3 transaction(s)") {
			t.Errorf("err = %q, want the tx-failure count named", err.Error())
		}
	})

	t.Run("both failure classes and a coverage error join", func(t *testing.T) {
		coverageErr := errors.New("sdex-claim-audit walked 37 of 100 requested ledgers")
		err := claimAuditVerdict(coverageErr, 1, 1, 42)
		if !errors.Is(err, coverageErr) {
			t.Fatalf("err = %v, want it to still wrap the coverage error alongside the new one", err)
		}
	})
}
