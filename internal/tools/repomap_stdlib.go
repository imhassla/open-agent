//go:build !treesitter

package tools

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// extractSymbols (default, dependency-free): go/ast for Go, regex for py/js/ts.
func extractSymbols(path string) []string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".go":
		return goSymbols(path)
	case ".py":
		return regexSymbols(path, rePySym)
	case ".js", ".jsx", ".ts", ".tsx", ".mjs", ".cjs":
		return regexSymbols(path, reJSSym)
	default:
		return nil
	}
}

func goSymbols(path string) []string {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		return nil
	}
	var out []string
	for _, decl := range f.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			name := d.Name.Name
			if d.Recv != nil && len(d.Recv.List) > 0 {
				name = recvTypeName(d.Recv.List[0].Type) + "." + name
			}
			out = append(out, name)
		case *ast.GenDecl:
			for _, spec := range d.Specs {
				if ts, ok := spec.(*ast.TypeSpec); ok {
					out = append(out, ts.Name.Name)
				}
			}
		}
	}
	return out
}

func recvTypeName(e ast.Expr) string {
	switch t := e.(type) {
	case *ast.StarExpr:
		return recvTypeName(t.X)
	case *ast.IndexExpr: // generic receiver T[P]
		return recvTypeName(t.X)
	case *ast.IndexListExpr: // generic receiver T[P1, P2]
		return recvTypeName(t.X)
	case *ast.Ident:
		return t.Name
	}
	return "?"
}

var (
	rePySym = regexp.MustCompile(`(?m)^\s*(?:async\s+)?(?:def|class)\s+([A-Za-z_]\w*)`)
	reJSSym = regexp.MustCompile(`(?m)^\s*(?:export\s+)?(?:default\s+)?(?:async\s+)?(?:function|class|const|let|var)\s+([A-Za-z_$][\w$]*)`)
)

func regexSymbols(path string, re *regexp.Regexp) []string {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var out []string
	for _, m := range re.FindAllStringSubmatch(string(data), -1) {
		out = append(out, m[1])
	}
	return out
}
