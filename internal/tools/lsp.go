package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// lspServer maps a file extension to a language server invocation.
type lspServer struct {
	cmd    string
	args   []string
	langID string
}

// AnyLSPServerAvailable reports whether at least one supported language server is
// on PATH, so the lsp_* tools are only advertised when they can actually work.
func AnyLSPServerAvailable() bool {
	for _, cmd := range []string{"gopls", "pyright-langserver", "typescript-language-server", "rust-analyzer"} {
		if _, err := exec.LookPath(cmd); err == nil {
			return true
		}
	}
	return false
}

func lspServerFor(path string) (lspServer, bool) {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".go":
		return lspServer{"gopls", nil, "go"}, true
	case ".py":
		return lspServer{"pyright-langserver", []string{"--stdio"}, "python"}, true
	case ".ts", ".tsx":
		return lspServer{"typescript-language-server", []string{"--stdio"}, "typescript"}, true
	case ".js", ".jsx":
		return lspServer{"typescript-language-server", []string{"--stdio"}, "javascript"}, true
	case ".rs":
		return lspServer{"rust-analyzer", nil, "rust"}, true
	}
	return lspServer{}, false
}

// lspClient is the LSP-specific layer over the shared JSON-RPC transport: it adds
// publishDiagnostics capture on top of rpcConn (call/notify/shutdown promoted).
type lspClient struct {
	*rpcConn
	diagsMu sync.Mutex
	diags   map[string]json.RawMessage
	diagVer map[string]int // bumped on each publishDiagnostics, so callers can settle
}

func newLSPClient(ctx context.Context, srv lspServer) (*lspClient, error) {
	rc, err := dialRPC(ctx, nil, srv.cmd, srv.args...)
	if err != nil {
		return nil, err
	}
	c := &lspClient{
		rpcConn: rc,
		diags:   map[string]json.RawMessage{},
		diagVer: map[string]int{},
	}
	// Wire the diagnostics sink BEFORE serving so no notification is missed.
	rc.onNotify = func(method string, params json.RawMessage) {
		if method != "textDocument/publishDiagnostics" {
			return
		}
		var p struct {
			URI         string          `json:"uri"`
			Diagnostics json.RawMessage `json:"diagnostics"`
		}
		_ = json.Unmarshal(params, &p)
		c.diagsMu.Lock()
		c.diags[p.URI] = p.Diagnostics
		c.diagVer[p.URI]++
		c.diagsMu.Unlock()
	}
	rc.serve()
	return c, nil
}

// lspSession spawns a server for file, runs the LSP handshake + didOpen, then fn.
func lspSession(ctx context.Context, file string, fn func(c *lspClient, uri string) (string, error)) (string, error) {
	abs, err := filepath.Abs(file)
	if err != nil {
		return "", err
	}
	srv, ok := lspServerFor(abs)
	if !ok {
		return "", fmt.Errorf("no language server configured for %s files", filepath.Ext(abs))
	}
	if _, err := exec.LookPath(srv.cmd); err != nil {
		return "", fmt.Errorf("language server %q is not installed (install it to use lsp tools)", srv.cmd)
	}
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)

	c, err := newLSPClient(cctx, srv)
	if err != nil {
		cancel()
		return "", err
	}
	defer c.shutdown(cancel)

	root := "file://" + filepath.Dir(abs)
	if _, err := c.call(cctx, "initialize", map[string]any{
		"processId": os.Getpid(),
		"rootUri":   root,
		// Declare the capabilities we actually consume — some servers gate
		// diagnostics/hover on declared client capabilities and stay silent otherwise.
		"capabilities": map[string]any{
			"textDocument": map[string]any{
				"synchronization":    map[string]any{"didSave": false, "dynamicRegistration": false},
				"publishDiagnostics": map[string]any{"relatedInformation": true},
				"hover":              map[string]any{"contentFormat": []string{"markdown", "plaintext"}},
				"definition":         map[string]any{"dynamicRegistration": false},
				"references":         map[string]any{"dynamicRegistration": false},
			},
		},
	}); err != nil {
		return "", err
	}
	c.notify("initialized", map[string]any{})

	src, err := os.ReadFile(abs)
	if err != nil {
		return "", err
	}
	uri := "file://" + abs
	c.notify("textDocument/didOpen", map[string]any{
		"textDocument": map[string]any{"uri": uri, "languageId": srv.langID, "version": 1, "text": string(src)},
	})
	return fn(c, uri)
}

func docPos(uri string, line, character int) map[string]any {
	if line > 0 {
		line-- // tool args are 1-based; LSP is 0-based
	}
	if character > 0 {
		character--
	}
	return map[string]any{
		"textDocument": map[string]any{"uri": uri},
		"position":     map[string]any{"line": line, "character": character},
	}
}

