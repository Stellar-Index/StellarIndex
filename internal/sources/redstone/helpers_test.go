package redstone

import (
	"strings"
	"testing"
)

// ─── feedIDsFromOpArgs ─────────────────────────────────────────

func TestFeedIDsFromOpArgs_tooFewArgs(t *testing.T) {
	for _, n := range []int{0, 1, 2} {
		args := make([]string, n)
		_, _, err := feedIDsFromOpArgs(args)
		if err == nil {
			t.Errorf("expected error for arity %d, got nil", n)
		}
		if !strings.Contains(err.Error(), "arity") {
			t.Errorf("error %q missing \"arity\" fragment", err.Error())
		}
	}
}

func TestFeedIDsFromOpArgs_invalidUpdaterArg(t *testing.T) {
	// args[0] is supposed to be a base64-encoded SCVal::Address.
	// Garbage triggers the args[0]-tagged error.
	args := []string{"not-base64", "ignored", "ignored"}
	_, _, err := feedIDsFromOpArgs(args)
	if err == nil {
		t.Fatal("expected error on malformed args[0], got nil")
	}
	if !strings.Contains(err.Error(), "args[0]") {
		t.Errorf("error %q missing \"args[0]\" fragment", err.Error())
	}
}

func TestFeedIDsFromOpArgs_invalidFeedsArg(t *testing.T) {
	// args[0] is a valid SCVal::Address (the relayer), args[1] is
	// junk → we should see an args[1]-tagged error.
	args := []string{
		encodeAddressArg(t, relayerG),
		"not-base64",
		"ignored",
	}
	_, _, err := feedIDsFromOpArgs(args)
	if err == nil {
		t.Fatal("expected error on malformed args[1], got nil")
	}
	if !strings.Contains(err.Error(), "args[1]") {
		t.Errorf("error %q missing \"args[1]\" fragment", err.Error())
	}
}

func TestFeedIDsFromOpArgs_happyPath(t *testing.T) {
	want := []string{"BTC", "ETH"}
	args := []string{
		encodeAddressArg(t, relayerG),
		encodeStringVecArg(t, want),
		encodePayloadArg(t),
	}
	feedIDs, updater, err := feedIDsFromOpArgs(args)
	if err != nil {
		t.Fatalf("feedIDsFromOpArgs: %v", err)
	}
	if updater != relayerG {
		t.Errorf("updater = %q, want %q", updater, relayerG)
	}
	if len(feedIDs) != 2 || feedIDs[0] != "BTC" || feedIDs[1] != "ETH" {
		t.Errorf("feedIDs = %v, want %v", feedIDs, want)
	}
}
