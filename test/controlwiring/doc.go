// Package controlwiring holds cross-seam tests of one question: does the
// path that actually runs in production invoke the control the tree
// declares? A flag parsed by a binary, a script under scripts/ci, a
// registry bool and a contract-identity factory anchor all read as
// "present" to go vet, promtool and every per-package unit test — the
// defect class is that the systemd ExecStart, the
// Cloudflare build command, the replay command or the decoder's Matches
// never reaches them. Only a test that reads BOTH sides of the seam can
// see that.
//
// A leg whose owning fix has not landed is build-tagged (k023evidence)
// and graduates to the default suite when it does; list the tagged legs
// with `grep -l '^//go:build k023evidence' test/controlwiring/*.go`.
package controlwiring
