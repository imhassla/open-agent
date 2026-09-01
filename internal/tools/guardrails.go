package tools

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Guardrails: deterministic pre-execution rules for the worker-facing mutating
// tools. Non-interactive agent runs have no human to approve a call, so these
// are RULES, not prompts — a denial returns an error (surfaced to the model as
// "ERROR: …", routing into ToolErrors and keeping the apply-guard honest) with
// a constructive next step so the model adapts instead of blind-retrying;
// noteRepeat then throttles identical retries.
//
// Two layers:
//   - Write confinement: every mutating file tool refuses to write outside the
//     working directory (absolute paths out of tree, ../ escapes). Until now
//     this was only a soft preamble hint — a misrouted model could write
//     anywhere the process could.
//   - Bash denylist: a small, tight list of catastrophic command shapes
//     (recursive rm of / or ~, force-push, fork bomb, mkfs/dd-to-device,
//     curl-pipe-to-shell). Deliberately narrow: false positives on normal dev
//     commands cost worker steps, so each rule anchors on the destructive form.
//
// Kill-switch: OPEN_AGENT_NO_GUARDRAILS=1 disables both layers (for the rare
// legitimate out-of-tree task; the hardened Docker sandbox is the right tool
// for genuinely untrusted work).

// guardrailsOff is read per call so tests can flip it with t.Setenv.
func guardrailsOff() bool { return os.Getenv("OPEN_AGENT_NO_GUARDRAILS") == "1" }

// ConfineWrite rejects a mutating tool's path when it resolves outside the
// current working directory. Both sides are symlink-resolved over their
// existing prefix (macOS: Getwd returns /private/var/… while Abs of a
// /var/…-spelled path doesn't — without resolution every absolute in-tree
// path under a symlinked root would false-block). Symlinks are chased: an
// in-tree symlink pointing outside is blocked whether it resolves (prefix
// walk) or dangles (explicit one-hop target check below).
func ConfineWrite(path string) error {
	if guardrailsOff() {
		return nil
	}
	// Go never expands shell tilde: "~/x" is a RELATIVE path that would silently
	// create a literal "~" directory in the repo. Models emit these; reject with
	// steering instead of leaving junk (which rm -rf ~ then can't clean — denied).
	if path == "~" || strings.HasPrefix(path, "~/") {
		return fmt.Errorf("blocked by guardrail 'no-writes-outside-project': shell ~ is not expanded here — " +
			"use a path relative to the working directory")
	}
	cwd, err := os.Getwd()
	if err != nil {
		return nil // cannot establish a root — do not turn every write into an error
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil
	}
	outside := func() error {
		return fmt.Errorf("blocked by guardrail 'no-writes-outside-project': %s resolves outside the working directory %s. "+
			"Write only to paths inside the project (relative paths, no ../ escapes); if the task genuinely "+
			"needs an external file, explain that in your final answer instead of writing it", path, cwd)
	}
	rel, err := filepath.Rel(resolveExistingPrefix(cwd), resolveExistingPrefix(abs))
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return outside()
	}
	// A DANGLING in-tree symlink evades resolveExistingPrefix (EvalSymlinks errors
	// on it, the walk resolves only its parent) — but os.WriteFile would follow it
	// and create the file at its target. Confine the link target too (one hop:
	// dangling means the chain ends there).
	if fi, lerr := os.Lstat(abs); lerr == nil && fi.Mode()&os.ModeSymlink != 0 {
		if tgt, rerr := os.Readlink(abs); rerr == nil {
			if !filepath.IsAbs(tgt) {
				tgt = filepath.Join(filepath.Dir(abs), tgt)
			}
			trel, terr := filepath.Rel(resolveExistingPrefix(cwd), resolveExistingPrefix(tgt))
			if terr != nil || trel == ".." || strings.HasPrefix(trel, ".."+string(filepath.Separator)) {
				return outside()
			}
		}
	}
	return checkWriteGlobs(rel) // user write-path deny rules, inside-tree
}

