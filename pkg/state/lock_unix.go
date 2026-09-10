//go:build !windows

package state

import (
	"fmt"
	"os"
	"syscall"
)

func lockStateFile(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0755)
	if err != nil {
		return nil, fmt.Errorf("failed to open state lock file '%s': %w", path, err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("failed to lock state file '%s': %w", path, err)
	}
	return f, nil
}

func unlockStateFile(f *os.File) error {
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_UN); err != nil {
		_ = f.Close()
		return fmt.Errorf("failed to unlock state file: %w", err)
	}
	return f.Close()
}
