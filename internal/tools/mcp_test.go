package tools

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseMCPTools(t *testing.T) {
	res := json.RawMessage(`{"tools":[
		{"name":"search","description":"web search","inputSchema":{"type":"object","properties":{"q":{"type":"string"}}}},
		{"name":"fetch","description":"get url"}]}`)
	ts, err := parseMCPTools(res)
	if err != nil {
		t.Fatal(err)
	}
	if len(ts) != 2 || ts[0].Name != "search" || ts[1].Name != "fetch" {
		t.Fatalf("parsed tools wrong: %+v", ts)
	}
	if ts[0].InputSchema["type"] != "object" {
		t.Errorf("inputSchema not preserved: %v", ts[0].InputSchema)
	}
}

func TestParseMCPToolResult(t *testing.T) {
	ok, err := parseMCPToolResult(json.RawMessage(`{"content":[{"type":"text","text":"hello"},{"type":"text","text":"world"}]}`))
	if err != nil || ok != "hello\nworld" {
		t.Fatalf("ok result = %q, err = %v", ok, err)
	}
	if _, err := parseMCPToolResult(json.RawMessage(`{"content":[{"type":"text","text":"boom"}],"isError":true}`)); err == nil {
		t.Error("isError result should be an error")
	}
}

func TestLoadMCPServers(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir) // guardrails confine writes to cwd
	p := filepath.Join(dir, ".mcp.json")
	writeFile(t, p, `{"mcpServers":{"fs":{"command":"mcp-fs","args":["--root","/tmp"]},"web":{"command":"mcp-web"}}}`)
	servers, err := LoadMCPServers(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(servers) != 2 || servers[0].Name != "fs" || servers[1].Name != "web" {
		t.Fatalf("servers wrong (want stable order fs,web): %+v", servers)
	}
	if servers[0].Command != "mcp-fs" || len(servers[0].Args) != 2 {
		t.Errorf("fs server parsed wrong: %+v", servers[0])
	}
	// Missing file → (nil, nil).
	if s, err := LoadMCPServers(filepath.Join(dir, "nope.json")); err != nil || s != nil {
		t.Errorf("missing file should be (nil,nil), got %v %v", s, err)
	}
}

// TestRPCCallRoundTrip drives rpcConn over in-memory pipes against a fake server,
// exercising serve + write framing + readLoop + call response routing.
func TestRPCCallRoundTrip(t *testing.T) {
	cr, cw := io.Pipe() // client → server
	sr, sw := io.Pipe() // server → client
	c := &rpcConn{stdin: cw, stdout: sr, pending: map[int]chan json.RawMessage{}, closed: make(chan struct{})}
	c.serve()

	go func() {
		rd := bufio.NewReader(cr)
		msg, err := readRPCMessage(rd)
		if err != nil {
			return
		}
		var m struct {
			ID int `json:"id"`
		}
		_ = json.Unmarshal(msg, &m)
		resp := fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"result":{"ok":true}}`, m.ID)
		fmt.Fprintf(sw, "Content-Length: %d\r\n\r\n%s", len(resp), resp)
	}()

	res, err := c.call(context.Background(), "ping", map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(res), `"ok":true`) {
		t.Errorf("unexpected result: %s", res)
	}
}
