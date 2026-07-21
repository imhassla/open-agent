//go:build treesitter

package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"

	sitter "github.com/smacker/go-tree-sitter"
	"github.com/smacker/go-tree-sitter/golang"
	"github.com/smacker/go-tree-sitter/java"
	"github.com/smacker/go-tree-sitter/javascript"
	"github.com/smacker/go-tree-sitter/python"
	"github.com/smacker/go-tree-sitter/rust"
	tsts "github.com/smacker/go-tree-sitter/typescript/typescript"
)

type tsConfig struct {
	lang     *sitter.Language
	defTypes map[string]bool
}

func defSet(xs ...string) map[string]bool {
	m := make(map[string]bool, len(xs))
	for _, x := range xs {
		m[x] = true
	}
	return m
}

func tsConfigFor(ext string) (tsConfig, bool) {
	switch ext {
	case ".go":
		return tsConfig{golang.GetLanguage(), defSet("function_declaration", "method_declaration", "type_spec")}, true
	case ".py":
		return tsConfig{python.GetLanguage(), defSet("function_definition", "class_definition")}, true
	case ".js", ".jsx", ".mjs", ".cjs":
		return tsConfig{javascript.GetLanguage(), defSet("function_declaration", "class_declaration", "method_definition")}, true
	case ".ts", ".tsx":
		return tsConfig{tsts.GetLanguage(), defSet("function_declaration", "class_declaration", "method_definition", "interface_declaration", "type_alias_declaration", "enum_declaration")}, true
	case ".rs":
		return tsConfig{rust.GetLanguage(), defSet("function_item", "struct_item", "enum_item", "trait_item", "mod_item")}, true
	case ".java":
		return tsConfig{java.GetLanguage(), defSet("class_declaration", "interface_declaration", "method_declaration", "enum_declaration")}, true
	}
	return tsConfig{}, false
}

// extractSymbols (-tags treesitter): CGo tree-sitter parsing for richer,
// grammar-accurate multi-language symbol extraction.
func extractSymbols(path string) []string {
	cfg, ok := tsConfigFor(strings.ToLower(filepath.Ext(path)))
	if !ok {
		return nil
	}
	src, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	parser := sitter.NewParser()
	parser.SetLanguage(cfg.lang)
	tree, err := parser.ParseCtx(context.Background(), nil, src)
	if err != nil || tree == nil {
		return nil
	}
	defer tree.Close()

	var out []string
	walkDefs(tree.RootNode(), src, cfg.defTypes, &out)
	return out
}

func walkDefs(n *sitter.Node, src []byte, defTypes map[string]bool, out *[]string) {
	if n == nil {
		return
	}
	if defTypes[n.Type()] {
		if name := nodeName(n, src); name != "" {
			*out = append(*out, name)
		}
	}
	for i := 0; i < int(n.NamedChildCount()); i++ {
		walkDefs(n.NamedChild(i), src, defTypes, out)
	}
}

func nodeName(n *sitter.Node, src []byte) string {
	if f := n.ChildByFieldName("name"); f != nil {
		return f.Content(src)
	}
	for i := 0; i < int(n.NamedChildCount()); i++ {
		c := n.NamedChild(i)
		if strings.Contains(c.Type(), "identifier") {
			return c.Content(src)
		}
	}
	return ""
}
