//go:build !aix && !android && !darwin && !dragonfly && !freebsd && !hurd && !illumos && !ios && !linux && !netbsd && !openbsd && !solaris

package storage

import (
	"fmt"
	"os"
)

func acquireStoreLock(string) (*os.File, error) {
	return nil, fmt.Errorf("%w: this operating system has no supported advisory lock", ErrLockUnsupported)
}

func releaseStoreLock(*os.File) error { return nil }