// resolveExistingPrefix symlink-resolves the longest EXISTING ancestor of p and
// re-joins the not-yet-existing remainder (a write target's file/dirs may not
// exist yet). Falls back to p unchanged when nothing resolves.
func resolveExistingPrefix(p string) string {
	rest := ""
	cur := p
	for {
		if r, err := filepath.EvalSymlinks(cur); err == nil {
			return filepath.Join(r, rest)
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return p
		}
		rest = filepath.Join(filepath.Base(cur), rest)
		cur = parent
	}
}

// bashDenyRule pairs a compiled pattern with its name and steering advice.
type bashDenyRule struct {
	name string
	re   *regexp.Regexp
	fix  string
}

var bashDenyRules = []bashDenyRule{
	{
		// Target group covers the bare token AND its accident variants: trailing
		// slash (~/, $HOME/), glob (/*), doubled slash (//), ${HOME}, quoting —
		// all byte-for-byte as catastrophic as the bare form, and exactly what a
		// confused model emits. `~/foo` and `/tmp/x` still pass: the terminator
		// fails at the first letter of the real path.
		name: "no-recursive-rm-of-root-or-home",
		re:   regexp.MustCompile(`(?i)\brm\s+(-[a-z]*\s+)*-[a-z]*[rf][a-z]*\s+(-[a-z]*\s+)*['"]?(/|~|\$HOME|\$\{HOME\})/?\**['"]?(\s|$|;|&|\|)`),
		fix:  "delete specific project paths instead",
	},
	{
		// [^;&|\n]* — the \n matters: without it, `git push origin main` on line 1
		// followed by `rm -f tmp.txt` on line 2 would be attributed to git push.
		// --force-with-lease (the "safe" force) is still blocked: an unattended
		// worker has no business rewriting remote history in any flavor.
		name: "no-force-push",
		re:   regexp.MustCompile(`\bgit\s+push\b[^;&|\n]*(\s--force\b|\s-f\b|\s--force-with-lease\b)`),
		fix:  "push normally, or leave history rewrites to the user",
	},
	{
		name: "no-fork-bomb",
		re:   regexp.MustCompile(`:\s*\(\s*\)\s*\{\s*:\s*\|\s*:\s*&\s*\}\s*;`),
		fix:  "this is a fork bomb, not a useful command",
	},
	{
		// mkfs anchored to command position (start / after separator / after sudo)
		// so `grep mkfs docs/` and `man mkfs` pass. dd enumerates raw devices so
		// the common `of=/dev/null` benchmark idiom passes.
		name: "no-mkfs-or-raw-device-write",
		re:   regexp.MustCompile(`(?i)((^|[;&|]\s*|\bsudo\s+)mkfs(\.\w+)?\b|\bdd\b[^;&|\n]*\bof=/dev/(sd|hd|nvme|disk|mmcblk|vd|xvd|loop)|>\s*/dev/(sd|hd|nvme|disk|mmcblk|vd|xvd|loop))`),
		fix:  "raw device operations are out of scope for a coding task",
	},
	{
		name: "no-curl-pipe-shell",
		re:   regexp.MustCompile(`(?i)\b(curl|wget)\b[^;&|\n]*\|\s*(sudo\s+)?(ba|z|da)?sh\b`),
		fix:  "download to a file, inspect it, then run it explicitly if the task requires",
	},
	{
		name: "no-recursive-chmod-of-root",
		re:   regexp.MustCompile(`(?i)\bchmod\s+(-[a-z]*R[a-z]*\s+)\S+\s+("?/"?)(\s|$|;|&|\|)`),
		fix:  "chmod specific project paths instead",
	},
}

// CheckBashCommand rejects catastrophic command shapes before execution.
// Built-in rules run first (authoritative), then user-loaded extras.
func CheckBashCommand(cmd string) error {
	if guardrailsOff() {
		return nil
	}
	for _, r := range bashDenyRules {
		if r.re.MatchString(cmd) {
			return fmt.Errorf("blocked by guardrail '%s': this command shape is destructive and never needed for the task — %s", r.name, r.fix)
		}
	}
	for _, r := range extraBashRules {
		if r.re.MatchString(cmd) {
			return fmt.Errorf("blocked by guardrail '%s' (user rule): this command matches a deny pattern from your guardrails config", r.name)
		}
	}
	return nil
}

