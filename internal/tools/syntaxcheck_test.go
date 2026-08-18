package tools

import (
	"os"
	"strings"
	"testing"
)

func TestCheckPyJSSyntax(t *testing.T) {
	t.Chdir(t.TempDir())

	if pyBin() == "" {
		t.Skip("python3 not installed")
	}
	if err := os.WriteFile("ok.py", []byte("x = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := CheckPyJSSyntax("ok.py"); err != nil {
		t.Errorf("valid python flagged: %v", err)
	}
	if err := os.WriteFile("bad.py", []byte("def f(:\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := CheckPyJSSyntax("bad.py")
	if err == nil {
		t.Fatal("invalid python not flagged")
	}
	// Controlled ast.parse output: file:line: msg — compact and informative.
	if !strings.Contains(err.Error(), "bad.py:1") || !strings.Contains(strings.ToLower(err.Error()), "syntax") {
		t.Errorf("python diagnosis lost its payload: %v", err)
	}
	// ast.parse must leave NO __pycache__ in the tree (the reason we don't use py_compile).
	if _, serr := os.Stat("__pycache__"); serr == nil {
		t.Error("python check littered __pycache__ into the tree")
	}
}

func TestCheckPyJSSyntax_Node(t *testing.T) {
	t.Chdir(t.TempDir())
	if nodeBin() == "" {
		t.Skip("node not installed")
	}
	if err := os.WriteFile("ok.js", []byte("const x = 1;\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := CheckPyJSSyntax("ok.js"); err != nil {
		t.Errorf("valid js flagged: %v", err)
	}
	if err := os.WriteFile("bad.js", []byte("function f( {\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := CheckPyJSSyntax("bad.js")
	if err == nil {
		t.Fatal("invalid js not flagged")
	}
	if !strings.Contains(err.Error(), "Error") {
		t.Errorf("js diagnosis lost the Error line: %v", err)
	}

	// A syntax error on a LONG source line: the extracted diagnosis must still
	// carry the Error line (naive flatten-and-truncate lost it entirely).
	long := "const y = {" + strings.Repeat("aaaa: 1, ", 40) + "\n"
	if err := os.WriteFile("long.js", []byte(long), 0o644); err != nil {
		t.Fatal(err)
	}
	err = CheckPyJSSyntax("long.js")
	if err == nil {
		t.Fatal("long-line syntax error not flagged")
	}
	head := err.Error()
	if len(head) > 200 {
		head = head[:200]
	}
	if !strings.Contains(head, "Error") {
		t.Errorf("Error line does not survive the 200-char note budget: %q", err.Error())
	}
}

func TestCheckPyJSSyntax_EdgeCases(t *testing.T) {
	t.Chdir(t.TempDir())
	// Leading-dash basename must not be parsed as an interpreter FLAG.
	if nodeBin() != "" {
		if err := os.WriteFile("-e.js", []byte("const x = 1;\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := CheckPyJSSyntax("-e.js"); err != nil {
			t.Errorf("valid dash-named js misdiagnosed: %v", err)
		}
	}
	if pyBin() != "" {
		if err := os.WriteFile("-x.py", []byte("x = 1\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := CheckPyJSSyntax("-x.py"); err != nil {
			t.Errorf("valid dash-named py misdiagnosed: %v", err)
		}
	}
	// Unknown extension and missing file are silently fine (best-effort contract).
	if err := CheckPyJSSyntax("x.rb"); err != nil {
		t.Errorf("unknown ext must be silent: %v", err)
	}
	if err := CheckPyJSSyntax("nope.py"); err != nil {
		t.Errorf("missing file must be silent: %v", err)
	}
}
