package tools

import (
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"os"
	"strings"
)

// ReplaceGoFunc swaps an entire function or method declaration in a .go file by
// NAME, using AST positions instead of string matching — the reliable way for a
// model to rewrite a large function without reproducing its exact current bytes
// (where cheap models systematically fail). name is "FuncName" for a function or
// "Recv.Method" for a method; newSrc must be a complete declaration (doc comment
// included if wanted — the old one is replaced along with the function).
func ReplaceGoFunc(path, name, newSrc string) (string, error) {
	src, err := os.ReadFile(path)
	if err != nil {
		// Same policy as edit_file: a mutating tool never redirects to a
		// near-match, but the error names the candidates for an instant retry.
		if os.IsNotExist(err) {
			if hint := notFoundHint(path, resolveNear(path)); hint != "" {
				return "", fmt.Errorf("%s (pass the correct path explicitly)", hint)
			}
		}
		return "", err
	}
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, src, parser.ParseComments|parser.SkipObjectResolution)
	if err != nil {
		return "", fmt.Errorf("cannot parse %s: %v", path, err)
	}

	// Validate the replacement FIRST: it must itself parse as exactly one func
	// decl whose name matches — a broken replacement must never hit the disk.
	wantRecv, wantName := splitFuncName(name)
	newDecl, err := parseSingleFunc(newSrc)
	if err != nil {
		return "", fmt.Errorf("new_source: %v", err)
	}
	if got := newDecl.Name.Name; got != wantName {
		return "", fmt.Errorf("new_source declares %q but name is %q", got, wantName)
	}
	// Verify the receiver matches as well.
	var gotRecv string
	if newDecl.Recv != nil && len(newDecl.Recv.List) > 0 {
		gotRecv = recvTypeName(newDecl.Recv.List[0].Type)
	}
	if gotRecv != wantRecv {
		return "", fmt.Errorf("new_source declares receiver %q but want %q", gotRecv, wantRecv)
	}

	decl, available := findFunc(f, wantRecv, wantName)
	if decl == nil {
		return "", fmt.Errorf("no function %q in %s; available: %s", name, path, strings.Join(available, ", "))
	}

	// Replace from the doc comment (if any) through the closing brace, so stale
	// documentation never survives a body swap.
	start := decl.Pos()
	if decl.Doc != nil {
		start = decl.Doc.Pos()
	}
	from, to := fset.Position(start).Offset, fset.Position(decl.End()).Offset
	var b strings.Builder
	b.Write(src[:from])
	b.WriteString(strings.TrimSpace(newSrc))
	b.Write(src[to:])

	// gofmt the result; a format error means the splice produced invalid Go —
	// refuse to write it.
	formatted, err := format.Source([]byte(b.String()))
	if err != nil {
		return "", fmt.Errorf("replacement produces invalid Go (nothing written): %v", err)
	}
	if err := os.WriteFile(path, formatted, 0o644); err != nil {
		return "", err
	}
	return fmt.Sprintf("replaced %s in %s (%d → %d bytes)", name, path, to-from, len(strings.TrimSpace(newSrc))), nil
}

// splitFuncName parses "Recv.Method" into (recv, method); a bare name yields ("", name).
func splitFuncName(name string) (recv, fn string) {
	if i := strings.LastIndex(name, "."); i >= 0 {
		return name[:i], name[i+1:]
	}
	return "", name
}

// parseSingleFunc parses newSrc as exactly one top-level function declaration.
func parseSingleFunc(newSrc string) (*ast.FuncDecl, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "new.go", "package _p\n"+newSrc, parser.ParseComments|parser.SkipObjectResolution)
	if err != nil {
		return nil, fmt.Errorf("does not parse as Go: %v", err)
	}
	var fn *ast.FuncDecl
	for _, d := range f.Decls {
		fd, ok := d.(*ast.FuncDecl)
		if !ok {
			return nil, fmt.Errorf("must contain only one function declaration")
		}
		if fn != nil {
			return nil, fmt.Errorf("must contain exactly one function declaration, found several")
		}
		fn = fd
	}
	if fn == nil {
		return nil, fmt.Errorf("no function declaration found")
	}
	return fn, nil
}

// findFunc locates a func/method by (receiver, name); when absent it returns the
// available names so the model can self-correct in one step.
func findFunc(f *ast.File, wantRecv, wantName string) (*ast.FuncDecl, []string) {
	var available []string
	for _, d := range f.Decls {
		fd, ok := d.(*ast.FuncDecl)
		if !ok {
			continue
		}
		recv := ""
		if fd.Recv != nil && len(fd.Recv.List) > 0 {
			recv = recvTypeName(fd.Recv.List[0].Type)
		}
		label := fd.Name.Name
		if recv != "" {
			label = recv + "." + fd.Name.Name
		}
		available = append(available, label)
		if fd.Name.Name == wantName && recv == wantRecv {
			return fd, nil
		}
	}
	return nil, available
}
