//go:build unix

package store

import (
	"os"
	"syscall"
)

// lockFile takes an exclusive advisory lock on path (created if missing) and returns the function that releases it.
// It blocks until the lock is free. The lock belongs to the open file, so two stores in one process exclude each
// other as well as two processes.
func lockFile(path string) (func(), error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	for {
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX)
		if err != syscall.EINTR {
			break
		}
	}
	if err != nil {
		f.Close()
		return nil, err
	}
	return func() { syscall.Flock(int(f.Fd()), syscall.LOCK_UN); f.Close() }, nil
}
