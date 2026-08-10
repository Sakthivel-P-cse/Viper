//go:build aix || android || darwin || dragonfly || freebsd || hurd || illumos || ios || linux || netbsd || openbsd || solaris

package storage

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

func acquireStoreLock(directory string) (*os.File, error) {
	lock, err := os.OpenFile(filepath.Join(directory, lockFilename), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open storage lock: %w", err)
	}
	if err = lock.Chmod(0o600); err != nil {
		_ = lock.Close()
		return nil, fmt.Errorf("secure storage lock: %w", err)
	}
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = lock.Close()
		if errors.Is(err, syscall.EAGAIN) || errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, ErrLocked
		}
		return nil, fmt.Errorf("lock storage directory: %w", err)
	}
	return lock, nil
}

func releaseStoreLock(lock *os.File) error {
	if lock == nil {
		return nil
	}
	return errors.Join(syscall.Flock(int(lock.Fd()), syscall.LOCK_UN), lock.Close())
}
