package instance

import (
	"os"

	"golang.org/x/sys/windows"
)

// tryLock takes the lock file for this process, without waiting. got is
// false when another process holds it; an error means the file couldn't be
// used at all (no guard from it then). The lock goes when release is called
// or the process ends, however it ends: no stale lock is left behind.
func tryLock(path string) (release func(), got bool, err error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, false, err
	}
	h := windows.Handle(f.Fd())
	flags := uint32(windows.LOCKFILE_EXCLUSIVE_LOCK | windows.LOCKFILE_FAIL_IMMEDIATELY)
	if err := windows.LockFileEx(h, flags, 0, 1, 0, new(windows.Overlapped)); err != nil {
		f.Close()
		if err == windows.ERROR_LOCK_VIOLATION || err == windows.ERROR_IO_PENDING {
			return nil, false, nil
		}
		return nil, false, err
	}
	return func() {
		_ = windows.UnlockFileEx(h, 0, 1, 0, new(windows.Overlapped))
		f.Close()
	}, true, nil
}
