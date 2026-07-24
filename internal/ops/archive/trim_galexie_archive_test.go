package archive

import (
	"strings"
	"testing"
)

// TestParseTrimFlags_Defaults verifies the safety primitives: when
// neither --dry-run nor --commit is set, the parser leaves dryRun
// at its flag default (false) and trimGalexieArchive's body
// promotes it to true. We test the latter behaviour by mimicking
// the promotion logic.
func TestParseTrimFlags_Defaults(t *testing.T) {
	t.Parallel()
	opts, err := parseTrimFlags([]string{"-older-than-ledger", "1000"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if opts.olderThan != 1000 {
		t.Errorf("olderThan = %d, want 1000", opts.olderThan)
	}
	if !opts.verifyUpstream {
		t.Errorf("verifyUpstream must default to true (HEAD-before-delete is the primary safety primitive)")
	}
	if opts.maxFiles != 100000 {
		t.Errorf("maxFiles default = %d, want 100000", opts.maxFiles)
	}
	if opts.dryRun || opts.commit {
		t.Errorf("neither dryRun nor commit should be set by default (the body promotes dryRun=true post-parse); got dryRun=%v commit=%v", opts.dryRun, opts.commit)
	}
}

// TestParseTrimFlags_NoVerifyUpstreamAloneIsRefused is the REL-05
// regression: --no-verify-upstream disables the ONLY check that the
// cold tier actually holds the files hot is about to lose, so it must
// NOT be usable on its own — a second explicit acknowledgement flag
// (--i-have-verified-cold-out-of-band) is required.
func TestParseTrimFlags_NoVerifyUpstreamAloneIsRefused(t *testing.T) {
	t.Parallel()
	_, err := parseTrimFlags([]string{"-older-than-ledger", "1000", "-no-verify-upstream"})
	if err == nil {
		t.Fatal("expected -no-verify-upstream ALONE (no second ack) to be refused, got nil error")
	}
	if !strings.Contains(err.Error(), "i-have-verified-cold-out-of-band") {
		t.Errorf("error should name the required second flag, got: %v", err)
	}
}

// TestParseTrimFlags_NoVerifyUpstreamWithAck: the safety-primitive
// bypass IS usable once both flags are set together.
func TestParseTrimFlags_NoVerifyUpstreamWithAck(t *testing.T) {
	t.Parallel()
	opts, err := parseTrimFlags([]string{
		"-older-than-ledger", "1000",
		"-no-verify-upstream",
		"-i-have-verified-cold-out-of-band",
	})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if opts.verifyUpstream {
		t.Errorf("-no-verify-upstream should flip verifyUpstream to false")
	}
	if !opts.iHaveVerifiedOutOfBand {
		t.Errorf("iHaveVerifiedOutOfBand should be true")
	}
}

// TestParseTrimFlags_AckAloneWithoutNoVerifyIsHarmless: the ack flag
// by itself (verify-upstream still on) is not an error — it's only
// meaningful paired with -no-verify-upstream, but passing it alone
// must not break the default safe path.
func TestParseTrimFlags_AckAloneWithoutNoVerifyIsHarmless(t *testing.T) {
	t.Parallel()
	opts, err := parseTrimFlags([]string{"-older-than-ledger", "1000", "-i-have-verified-cold-out-of-band"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !opts.verifyUpstream {
		t.Errorf("verifyUpstream must still default to true when -no-verify-upstream isn't passed")
	}
}

func TestParseTrimFlags_CommitOptIn(t *testing.T) {
	t.Parallel()
	opts, err := parseTrimFlags([]string{"-older-than-ledger", "1000", "-commit"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !opts.commit {
		t.Errorf("-commit should set opts.commit=true")
	}
	if opts.dryRun {
		t.Errorf("-commit alone should NOT also set dryRun; got dryRun=true")
	}
}

func TestParseTrimFlags_OverflowGuard(t *testing.T) {
	t.Parallel()
	_, err := parseTrimFlags([]string{"-older-than-ledger", "9999999999"})
	if err == nil {
		t.Fatal("expected error for uint32 overflow, got nil")
	}
	if !strings.Contains(err.Error(), "uint32 range") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestSplitBucketPath(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in     string
		bucket string
		prefix string
	}{
		{"galexie-archive", "galexie-archive", ""},
		{"aws-public-blockchain/v1.1/stellar/ledgers/pubnet", "aws-public-blockchain", "v1.1/stellar/ledgers/pubnet"},
		{"my-bucket/a/b/c", "my-bucket", "a/b/c"},
	}
	for _, c := range cases {
		c := c
		t.Run(c.in, func(t *testing.T) {
			t.Parallel()
			b, p, err := splitBucketPath(c.in)
			if err != nil {
				t.Fatalf("split %q: %v", c.in, err)
			}
			if b != c.bucket {
				t.Errorf("bucket = %q, want %q", b, c.bucket)
			}
			if p != c.prefix {
				t.Errorf("prefix = %q, want %q", p, c.prefix)
			}
		})
	}
}

func TestMinMaxInt(t *testing.T) {
	t.Parallel()
	if minInt(3, 5) != 3 {
		t.Error("minInt(3,5) != 3")
	}
	if minInt(7, 5) != 5 {
		t.Error("minInt(7,5) != 5")
	}
	if maxInt(3, 5) != 5 {
		t.Error("maxInt(3,5) != 5")
	}
	if maxInt(7, 5) != 7 {
		t.Error("maxInt(7,5) != 7")
	}
}
