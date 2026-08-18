package tools

import (
	"os"
	"path/filepath"

	git "github.com/go-git/go-git/v5"
)

// depDirs are gitignored dependency stores that test runs READ. They are not in
// the git tree, so a materialized-HEAD checkout lacks them and a non-Go baseline
// (npm/pip) would spuriously fail to resolve imports — defeating FAIL_TO_PASS. We
// symlink them from the live worktree into the checkout (cheap, no copy).
//
// NOTE: only read-during-test stores are listed. Build-OUTPUT/cache dirs (target,
// .gradle, .tox, Pods) are deliberately EXCLUDED: a symlink to them would let a
// build in the checkout write THROUGH into the live tree and race across parallel
// verifies. Acceptance commands should run tests against already-installed deps,
// not invoke installers (which would also write through node_modules/.venv).
var depDirs = []string{"node_modules", "vendor", ".venv", "venv"}

// overlayDeps symlinks the dependency directories that exist in srcRoot into
// destDir (best-effort; skips any already present). It never copies, so it is
// cheap and the deps stay read-shared with the live tree.
func overlayDeps(srcRoot, destDir string) {
	for _, d := range depDirs {
		src := filepath.Join(srcRoot, d)
		fi, err := os.Stat(src)
		if err != nil || !fi.IsDir() {
			continue
		}
		dst := filepath.Join(destDir, d)
		if _, err := os.Lstat(dst); err == nil {
			continue // already materialized (tracked) — don't clobber
		}
		_ = os.Symlink(src, dst)
	}
}

// MaterializeHEADCheckout builds an isolated checkout of the committed HEAD tree
// in a fresh temp dir — WITHOUT the dependency-symlink overlay. Exported for
// best-of-N candidate trees, which hand the tree to an arbitrary LLM worker with
// a bash tool: a symlinked node_modules/vendor/.venv would let any candidate
// (including the losers) write straight through into the user's REAL dep dirs
// (npm install, rm -rf node_modules/ …) — invisible in the diff, violating the
// "only the winning diff lands on your tree" guarantee. Candidates therefore
// start without untracked dep dirs (tracked vendor/ still materializes from
// HEAD); a worker that needs them installs fresh inside its own tree. The
// returned dir is NOT a git repository — callers that need diffs must `git init`
// and commit a baseline themselves. cleanup must always run.
func MaterializeHEADCheckout(root string) (dir string, cleanup func(), ok bool) {
	return materialize(root, nil, false)
}

// materializeWithChanges builds an isolated checkout in a fresh temp dir: the
// committed HEAD tree, plus the gitignored dependency overlay, plus the live
// working-tree version of every changed file EXCEPT those for which restoreToHEAD
// returns true (which stay at their HEAD content). This is the substrate for
// differential execution (e.g. the test-tampering guard runs the change's source
// against the ORIGINAL tests by restoring *_test.go to HEAD). Returns the dir, a
// cleanup func, and ok=false if it couldn't be built (not a repo / no HEAD).
func materializeWithChanges(root string, restoreToHEAD func(path string) bool) (dir string, cleanup func(), ok bool) {
	return materialize(root, restoreToHEAD, true)
}

func materialize(root string, restoreToHEAD func(path string) bool, overlay bool) (dir string, cleanup func(), ok bool) {
	repo, err := openRepo(root)
	if err != nil {
		return "", func() {}, false
	}
	wt, st, err := worktreeStatus(repo)
	if err != nil {
		return "", func() {}, false
	}
	tree := headTree(repo)
	if tree == nil {
		return "", func() {}, false
	}
	wtRoot := wt.Filesystem.Root()
	d, err := os.MkdirTemp("", "open-agent-co-")
	if err != nil {
		return "", func() {}, false
	}
	cleanup = func() { _ = os.RemoveAll(d) }
	if err := materializeTree(tree, d); err != nil {
		cleanup()
		return "", func() {}, false
	}
	if overlay {
		overlayDeps(wtRoot, d)
	}

	for path, s := range st {
		if s.Worktree == git.Unmodified && s.Staging == git.Unmodified {
			continue
		}
		if restoreToHEAD != nil && restoreToHEAD(path) {
			continue // keep the HEAD version already materialized
		}
		dst := filepath.Join(d, filepath.FromSlash(path))
		src := filepath.Join(wtRoot, filepath.FromSlash(path))
		fi, err := os.Lstat(src)
		if err != nil {
			if os.IsNotExist(err) {
				_ = os.Remove(dst) // deleted in the worktree → reflect the deletion
			}
			continue // other stat errors: leave the HEAD version rather than guess
		}
		_ = os.MkdirAll(filepath.Dir(dst), 0o755)
		if fi.Mode()&os.ModeSymlink != 0 {
			// Reproduce a symlink as a symlink (don't dereference it into a file).
			if target, rerr := os.Readlink(src); rerr == nil {
				_ = os.Remove(dst)
				_ = os.Symlink(target, dst)
			}
			continue
		}
		live, rerr := os.ReadFile(src)
		if rerr != nil {
			continue
		}
		_ = os.WriteFile(dst, live, fi.Mode().Perm())
	}
	return d, cleanup, true
}
