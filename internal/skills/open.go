package skills

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// openBundleFile confines the actual open to the pinned canonical bundle.
// Resolving a path and then opening it by absolute name would permit a concurrent
// writer to replace a component with an escaping symlink between those steps.
func openBundleFile(base, file string) (*os.File, string, error) {
	if filepath.IsAbs(file) || !inside(base, filepath.Join(base, file)) {
		return nil, "", fmt.Errorf("skill resource must be bundle-relative and cannot escape bundle")
	}
	root, err := pinBundleRoot(base)
	if err != nil {
		return nil, "", err
	}
	defer root.Close()
	path, err := filepath.EvalSymlinks(filepath.Join(base, file))
	if err != nil {
		return nil, "", err
	}
	if !inside(base, path) {
		return nil, "", fmt.Errorf("skill resource symlink escapes bundle")
	}
	relative, err := filepath.Rel(base, path)
	if err != nil {
		return nil, "", err
	}
	// This check avoids opening known devices. The descriptor check below also
	// rejects nonregular replacements; Unix opens are nonblocking for raced FIFOs.
	info, err := root.Stat(relative)
	if err != nil {
		return nil, "", err
	}
	if !info.Mode().IsRegular() {
		return nil, "", fmt.Errorf("skill resource must be a regular file")
	}
	f, err := root.OpenFile(relative, skillReadFlags, 0)
	if err != nil {
		return nil, "", err
	}
	info, err = f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		f.Close()
		if err != nil {
			return nil, "", err
		}
		return nil, "", fmt.Errorf("skill resource must be a regular file")
	}
	return f, path, nil
}

// OpenRoot follows links in its own pathname. Pin each already-canonical
// component from the volume root and verify the opened directory's identity so
// replacement of the bundle or an ancestor cannot redirect the root itself.
func pinBundleRoot(base string) (*os.Root, error) {
	if !filepath.IsAbs(base) {
		return nil, fmt.Errorf("skill bundle must be absolute")
	}
	volumeRoot := filepath.VolumeName(base) + string(filepath.Separator)
	root, err := os.OpenRoot(volumeRoot)
	if err != nil {
		return nil, err
	}
	for _, part := range strings.Split(strings.TrimPrefix(filepath.Clean(base), volumeRoot), string(filepath.Separator)) {
		if part == "" {
			continue
		}
		info, err := root.Lstat(part)
		if err != nil || !info.IsDir() {
			root.Close()
			if err != nil {
				return nil, err
			}
			return nil, fmt.Errorf("canonical skill bundle directory was redirected: %s", base)
		}
		next, err := root.OpenRoot(part)
		root.Close()
		if err != nil {
			return nil, err
		}
		opened, err := next.Stat(".")
		if err != nil || !os.SameFile(info, opened) {
			next.Close()
			if err != nil {
				return nil, err
			}
			return nil, fmt.Errorf("skill bundle directory changed while opening: %s", base)
		}
		root = next
	}
	return root, nil
}
