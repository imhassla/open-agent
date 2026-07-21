package tools

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
)

// rpcConn is a minimal JSON-RPC 2.0 client over a subprocess's stdio with
// Content-Length framing — the shared transport for the LSP bridge and the MCP
// client. After dialRPC, set onNotify (optional), then call serve() to start the
// read loop (split so a notification can't arrive before its handler is wired).
type rpcConn struct {
	cmd      *exec.Cmd
	stdin    io.WriteCloser
	stdout   io.Reader
	wmu      sync.Mutex // serializes writes to stdin
	mu       sync.Mutex // protects nextID + pending
	nextID   int
	pending  map[int]chan json.RawMessage
	closed   chan struct{}
	onNotify func(method string, params json.RawMessage) // optional notification sink
}

// dialRPC spawns the server. env (nil = inherit) sets the child environment —
// MCP servers often need credentials/PATH entries passed through.
func dialRPC(ctx context.Context, env []string, name string, args ...string) (*rpcConn, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	if env != nil {
		cmd.Env = env
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &rpcConn{
		cmd:     cmd,
		stdin:   stdin,
		stdout:  stdout,
		pending: map[int]chan json.RawMessage{},
		closed:  make(chan struct{}),
	}, nil
}

// serve starts the read loop. Set onNotify before calling it.
func (c *rpcConn) serve() { go c.readLoop(bufio.NewReader(c.stdout)) }

// shutdown closes stdin, cancels the process context (killing it), then Wait()s to
// reap it — without Wait the process becomes a zombie and its stdout FD leaks.
func (c *rpcConn) shutdown(cancel context.CancelFunc) {
	_ = c.stdin.Close()
	cancel()
	if c.cmd != nil {
		_ = c.cmd.Wait()
	}
}

func (c *rpcConn) readLoop(rd *bufio.Reader) {
	for {
		msg, err := readRPCMessage(rd)
		if err != nil {
			close(c.closed)
			return
		}
		var m struct {
			ID     *json.RawMessage `json:"id"`
			Method string           `json:"method"`
			Params json.RawMessage  `json:"params"`
		}
		_ = json.Unmarshal(msg, &m)
		switch {
		case m.ID != nil && m.Method != "":
			// server -> client request: reply null so the server doesn't stall.
			// Write from a separate goroutine so the read loop never blocks on
			// stdin backpressure (a chatty server could otherwise deadlock both pipes).
			var id any
			_ = json.Unmarshal(*m.ID, &id)
			go c.write(map[string]any{"jsonrpc": "2.0", "id": id, "result": nil})
		case m.ID != nil:
			var idn int
			_ = json.Unmarshal(*m.ID, &idn)
			c.mu.Lock()
			ch := c.pending[idn]
			delete(c.pending, idn)
			c.mu.Unlock()
			if ch != nil {
				ch <- msg
			}
		case m.Method != "":
			if c.onNotify != nil {
				c.onNotify(m.Method, m.Params)
			}
		}
	}
}

func readRPCMessage(rd *bufio.Reader) ([]byte, error) {
	length := 0
	for {
		line, err := rd.ReadString('\n')
		if err != nil {
			return nil, err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			break
		}
		if v, ok := strings.CutPrefix(line, "Content-Length:"); ok {
			fmt.Sscanf(strings.TrimSpace(v), "%d", &length)
		}
	}
	if length <= 0 {
		return nil, fmt.Errorf("missing content-length")
	}
	buf := make([]byte, length)
	if _, err := io.ReadFull(rd, buf); err != nil {
		return nil, err
	}
	return buf, nil
}

func (c *rpcConn) write(v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	c.wmu.Lock()
	defer c.wmu.Unlock()
	_, err = fmt.Fprintf(c.stdin, "Content-Length: %d\r\n\r\n%s", len(data), data)
	return err
}

// call sends a request and waits for its response (or ctx/closed).
func (c *rpcConn) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	c.mu.Lock()
	id := c.nextID
	c.nextID++
	ch := make(chan json.RawMessage, 1)
	c.pending[id] = ch
	c.mu.Unlock()

	if err := c.write(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}); err != nil {
		return nil, err
	}
	select {
	case <-ctx.Done():
		c.dropPending(id)
		return nil, ctx.Err()
	case <-c.closed:
		c.dropPending(id)
		return nil, fmt.Errorf("rpc server closed unexpectedly")
	case msg := <-ch:
		var r struct {
			Result json.RawMessage `json:"result"`
			Error  *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		_ = json.Unmarshal(msg, &r)
		if r.Error != nil {
			return nil, fmt.Errorf("rpc error: %s", r.Error.Message)
		}
		return r.Result, nil
	}
}

func (c *rpcConn) notify(method string, params any) {
	_ = c.write(map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
}

// dropPending removes an abandoned request so the pending map doesn't grow on
// ctx-cancel / connection-close (the read loop's nil-check tolerates the race).
func (c *rpcConn) dropPending(id int) {
	c.mu.Lock()
	delete(c.pending, id)
	c.mu.Unlock()
}
