package agent

import (
	"context"
	"io"
	"strings"

	"github.com/imhassla/open-agent/internal/tools"
)

// AskTools returns the conversational tool set: web_search (DuckDuckGo by
// default; upgrade to grounded Sonar with RegisterWebSearch) + web_fetch. It is
// deliberately NOT the coding tool set — an ask turn searches and reads the web
// to answer, it does not run bash or touch files. A tool-free chat is still the
// common case: the model calls these only when the question needs current or
// external facts, so trivial turns stay a single fast call.
func AskTools() *Registry {
	r := NewRegistry()
	r.Register(Tool{
		Def: schema("web_search",
			"Search the web for CURRENT information. Returns ranked results (or a grounded cited answer). "+
				"Use for facts, versions, docs, news, prices — anything past your training data or that may have changed.",
			obj(props{"query": str("Search query"), "max_results": integer("Max results (default 5)")}, "query")),
		Handler: func(ctx context.Context, a map[string]any) (string, error) {
			return tools.SearchDDG(ctx, argStr(a, "query"), argInt(a, "max_results"))
		},
	})
	r.Register(Tool{
		Def: schema("web_fetch",
			"Fetch a URL and return its cleaned text content (markup stripped). Use to read a specific page a search surfaced.",
			obj(props{"url": str("The URL to fetch")}, "url")),
		Handler: func(ctx context.Context, a map[string]any) (string, error) {
			return tools.FetchPage(ctx, argStr(a, "url"), 0)
		},
	})
	return r
}

