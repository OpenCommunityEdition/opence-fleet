//go:build !unix

package update

import (
	"errors"
	"os"
)

// Exec is for Unix only.
func Exec(string, *os.File) error { return errors.New("restarting into an update needs Unix") }
