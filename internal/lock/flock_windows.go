//go:build windows

package lock

import (
	"errors"
	"os"
)

// On Windows the file lock degrades to "file exists": good enough for a
// single operator box; the in-process mutex still protects the service.
var errWouldBlock = errors.New("would block")

func flock(f *os.File) error   { return nil }
func funlock(f *os.File) error { return nil }
