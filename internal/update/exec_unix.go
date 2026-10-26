//go:build unix

package update

import (
	"os"
	"syscall"
)

// Exec replaces the process with binary, with the same arguments; listener
// (if any) stays open in the new process.
func Exec(binary string, listener *os.File) error {
	if listener != nil {
		// (Go opens files close-on-exec)
		if _, _, errno := syscall.Syscall(syscall.SYS_FCNTL, listener.Fd(), syscall.F_SETFD, 0); errno != 0 {
			return errno
		}
	}
	return syscall.Exec(binary, append([]string{binary}, os.Args[1:]...), environment(listener))
}
