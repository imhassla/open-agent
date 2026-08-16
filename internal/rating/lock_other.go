//go:build !unix

package rating

// Non-unix fallback: no cross-process lock (single-process safety still comes
// from s.mu). The supported platforms are darwin/linux.
type fileLock struct{}

func lockRatings(string) (*fileLock, error) { return &fileLock{}, nil }

func (l *fileLock) unlock() {}
