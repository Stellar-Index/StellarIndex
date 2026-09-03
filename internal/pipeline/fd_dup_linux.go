//go:build linux

package pipeline

import "golang.org/x/sys/unix"

// dupOnto makes newfd refer to the same open file as oldfd, closing
// whatever newfd pointed at first — the classic dup2 contract.
//
// Linux is the one platform where that contract has two spellings.
// dup2 exists in the amd64 and 32-bit syscall tables, but arm64 and
// riscv64 were added after dup3 superseded it and never received a
// dup2 entry, so syscall.Dup2 does not compile there at all. dup3 is
// present on every Linux architecture and, with flags of zero, is
// dup2 exactly. The build passing on amd64 CI and on the amd64
// production host is what let the arm64 break go unnoticed.
func dupOnto(oldfd, newfd int) error {
	return unix.Dup3(oldfd, newfd, 0)
}
