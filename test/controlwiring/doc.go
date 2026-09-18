// Package controlwiring holds cross-seam tests of one question: does the
// path that actually runs in production invoke the control the tree
// declares? A flag parsed by a binary, a script under scripts/ci, a
// registry bool and a contract-identity factory anchor all read as
// "present" to go vet, promtool and every per-package unit test — the
// defect class (audit class K023) is that the systemd ExecStart, the
// Cloudflare build command, the replay command or the decoder's Matches
// never reaches them. Only a test that reads BOTH sides of the seam can
// see that.
//
// The tests are build-tagged (k023evidence) until every leg's owning fix
// has landed; see deployed_controls_test.go for the per-leg status and
// the command that prints it.
package controlwiring