// CoreTools returns the default tool registry: web search/fetch, bash, file
// read/write, and final_answer.
func CoreTools() *Registry {
	r := NewRegistry()

	r.Register(Tool{
		Def: schema("web_search",
			"Search the web (DuckDuckGo). Returns ranked results with titles, URLs and snippets.",
			obj(props{"query": str("Search query"), "max_results": integer("Max results (default 5)")}, "query")),
		Handler: func(ctx context.Context, a map[string]any) (string, error) {
			return tools.SearchDDG(ctx, argStr(a, "query"), argInt(a, "max_results"))
		},
	})

	r.Register(Tool{
		Def: schema("web_fetch",
			"Fetch a URL and return its cleaned text content (markup stripped).",
			obj(props{"url": str("The URL to fetch")}, "url")),
		Handler: func(ctx context.Context, a map[string]any) (string, error) {
			return tools.FetchPage(ctx, argStr(a, "url"), 0)
		},
	})

	r.Register(Tool{
		Def: schema("bash",
			"Run a bash command and return combined stdout/stderr. Use for builds, tests, git, and file inspection. "+
				"Output returns only when the command FINISHES (not streamed), and the whole process group is killed at "+
				"timeout_sec. So make long work SELF-BOUNDED rather than relying on the timeout: prefer per-item timeouts "+
				"and a hard overall budget (e.g. `nmap -sn --host-timeout 2s`, `fping -a -g`, `timeout 20 <cmd>`, "+
				"`curl --max-time 5`), and avoid unbounded scans/backgrounded jobs whose children outlive the shell.",
			obj(props{"command": str("The bash command to run"), "timeout_sec": integer("Timeout in seconds (default 30)")}, "command")),
		Handler: func(ctx context.Context, a map[string]any) (string, error) {
			return tools.BashExec(ctx, argStr(a, "command"), argInt(a, "timeout_sec"))
		},
		Stream: func(ctx context.Context, a map[string]any, w io.Writer) (string, error) {
			return tools.BashExecStream(ctx, argStr(a, "command"), argInt(a, "timeout_sec"), w)
		},
	})

	r.Register(Tool{
		Def: schema("read_file",
			"Read a file from disk. Without start/end returns the file (large files are truncated with "+
				"a marker naming the line to continue from); with start/end returns that 1-based inclusive "+
				"line range with line numbers. Page through large files with start/end instead of re-reading.",
			obj(props{
				"path":  str("Path to the file"),
				"start": integer("First line (1-based, optional)"),
				"end":   integer("Last line (inclusive, optional)"),
			}, "path")),
		Handler: func(ctx context.Context, a map[string]any) (string, error) {
			start, end := argInt(a, "start"), argInt(a, "end")
			if start > 0 || end > 0 {
				return tools.ReadFileLines(argPath(a, "path", "file", "filename", "filepath"), start, end)
			}
			return tools.ReadFile(argPath(a, "path", "file", "filename", "filepath"), 0)
		},
	})

	r.Register(Tool{
		Applies: true,
		Def: schema("write_file",
			"Write content to a file (creates parent dirs, overwrites if present). For an existing "+
				"file prefer edit_file unless you intend a full rewrite.",
			obj(props{"path": str("Path to write"), "content": str("Full file content")}, "path", "content")),
		Handler: func(ctx context.Context, a map[string]any) (string, error) {
			return tools.WriteFile(argPath(a, "path", "file", "filename", "filepath"), argStr(a, "content"))
		},
	})

	r.Register(Tool{
		Applies: true,
		Def: schema("edit_file",
			"Make a surgical edit: replace old_string with new_string in a file. old_string must be "+
				"unique in the file (include surrounding context) unless replace_all is set.",
			obj(props{
				"path":        str("Path to the file"),
				"old_string":  str("Exact text to replace (must be unique unless replace_all)"),
				"new_string":  str("Replacement text"),
				"replace_all": boolean("Replace every occurrence (default false)"),
			}, "path", "old_string", "new_string")),
		Handler: func(ctx context.Context, a map[string]any) (string, error) {
			return tools.EditFile(argPath(a, "path", "file", "filename", "filepath"), argStr(a, "old_string"), argStr(a, "new_string"), argBool(a, "replace_all"))
		},
	})

	r.Register(Tool{
		Def: schema("glob",
			"List files matching a glob pattern. Supports a '**/' segment for recursive match (e.g. '**/*.go').",
			obj(props{"pattern": str("Glob pattern")}, "pattern")),
		Handler: func(ctx context.Context, a map[string]any) (string, error) {
			matches, err := tools.Glob(argStr(a, "pattern"))
			if err != nil {
				return "", err
			}
			if len(matches) == 0 {
				return "(no matches)", nil
			}
			return strings.Join(matches, "\n"), nil
		},
	})

	r.Register(Tool{
		Def: schema("grep",
			"Search file contents by regex under a directory. Returns path:line:text matches.",
			obj(props{
				"pattern":     str("Regular expression"),
				"path":        str("Root directory to search (default current dir)"),
				"max_results": integer("Max matches (default 100)"),
			}, "pattern")),
		Handler: func(ctx context.Context, a map[string]any) (string, error) {
			return tools.Grep(ctx, argStr(a, "pattern"), argStr(a, "path"), argInt(a, "max_results"))
		},
	})

	r.Register(Tool{
		Def: schema("repo_map",
			"Get a budget-bounded structural map of the codebase: the most relevant files and their top "+
				"symbols (functions/types/classes), ranked by reference frequency. Pass 'focus' terms "+
				"(identifiers from the task) to bias it. Use this to orient before reading files.",
			obj(props{
				"focus":     arr("Identifiers/terms to prioritize (optional)"),
				"path":      str("Root directory (default current dir)"),
				"max_bytes": integer("Budget for the map (default 4000)"),
			})),
		Handler: func(ctx context.Context, a map[string]any) (string, error) {
			return tools.RepoMap(argStr(a, "path"), argStrSlice(a, "focus"), argInt(a, "max_bytes"))
		},
	})

	r.Register(Tool{
		Def: schema("go_symbols",
			"List Go symbols (funcs/methods/types/vars/consts) with signatures and line numbers in a "+
				".go file or directory — an AST-accurate outline. Use to understand a Go file before editing.",
			obj(props{"path": str("A .go file or directory")}, "path")),
		Handler: func(ctx context.Context, a map[string]any) (string, error) {
			return tools.GoSymbols(argPath(a, "path", "file", "filename", "filepath"))
		},
	})

	r.Register(Tool{
		Def: schema("go_find_refs",
			"Find references to a Go identifier across .go files under a directory (path:line:text).",
			obj(props{
				"name":        str("Identifier to find"),
				"path":        str("Root directory (default current dir)"),
				"max_results": integer("Max matches (default 100)"),
			}, "name")),
		Handler: func(ctx context.Context, a map[string]any) (string, error) {
			return tools.GoFindRefs(argStr(a, "path"), argStr(a, "name"), argInt(a, "max_results"))
		},
	})

	r.Register(Tool{
		Applies: true,
		Def: schema("go_replace_func",
			"Replace an ENTIRE Go function/method by name (AST-based, no exact-text matching). "+
				"PREFER this over edit_file when rewriting a whole function — it cannot miss like "+
				"old_string can. name is 'FuncName' or 'Recv.Method'; new_source must be one complete "+
				"function declaration (include a doc comment — the old one is replaced too). "+
				"The result is gofmt-ed; invalid Go is rejected without writing.",
			obj(props{
				"path":       str("The .go file"),
				"name":       str("Function name, or Recv.Method for a method"),
				"new_source": str("The complete replacement function declaration"),
			}, "path", "name", "new_source")),
		Handler: func(ctx context.Context, a map[string]any) (string, error) {
			return tools.ReplaceGoFunc(argPath(a, "path", "file", "filename", "filepath"), argStr(a, "name"), argStr(a, "new_source"))
		},
	})

	r.Register(Tool{
		// go_fmt mutates the tree only with write=true; AppliesWhen reflects that so the
		// apply-guard counts a real reformat (and doesn't false-count a read-only format).
		Applies:     true,
		AppliesWhen: func(a map[string]any) bool { return argBool(a, "write") },
		Def: schema("go_fmt",
			"Format a Go file with gofmt (in-process). With write=true it rewrites the file; otherwise "+
				"returns the formatted source. Run after editing Go code.",
			obj(props{
				"path":  str("The .go file"),
				"write": boolean("Rewrite the file in place (default false)"),
			}, "path")),
		Handler: func(ctx context.Context, a map[string]any) (string, error) {
			return tools.GoFmt(argPath(a, "path", "file", "filename", "filepath"), argBool(a, "write"))
		},
	})

	// Only advertise the lsp_* tools when a language server is actually installed —
	// otherwise the agent wastes steps calling a tool that can only error.
	if tools.AnyLSPServerAvailable() {
		r.Register(Tool{
			Def: schema("lsp_diagnostics",
				"Compiler/linter diagnostics for a file via its language server (gopls/pyright/"+
					"typescript-language-server/rust-analyzer). Compiler-grade truth, not heuristics.",
				obj(props{"file": str("Path to the source file")}, "file")),
			Handler: func(ctx context.Context, a map[string]any) (string, error) {
				return tools.LSPDiagnostics(ctx, argPath(a, "file", "path", "filename", "filepath"))
			},
		})

		r.Register(Tool{
			Def: schema("lsp_hover",
				"Type/signature/docs at a position (1-based line & column) via the language server.",
				obj(props{"file": str("Source file"), "line": integer("1-based line"), "column": integer("1-based column")}, "file", "line", "column")),
			Handler: func(ctx context.Context, a map[string]any) (string, error) {
				return tools.LSPHover(ctx, argPath(a, "file", "path", "filename", "filepath"), argInt(a, "line"), argInt(a, "column"))
			},
		})

		r.Register(Tool{
			Def: schema("lsp_definition",
				"Jump to the definition of the symbol at a position (1-based line & column).",
				obj(props{"file": str("Source file"), "line": integer("1-based line"), "column": integer("1-based column")}, "file", "line", "column")),
			Handler: func(ctx context.Context, a map[string]any) (string, error) {
				return tools.LSPDefinition(ctx, argPath(a, "file", "path", "filename", "filepath"), argInt(a, "line"), argInt(a, "column"))
			},
		})

		r.Register(Tool{
			Def: schema("lsp_references",
				"Find all references to the symbol at a position (1-based line & column) — type-resolved.",
				obj(props{"file": str("Source file"), "line": integer("1-based line"), "column": integer("1-based column")}, "file", "line", "column")),
			Handler: func(ctx context.Context, a map[string]any) (string, error) {
				return tools.LSPReferences(ctx, argPath(a, "file", "path", "filename", "filepath"), argInt(a, "line"), argInt(a, "column"))
			},
		})
	}

	r.Register(Tool{
		Def: schema("go_eval",
			"Run a Go snippet in-process (yaegi interpreter — no compilation). REPL-style: include any "+
				"imports the snippet needs, e.g. `import \"fmt\"; fmt.Println(2+3)`. Returns captured output. "+
				"Use for quick Go calculations/checks; for untrusted code prefer the sandbox.",
			obj(props{
				"code":        str("Go source to evaluate (with its imports)"),
				"timeout_sec": integer("Timeout (default 10)"),
			}, "code")),
		Handler: func(ctx context.Context, a map[string]any) (string, error) {
			return tools.GoEval(argStr(a, "code"), argInt(a, "timeout_sec"))
		},
	})

	r.Register(Tool{
		Def: schema("run_tests",
			"Detect the project (Go/Python/Node) and run its test suite, returning a structured "+
				"per-test pass/fail summary (Go via `go test -json`). Use to verify your changes.",
			obj(props{"path": str("Project directory (default current dir)")})),
		Handler: func(ctx context.Context, a map[string]any) (string, error) {
			return tools.RunTests(ctx, argStr(a, "path"))
		},
	})

	r.Register(Tool{
		Def: schema("probe",
			"Inspect one or more URLs concurrently: HTTP status, timing, size, redirect target, key "+
				"response/security headers, missing security headers, and the TLS certificate. Pass many "+
				"URLs to batch-probe them at once.",
			obj(props{
				"urls":        arr("URLs to probe"),
				"timeout_sec": integer("Per-request timeout (default 15)"),
			}, "urls")),
		Handler: func(ctx context.Context, a map[string]any) (string, error) {
			return tools.Probe(ctx, argStrSlice(a, "urls"), argInt(a, "timeout_sec"))
		},
	})

	r.Register(Tool{
		Def: schema("crawl",
			"Bounded concurrent same-host crawl from a start URL; returns discovered URLs with page titles.",
			obj(props{
				"url":       str("Start URL"),
				"max_pages": integer("Max pages to crawl (default 20)"),
			}, "url")),
		Handler: func(ctx context.Context, a map[string]any) (string, error) {
			return tools.Crawl(ctx, argStr(a, "url"), argInt(a, "max_pages"))
		},
	})

	r.Register(Tool{
		Def: schema("git_status",
			"Show changed files in the working tree (git status), via pure-Go go-git.",
			obj(props{"path": str("Repo directory (default current dir)")})),
		Handler: func(ctx context.Context, a map[string]any) (string, error) {
			return tools.GitStatus(argStr(a, "path"))
		},
	})

	r.Register(Tool{
		Def: schema("git_log",
			"Show recent commits (hash, date, subject).",
			obj(props{"path": str("Repo directory (default current dir)"), "n": integer("How many commits (default 10)")})),
		Handler: func(ctx context.Context, a map[string]any) (string, error) {
			return tools.GitLog(argStr(a, "path"), argInt(a, "n"))
		},
	})

	r.Register(Tool{
		Def: schema("git_blame",
			"Per-line authorship for a file at HEAD (optionally a line range).",
			obj(props{
				"file":  str("File path (relative to repo)"),
				"path":  str("Repo directory (default current dir)"),
				"start": integer("First line (optional)"),
				"end":   integer("Last line (optional)"),
			}, "file")),
		Handler: func(ctx context.Context, a map[string]any) (string, error) {
			return tools.GitBlame(argStr(a, "path"), argStr(a, "file"), argInt(a, "start"), argInt(a, "end"))
		},
	})

	r.Register(Tool{
		Def: schema("final_answer",
			"Provide the final answer to the user and end the task.",
			obj(props{"answer": str("The complete final answer")}, "answer")),
		// Intercepted by the loop; handler is a no-op fallback.
		Handler: func(ctx context.Context, a map[string]any) (string, error) {
			return argStr(a, "answer"), nil
		},
	})

	return r
}

