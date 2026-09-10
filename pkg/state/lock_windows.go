//go:build windows

package state

import (
	"fmt"
	"os"

	"golang.org/x/sys/windows"
)

func lockStateFile(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0755)
	if err != nil {
		return nil, fmt.Errorf("failed to open state lock file '%s': %w", path, err)
	}
	var overlapped windows.Overlapped
	if err := windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK, 0, 1, 0, &overlapped); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("failed to lock state file '%s': %w", path, err)
	}
	return f, nil
}

func unlockStateFile(f *os.File) error {
	var overlapped windows.Overlapped
	if err := windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, &overlapped); err != nil {
		_ = f.Close()
		return fmt.Errorf("failed to unlock state file: %w", err)
	}
	return f.Close()
}
