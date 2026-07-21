package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"
)

// MCPServer is a configured MCP stdio server (command + args + extra env).
type MCPServer struct {
	Name    string
	Command string
	Args    []string
	Env     []string // extra "K=V" entries appended to the inherited environment
}

// MCPTool is a tool advertised by an MCP server (its JSON-Schema input included).
type MCPTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

// MCPClient is a live connection to one MCP stdio server over the shared JSON-RPC
// transport (the same rpcConn the LSP bridge uses).
type MCPClient struct {
	Name   string
	rpc    *rpcConn
	cancel context.CancelFunc
	tools  []MCPTool
}

// StartMCP spawns the server, runs the MCP initialize handshake, and lists its
// tools. The connection stays open until Close. The process is scoped to ctx.
func StartMCP(ctx context.Context, srv MCPServer) (*MCPClient, error) {
	cctx, cancel := context.WithCancel(ctx)
	var env []string
	if len(srv.Env) > 0 {
		env = append(os.Environ(), srv.Env...)
	}
	rc, err := dialRPC(cctx, env, srv.Command, srv.Args...)
	if err != nil {
		cancel()
		return nil, err
	}
	rc.serve()
	c := &MCPClient{Name: srv.Name, rpc: rc, cancel: cancel}
	// Bound the handshake so a live-but-silent server can't hang the whole CLI at
	// startup. The connection itself stays scoped to cctx (the run lifetime).
	hctx, hcancel := context.WithTimeout(cctx, 20*time.Second)
	defer hcancel()
	if _, err := rc.call(hctx, "initialize", map[string]any{
		"protocolVersion": "2024-11-05",
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "open-agent", "version": "1"},
	}); err != nil {
		c.Close()
		return nil, fmt.Errorf("mcp %s initialize: %w", srv.Name, err)
	}
	rc.notify("notifications/initialized", map[string]any{})
	ts, err := c.listTools(hctx)
	if err != nil {
		c.Close()
		return nil, fmt.Errorf("mcp %s tools/list: %w", srv.Name, err)
	}
	c.tools = ts
	return c, nil
}

// Tools returns the server's advertised tools (cached from the handshake).
func (c *MCPClient) Tools() []MCPTool { return c.tools }

// Close shuts down and reaps the server process.
func (c *MCPClient) Close() {
	if c.cancel != nil {
		c.rpc.shutdown(c.cancel)
	}
}

func (c *MCPClient) listTools(ctx context.Context) ([]MCPTool, error) {
	res, err := c.rpc.call(ctx, "tools/list", map[string]any{})
	if err != nil {
		return nil, err
	}
	return parseMCPTools(res)
}

func parseMCPTools(res json.RawMessage) ([]MCPTool, error) {
	var r struct {
		Tools []MCPTool `json:"tools"`
	}
	if err := json.Unmarshal(res, &r); err != nil {
		return nil, err
	}
	return r.Tools, nil
}

// CallTool invokes an MCP tool and returns its text content joined.
func (c *MCPClient) CallTool(ctx context.Context, name string, args map[string]any) (string, error) {
	res, err := c.rpc.call(ctx, "tools/call", map[string]any{"name": name, "arguments": args})
	if err != nil {
		return "", err
	}
	return parseMCPToolResult(res)
}

func parseMCPToolResult(res json.RawMessage) (string, error) {
	var r struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	if err := json.Unmarshal(res, &r); err != nil {
		return "", err
	}
	var b strings.Builder
	for _, part := range r.Content {
		if part.Text != "" {
			b.WriteString(part.Text)
			b.WriteByte('\n')
		}
	}
	out := strings.TrimRight(b.String(), "\n")
	if r.IsError {
		return "", fmt.Errorf("mcp tool error: %s", out)
	}
	if out == "" {
		out = "(no content)"
	}
	return out, nil
}

// LoadMCPServers reads an .mcp.json config ({"mcpServers": {name: {command,args}}})
// and returns the servers in stable name order. Missing file → (nil, nil).
func LoadMCPServers(path string) ([]MCPServer, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var cfg struct {
		MCPServers map[string]struct {
			Command string            `json:"command"`
			Args    []string          `json:"args"`
			Env     map[string]string `json:"env"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	var out []MCPServer
	for name, s := range cfg.MCPServers {
		if s.Command == "" {
			continue
		}
		var env []string
		for k, v := range s.Env {
			env = append(env, k+"="+v)
		}
		sort.Strings(env) // stable
		out = append(out, MCPServer{Name: name, Command: s.Command, Args: s.Args, Env: env})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}
