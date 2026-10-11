//go:build linux

package pipeline

import "golang.org/x/sys/unix"

// dupOnto makes newfd refer to the same open file as oldfd (dup2 contract). It uses dup3 with zero flags because arm64 and riscv64 have no dup2 syscall,
// so syscall.Dup2 does not compile there; dup3 exists on every Linux architecture.
func dupOnto(oldfd, newfd int) error {
	return unix.Dup3(oldfd, newfd, 0)
}
