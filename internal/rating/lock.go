//go:build unix

package rating

import (
	"os"
	"syscall"
)

// fileLock is a cross-process advisory lock guarding the ratings file. Multiple
// open-agent processes legitimately run at once (a bench matrix beside one-shot
// workers); without it, whole-state saves lose each other's updates — B's save
// silently drops the buckets A recorded between B's load and B's save.
type fileLock struct{ f *os.File }

// lockRatings blocks until it holds the exclusive lock for path.
func lockRatings(path string) (*fileLock, error) {
	f, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, err
	}
	return &fileLock{f: f}, nil
}

func (l *fileLock) unlock() {
	_ = syscall.Flock(int(l.f.Fd()), syscall.LOCK_UN)
	_ = l.f.Close()
}
