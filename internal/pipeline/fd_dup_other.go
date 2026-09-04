//go:build !linux

package pipeline

import "golang.org/x/sys/unix"

// dupOnto makes newfd refer to the same open file as oldfd, closing
// whatever newfd pointed at first. Outside Linux, dup2 is the only
// spelling and is present on every supported architecture.
func dupOnto(oldfd, newfd int) error {
	return unix.Dup2(oldfd, newfd)
}