// LSPDiagnostics returns compiler/linter diagnostics for a file. Servers
// (gopls/pyright/tsserver) routinely publish an empty set immediately after
// didOpen and the real diagnostics a beat later, so we wait for the result to
// SETTLE (a quiet period with no further publishDiagnostics) rather than
// returning on the first notification — which would falsely report "clean".
func LSPDiagnostics(ctx context.Context, file string) (string, error) {
	return lspSession(ctx, file, func(c *lspClient, uri string) (string, error) {
		const settle = 600 * time.Millisecond
		deadline := time.Now().Add(10 * time.Second)
		lastVer := -1
		var stableSince time.Time
		var latest json.RawMessage
		haveAny := false
		for time.Now().Before(deadline) {
			c.diagsMu.Lock()
			d, ok := c.diags[uri]
			ver := c.diagVer[uri]
			c.diagsMu.Unlock()
			if ok {
				haveAny = true
				latest = d
				if ver != lastVer {
					lastVer = ver
					stableSince = time.Now()
				} else if time.Since(stableSince) >= settle {
					return formatDiagnostics(latest), nil // settled
				}
			}
			select {
			case <-c.closed:
				if haveAny {
					return formatDiagnostics(latest), nil
				}
				return "", fmt.Errorf("language server exited")
			case <-time.After(100 * time.Millisecond):
			}
		}
		if haveAny {
			return formatDiagnostics(latest), nil
		}
		return "(no diagnostics reported within timeout)", nil
	})
}

func formatDiagnostics(raw json.RawMessage) string {
	var ds []struct {
		Range struct {
			Start struct{ Line, Character int } `json:"start"`
		} `json:"range"`
		Severity int    `json:"severity"`
		Message  string `json:"message"`
	}
	if json.Unmarshal(raw, &ds) != nil || len(ds) == 0 {
		return "no diagnostics (clean)"
	}
	sev := map[int]string{1: "error", 2: "warning", 3: "info", 4: "hint"}
	var b strings.Builder
	for _, d := range ds {
		fmt.Fprintf(&b, "%d:%d %s: %s\n", d.Range.Start.Line+1, d.Range.Start.Character+1, sev[d.Severity], d.Message)
	}
	return strings.TrimRight(b.String(), "\n")
}

// LSPHover returns hover info (type/docs) at a position.
func LSPHover(ctx context.Context, file string, line, character int) (string, error) {
	return lspSession(ctx, file, func(c *lspClient, uri string) (string, error) {
		res, err := c.call(ctx, "textDocument/hover", docPos(uri, line, character))
		if err != nil {
			return "", err
		}
		var h struct {
			Contents struct {
				Value string `json:"value"`
			} `json:"contents"`
		}
		if json.Unmarshal(res, &h) == nil && h.Contents.Value != "" {
			return h.Contents.Value, nil
		}
		return "(no hover info)", nil
	})
}

// LSPDefinition returns the definition location(s) of the symbol at a position.
func LSPDefinition(ctx context.Context, file string, line, character int) (string, error) {
	return lspSession(ctx, file, func(c *lspClient, uri string) (string, error) {
		res, err := c.call(ctx, "textDocument/definition", docPos(uri, line, character))
		if err != nil {
			return "", err
		}
		return formatLocations(res), nil
	})
}

// LSPReferences returns all references to the symbol at a position.
func LSPReferences(ctx context.Context, file string, line, character int) (string, error) {
	return lspSession(ctx, file, func(c *lspClient, uri string) (string, error) {
		params := docPos(uri, line, character)
		params["context"] = map[string]any{"includeDeclaration": true}
		res, err := c.call(ctx, "textDocument/references", params)
		if err != nil {
			return "", err
		}
		return formatLocations(res), nil
	})
}

func formatLocations(raw json.RawMessage) string {
	type loc struct {
		URI   string `json:"uri"`
		Range struct {
			Start struct{ Line, Character int } `json:"start"`
		} `json:"range"`
	}
	var locs []loc
	if json.Unmarshal(raw, &locs) != nil {
		var single loc
		if json.Unmarshal(raw, &single) == nil && single.URI != "" {
			locs = []loc{single}
		}
	}
	if len(locs) == 0 {
		return "(not found)"
	}
	var b strings.Builder
	for _, l := range locs {
		path := strings.TrimPrefix(l.URI, "file://")
		fmt.Fprintf(&b, "%s:%d:%d\n", path, l.Range.Start.Line+1, l.Range.Start.Character+1)
	}
	return strings.TrimRight(b.String(), "\n")
}
