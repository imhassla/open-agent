package agent

import (
	"bytes"
	"io"
	"sync"
)

// prefixWriter writes each COMPLETE line to w with a fixed prefix, buffering a
// partial trailing line until its newline arrives so a prefix never lands
// mid-line. A shared mutex serializes writes from concurrent tool dispatches so
// their lines don't interleave. Best-effort display only — write errors are
// swallowed (a broken stream must never fail the tool).
type prefixWriter struct {
	w      io.Writer
	mu     *sync.Mutex
	prefix string
	buf    bytes.Buffer // holds the current partial line (guarded by mu)
}

func (p *prefixWriter) Write(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.buf.Write(b)
	for {
		line, err := p.buf.ReadBytes('\n')
		if err != nil {
			// No newline yet: keep the remainder buffered for the next write.
			p.buf.Reset()
			p.buf.Write(line)
			break
		}
		_, _ = io.WriteString(p.w, p.prefix)
		_, _ = p.w.Write(line) // includes the trailing '\n'
	}
	return len(b), nil
}
