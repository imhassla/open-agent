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
	diagsMu    sync.Mutex
	diags      map[string]json.RawMessage
	diagVer    map[string]int // bumped on each publishDiagnostics, so callers can settle
	diagDocVer map[string]int // the DOC version the server echoed (0 = server omits it)
}

func newLSPClient(ctx context.Context, srv lspServer) (*lspClient, error) {
	rc, err := dialRPC(ctx, nil, srv.cmd, srv.args...)
	if err != nil {
		return nil, err
	}
	c := &lspClient{
		rpcConn:    rc,
		diags:      map[string]json.RawMessage{},
		diagVer:    map[string]int{},
		diagDocVer: map[string]int{},
	}
	// Wire the diagnostics sink BEFORE serving so no notification is missed.
	rc.onNotify = func(method string, params json.RawMessage) {
		if method != "textDocument/publishDiagnostics" {
			return
		}
		var p struct {
			URI         string          `json:"uri"`
			Version     int             `json:"version"` // doc version the server type-checked (0 when omitted)
			Diagnostics json.RawMessage `json:"diagnostics"`
		}
		_ = json.Unmarshal(params, &p)
		c.diagsMu.Lock()
		c.diags[p.URI] = p.Diagnostics
		c.diagVer[p.URI]++
		c.diagDocVer[p.URI] = p.Version
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

// ---------------------------------------------------------------------------
// Warm sessions — persistent per-process language servers for the post-edit
// verify hook. The lsp_* tools above spawn a fresh server per call (simple,
// stateless, self-cleaning); the hook cannot afford a cold start on every
// edit, so it uses this registry instead: StartLSPWarmup boots a server at
// most ONCE per language in the background, and LSPWarmDiagnostics answers
// only from an already-warm server, skipping (ok=false) otherwise. Sessions
// live until ShutdownWarmLSPSessions or process exit (the child reads EOF on
// its stdin pipe and exits with us).
// ---------------------------------------------------------------------------

// warmLSP is one persistent language-server session shared by all warm
// diagnostics queries for its language.
type warmLSP struct {
	c      *lspClient
	cancel context.CancelFunc
	mu     sync.Mutex     // serializes didClose/didOpen sequencing per session
	vers   map[string]int // uri -> last didOpen version sent
}

var (
	warmMu      sync.Mutex
	warmLSPs    = map[string]*warmLSP{} // key: server command ("gopls", ...)
	warmPending = map[string]uint64{}   // boot TOKEN in flight (0 = none); tokens make register compare-and-claim
	warmFailed  = map[string]bool{}     // failed/uninstalled once; never retried this process
	warmBootSeq uint64                  // token source (under warmMu)
)

// StartLSPWarmup asynchronously boots a persistent language server for file's
// language so LATER LSPWarmDiagnostics calls can answer fast. Non-blocking and
// idempotent: it no-ops when the language is unsupported, the server binary is
// missing, a session is already warm or booting, or a previous boot failed
// (failures are never retried within the process — respawn churn on every edit
// would cost more than the diagnostics are worth).
func StartLSPWarmup(file string) {
	srv, ok := lspServerFor(file)
	if !ok {
		return
	}
	warmMu.Lock()
	defer warmMu.Unlock()
	if warmLSPs[srv.cmd] != nil || warmPending[srv.cmd] != 0 || warmFailed[srv.cmd] {
		return
	}
	if _, err := exec.LookPath(srv.cmd); err != nil {
		warmFailed[srv.cmd] = true
		return
	}
	root, err := os.Getwd()
	if err != nil {
		warmFailed[srv.cmd] = true
		return
	}
	warmBootSeq++
	tok := warmBootSeq
	warmPending[srv.cmd] = tok
	go warmLSPBoot(srv, root, tok)
}

// warmLSPBoot dials + initializes a server, then registers it as warm. Runs in
// its own goroutine and always terminates: initialize is bounded by a 30s
// timeout and every exit path clears the pending flag (registering the session
// or recording failure) — no goroutine or process leak. The token makes
// registration compare-and-claim: after a Shutdown→re-warmup cycle an OLD boot
// (rooted at a stale cwd) can no longer win the race against the new boot and
// register itself (review A3).
func warmLSPBoot(srv lspServer, root string, tok uint64) {
	ctx, cancel := context.WithCancel(context.Background())
	// The session object owns cancel for the server's whole lifetime (this is a
	// deliberately long-lived context, released by ShutdownWarmLSPSessions or a
	// dead-server deregistration in LSPWarmDiagnostics).
	w := &warmLSP{cancel: cancel, vers: map[string]int{}}
	fail := func() {
		warmMu.Lock()
		if warmPending[srv.cmd] == tok {
			delete(warmPending, srv.cmd)
			warmFailed[srv.cmd] = true
		}
		warmMu.Unlock()
	}
	c, err := newLSPClient(ctx, srv)
	if err != nil {
		w.cancel()
		fail()
		return
	}
	w.c = c
	ictx, icancel := context.WithTimeout(ctx, 30*time.Second)
	defer icancel()
	if _, err := c.call(ictx, "initialize", map[string]any{
		"processId": os.Getpid(),
		"rootUri":   "file://" + root,
		"capabilities": map[string]any{
			"textDocument": map[string]any{
				"synchronization":    map[string]any{"didSave": false, "dynamicRegistration": false},
				"publishDiagnostics": map[string]any{"relatedInformation": true},
			},
		},
	}); err != nil {
		c.shutdown(w.cancel)
		fail()
		return
	}
	c.notify("initialized", map[string]any{})
	warmMu.Lock()
	registered := false
	if warmPending[srv.cmd] == tok { // still wanted AND still OUR boot (no Shutdown→re-warmup mid-boot)
		delete(warmPending, srv.cmd)
		warmLSPs[srv.cmd] = w
		registered = true
	}
	warmMu.Unlock()
	if !registered {
		c.shutdown(w.cancel) // registry was reset while booting — don't leak the server
	}
}

// diagSnapshot returns the last published diagnostics, the publish counter, and
// the doc version the server echoed for uri (0 when the server omits it).
func (c *lspClient) diagSnapshot(uri string) (json.RawMessage, int, int) {
	c.diagsMu.Lock()
	defer c.diagsMu.Unlock()
	return c.diags[uri], c.diagVer[uri], c.diagDocVer[uri]
}

// LSPWarmDiagnostics returns an error-only diagnostics summary for file from an
// ALREADY-warm language server (see StartLSPWarmup). It never boots a server.
// ok=false means the check did not run (no warm server, dead server, read
// failure, or no publish before ctx expired) — callers must treat that as "no
// information", not "clean". ok=true with an empty summary means the server
// reported no error-severity diagnostics. At most maxItems errors are included
// ("(+N more)" marks overflow); warnings/info/hints are dropped on purpose —
// mid-multi-file-change they are mostly noise.
func LSPWarmDiagnostics(ctx context.Context, file string, maxItems int) (string, bool) {
	abs, err := filepath.Abs(file)
	if err != nil {
		return "", false
	}
	srv, ok := lspServerFor(abs)
	if !ok {
		return "", false
	}
	warmMu.Lock()
	w := warmLSPs[srv.cmd]
	warmMu.Unlock()
	if w == nil {
		return "", false
	}
	src, err := os.ReadFile(abs)
	if err != nil {
		return "", false
	}
	uri := "file://" + abs
	_, before, _ := w.c.diagSnapshot(uri)
	w.mu.Lock()
	if w.vers[uri] > 0 {
		// Re-syncing an already-open doc: close + reopen replaces the content
		// wholesale without negotiating the server's didChange sync kind.
		w.c.notify("textDocument/didClose", map[string]any{
			"textDocument": map[string]any{"uri": uri},
		})
	}
	w.vers[uri]++
	wantVer := w.vers[uri]
	w.c.notify("textDocument/didOpen", map[string]any{
		"textDocument": map[string]any{"uri": uri, "languageId": srv.langID, "version": wantVer, "text": string(src)},
	})
	w.mu.Unlock()

	// Wait for a publish NEWER than `before`, then a short quiet period —
	// servers often publish an empty set first and the real result a beat
	// later (same settle rationale as LSPDiagnostics, but on a warm server the
	// window can be much shorter). ctx bounds the whole wait.
	const settle = 250 * time.Millisecond
	var latest json.RawMessage
	fresh := false
	var stableSince time.Time
	lastVer := before
	for {
		d, ver, docVer := w.c.diagSnapshot(uri)
		if ver != lastVer {
			lastVer = ver
			// Freshness needs more than "a publish arrived after we opened":
			// a late publish from a PREVIOUS query's type-check (or a didClose
			// clear) also bumps the counter and would surface stale errors or a
			// false clean. When the server echoes the doc version, require it
			// to be ≥ the version we just sent; servers that omit it (docVer 0)
			// keep the counter-only behavior (review A2).
			if docVer == 0 || docVer >= wantVer {
				latest, fresh = d, true
				stableSince = time.Now()
			}
		} else if fresh && time.Since(stableSince) >= settle {
			return formatErrorSummary(latest, maxItems), true
		}
		select {
		case <-ctx.Done():
			if fresh {
				return formatErrorSummary(latest, maxItems), true // unsettled but real
			}
			return "", false
		case <-w.c.closed:
			// Server died mid-query: deregister so future calls skip instantly.
			// shutdown (not bare cancel) — it is the package's only Wait() site,
			// so the dead child is reaped and the parent-side stdin FD closed
			// instead of leaking a zombie until process exit (review A1).
			warmMu.Lock()
			if warmLSPs[srv.cmd] == w {
				delete(warmLSPs, srv.cmd)
				warmFailed[srv.cmd] = true
			}
			warmMu.Unlock()
			w.c.shutdown(w.cancel)
			return "", false
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// formatErrorSummary renders at most maxItems ERROR-severity diagnostics as a
// single "L:C msg; L:C msg (+N more)" line; "" when there are none.
func formatErrorSummary(raw json.RawMessage, maxItems int) string {
	var ds []struct {
		Range struct {
			Start struct{ Line, Character int } `json:"start"`
		} `json:"range"`
		Severity int    `json:"severity"`
		Message  string `json:"message"`
	}
	if json.Unmarshal(raw, &ds) != nil {
		return ""
	}
	var parts []string
	extra := 0
	for _, d := range ds {
		if d.Severity != 1 {
			continue
		}
		if len(parts) >= maxItems {
			extra++
			continue
		}
		msg := strings.ReplaceAll(d.Message, "\n", " ")
		parts = append(parts, fmt.Sprintf("%d:%d %s", d.Range.Start.Line+1, d.Range.Start.Character+1, msg))
	}
	if len(parts) == 0 {
		return ""
	}
	s := strings.Join(parts, "; ")
	if extra > 0 {
		s += fmt.Sprintf(" (+%d more)", extra)
	}
	return s
}

// ShutdownWarmLSPSessions kills every warm language server and resets the warm
// registry, including failure markers (so a later StartLSPWarmup may boot
// again). Used by tests and available for graceful teardown; safe anytime.
func ShutdownWarmLSPSessions() {
	warmMu.Lock()
	sessions := warmLSPs
	warmLSPs = map[string]*warmLSP{}
	warmPending = map[string]uint64{}
	warmFailed = map[string]bool{}
	warmMu.Unlock()
	for _, w := range sessions {
		w.c.shutdown(w.cancel)
	}
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
