package tools

import (
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/printer"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// GoSymbols lists declared symbols (funcs, methods, types, top-level var/const)
// in a .go file or directory, with kind, signature and line — an AST-accurate
// outline. In-process via go/ast (zero-dep), more precise than repo_map.
func GoSymbols(path string) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	add := func(file string) {
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, file, nil, parser.SkipObjectResolution)
		if err != nil {
			fmt.Fprintf(&b, "%s: parse error: %v\n", file, err)
			return
		}
		for _, decl := range f.Decls {
			switch d := decl.(type) {
			case *ast.FuncDecl:
				fmt.Fprintf(&b, "%s:%d: %s\n", file, fset.Position(d.Pos()).Line, funcSig(fset, d))
			case *ast.GenDecl:
				for _, spec := range d.Specs {
					switch s := spec.(type) {
					case *ast.TypeSpec:
						fmt.Fprintf(&b, "%s:%d: type %s\n", file, fset.Position(s.Pos()).Line, s.Name.Name)
					case *ast.ValueSpec:
						if d.Tok == token.CONST || d.Tok == token.VAR {
							for _, n := range s.Names {
								fmt.Fprintf(&b, "%s:%d: %s %s\n", file, fset.Position(n.Pos()).Line, d.Tok, n.Name)
							}
						}
					}
				}
			}
		}
	}

	if info.IsDir() {
		_ = filepath.WalkDir(path, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if d.IsDir() {
				return skipNoise(path, p, d)
			}
			if strings.HasSuffix(p, ".go") {
				add(p)
			}
			return nil
		})
	} else {
		add(path)
	}
	out := strings.TrimRight(b.String(), "\n")
	if out == "" {
		return "(no Go symbols found)", nil
	}
	return out, nil
}

// funcSig renders "func (recv) Name(params) results" without the body.
func funcSig(fset *token.FileSet, d *ast.FuncDecl) string {
	stub := *d
	stub.Body = nil
	stub.Doc = nil
	var sb strings.Builder
	_ = printer.Fprint(&sb, fset, &stub)
	return strings.TrimSpace(sb.String())
}

// GoFindRefs returns syntactic references to an identifier across .go files under
// dir (path:line: text). Syntactic (AST identifier match) — for type-resolved
// references use the lsp tool.
func GoFindRefs(dir, name string, max int) (string, error) {
	if dir == "" {
		dir = "."
	}
	if max <= 0 {
		max = 100
	}
	var b strings.Builder
	count := 0
	capped := false
	seen := map[string]bool{} // (path:line) — collapse an ident appearing twice on one line
	walkErr := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			return skipNoise(dir, p, d)
		}
		if !strings.HasSuffix(p, ".go") {
			return nil
		}
		src, err := os.ReadFile(p)
		if err != nil {
			return nil
		}
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, p, src, parser.SkipObjectResolution)
		if err != nil {
			return nil
		}
		lines := strings.Split(string(src), "\n")
		ast.Inspect(f, func(n ast.Node) bool {
			id, ok := n.(*ast.Ident)
			if !ok || id.Name != name {
				return true
			}
			pos := fset.Position(id.Pos())
			key := fmt.Sprintf("%s:%d", p, pos.Line)
			if seen[key] {
				return true
			}
			// ast.Inspect's false return only stops recursion into a leaf's
			// (nonexistent) children, not the walk — so we must gate emission on
			// the cap explicitly, else output overshoots max.
			if count >= max {
				capped = true
				return false
			}
			seen[key] = true
			text := ""
			if pos.Line-1 < len(lines) {
				text = strings.TrimSpace(lines[pos.Line-1])
			}
			fmt.Fprintf(&b, "%s:%d: %s\n", p, pos.Line, text)
			count++
			return true
		})
		if capped {
			return filepath.SkipAll
		}
		return nil
	})
	if walkErr != nil && walkErr != filepath.SkipAll {
		return "", walkErr
	}
	if count == 0 {
		return fmt.Sprintf("(no references to %q)", name), nil
	}
	res := strings.TrimRight(b.String(), "\n")
	if capped {
		res += fmt.Sprintf("\n…[capped at %d]", max)
	}
	return res, nil
}

// GoFmtPreview returns a .go file's current content and its gofmt'd form WITHOUT
// writing — for the diff-preview gate (P2). before==after when already clean; the
// "not valid Go" error matches GoFmt(write=true)'s, so preview and apply agree on
// what (if anything) will be written.
func GoFmtPreview(path string) (before, after string, err error) {
	src, rerr := os.ReadFile(path)
	if rerr != nil {
		return "", "", rerr
	}
	out, ferr := format.Source(src)
	if ferr != nil {
		return "", "", fmt.Errorf("not valid Go: %w", ferr)
	}
	return string(src), string(out), nil
}

// CheckGoSyntax parses a .go file in-process and returns a non-nil error only
// when the file no longer parses (a syntax error) — a single-file, no-compile
// check for the post-edit verify hook. It is deliberately parse-only, NOT gofmt:
// it must not depend on formatting state (a valid-but-unformatted file is fine),
// so it never nudges toward a whole-file reformat. AllErrors makes the message
// point at the first real breakage.
func CheckGoSyntax(path string) error {
	src, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if _, err := parser.ParseFile(token.NewFileSet(), path, src, parser.AllErrors); err != nil {
		return err
	}
	return nil
}

// GoFmt formats a .go file with go/format (in-process gofmt). With write it
// rewrites the file; otherwise it returns the formatted source.
func GoFmt(path string, write bool) (string, error) {
	src, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	out, err := format.Source(src)
	if err != nil {
		return "", fmt.Errorf("not valid Go: %w", err)
	}
	if string(out) == string(src) {
		return "already gofmt-clean", nil
	}
	if write {
		mode := os.FileMode(0o644)
		if fi, err := os.Stat(path); err == nil {
			mode = fi.Mode().Perm() // preserve the original permissions (e.g. +x)
		}
		if err := os.WriteFile(path, out, mode); err != nil {
			return "", err
		}
		return "formatted " + path, nil
	}
	return string(out), nil
}