// ---- user-extendable rules -------------------------------------------------
//
// Users ADD deny rules via plain-text files (loaded once at startup by main):
//   ~/.config/open-agent/guardrails   (global)
//   ./.open-agent/guardrails          (project-local; gitignored by convention,
//                                      so best-of-N candidate trees see only
//                                      the global file — each candidate is a
//                                      fresh process that loads at its own cwd)
//
// Format, one rule per line, '#' comments:
//   name: regex               — bash deny rule (RE2)
//   write name: glob          — deny WRITES to matching paths even inside the
//                               tree; glob is filepath.Match (single *, no **)
//                               tested against both the cwd-relative path and
//                               its basename, so `write no-env: *.env` denies
//                               any .env at any depth.
//
// Semantics: user rules add to the built-ins and can never remove them — the
// safety floor is non-overridable; the only escape is the global kill-switch.
// A malformed line or invalid regex is warned to stderr and skipped: a config
// typo must never break the CLI.

type extraWriteGlob struct {
	name string
	glob string
}

var (
	extraBashRules  []bashDenyRule
	extraWriteGlobs []extraWriteGlob
)

// LoadGuardrailRules loads user deny rules from the given files, REPLACING any
// previously loaded extras (idempotent — safe to call again in tests). Missing
// files are silently fine; bad lines are skipped with a returned error each
// (the caller warns, never aborts).
func LoadGuardrailRules(paths []string) []error {
	var errs []error
	var bash []bashDenyRule
	var globs []extraWriteGlob
	for _, p := range paths {
		data, err := os.ReadFile(p)
		if err != nil {
			continue // absent config is the normal case
		}
		for i, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			if rest, ok := strings.CutPrefix(line, "write "); ok {
				name, glob, found := strings.Cut(rest, ":")
				name, glob = strings.TrimSpace(name), strings.TrimSpace(glob)
				if !found || name == "" || glob == "" {
					errs = append(errs, fmt.Errorf("%s:%d: malformed write rule (want \"write name: glob\")", p, i+1))
					continue
				}
				if _, merr := filepath.Match(glob, "probe"); merr != nil {
					errs = append(errs, fmt.Errorf("%s:%d: invalid glob %q: %v", p, i+1, glob, merr))
					continue
				}
				globs = append(globs, extraWriteGlob{name: name, glob: glob})
				continue
			}
			name, pattern, found := strings.Cut(line, ":")
			name, pattern = strings.TrimSpace(name), strings.TrimSpace(pattern)
			if !found || name == "" || pattern == "" {
				errs = append(errs, fmt.Errorf("%s:%d: malformed rule (want \"name: regex\")", p, i+1))
				continue
			}
			if name == "write" {
				// "write: glob" (colon typo for "write name: glob") would parse as
				// a bash rule named "write" and silently guard the wrong layer —
				// warn BEFORE regex compilation so the typo diagnosis wins even
				// when the glob is also an invalid regex.
				errs = append(errs, fmt.Errorf("%s:%d: rule named \"write\" — did you mean a write rule? (format: \"write name: glob\")", p, i+1))
				continue
			}
			re, rerr := regexp.Compile(pattern)
			if rerr != nil {
				errs = append(errs, fmt.Errorf("%s:%d: invalid regex for %q: %v", p, i+1, name, rerr))
				continue
			}
			bash = append(bash, bashDenyRule{name: name, re: re})
		}
	}
	extraBashRules, extraWriteGlobs = bash, globs
	return errs
}

// checkWriteGlobs applies user write-path deny globs to a cwd-relative path.
// Case-folded on BOTH sides: on case-insensitive filesystems (APFS) writing
// ".ENV" clobbers the ".env" a rule protects, and Match is case-sensitive —
// folding closes that bypass; a deliberately case-distinct deny glob is
// implausible, and matching more broadly is the safe direction for a denylist.
func checkWriteGlobs(rel string) error {
	lrel := strings.ToLower(rel)
	for _, g := range extraWriteGlobs {
		lglob := strings.ToLower(g.glob)
		m1, _ := filepath.Match(lglob, lrel)
		m2, _ := filepath.Match(lglob, filepath.Base(lrel))
		if m1 || m2 {
			return fmt.Errorf("blocked by guardrail '%s' (user rule): writing %s is denied by your guardrails config", g.name, rel)
		}
	}
	return nil
}