// ---- schema + arg helpers ----

type props = map[string]any

func obj(p props, required ...string) map[string]any {
	// A nil variadic marshals to JSON `null`, which strict providers (e.g. xAI/grok)
	// reject with "[standard_violation] /required" — emit an empty array instead.
	if required == nil {
		required = []string{}
	}
	return map[string]any{"type": "object", "properties": p, "required": required}
}
func str(desc string) map[string]any { return map[string]any{"type": "string", "description": desc} }
func integer(desc string) map[string]any {
	return map[string]any{"type": "integer", "description": desc}
}
func boolean(desc string) map[string]any {
	return map[string]any{"type": "boolean", "description": desc}
}
func arr(desc string) map[string]any {
	return map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": desc}
}

// argPath reads a single-file path argument accepting the schema key plus the
// synonyms models substitute for it (file/filename/filepath) — a wrong key
// otherwise becomes an empty path and a wasted ENOENT round-trip. NOT used for
// tools taking BOTH a dir and a file (git_blame), where aliasing would misroute.
func argPath(a map[string]any, keys ...string) string {
	for _, k := range keys {
		if v := argStr(a, k); v != "" {
			return v
		}
	}
	return ""
}

func argStr(a map[string]any, key string) string {
	if v, ok := a[key].(string); ok {
		return v
	}
	return ""
}

func argInt(a map[string]any, key string) int {
	switch v := a[key].(type) {
	case float64:
		return int(v)
	case int:
		return v
	}
	return 0
}

func argBool(a map[string]any, key string) bool {
	b, _ := a[key].(bool)
	return b
}

func argStrSlice(a map[string]any, key string) []string {
	v, ok := a[key].([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(v))
	for _, x := range v {
		if s, ok := x.(string); ok {
			out = append(out, s)
		}
	}
	return out
}
