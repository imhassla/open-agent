package event

import (
	"encoding/json"
	"os"
	"sync"
)

// Bus fans each event out to its sinks. Safe for concurrent use (parallel
// subagents emit at once); Emit serializes sink delivery.
type Bus struct {
	mu    sync.Mutex
	sinks []func(Event)
}

func NewBus(sinks ...func(Event)) *Bus {
	return &Bus{sinks: sinks}
}

func (b *Bus) Emit(e Event) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, s := range b.sinks {
		s(e)
	}
}

// JSONLSink returns a sink that appends each event as one JSON line to path.
func JSONLSink(path string) func(Event) {
	return func(e Event) {
		f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			return
		}
		defer f.Close()
		if data, err := json.Marshal(e); err == nil {
			f.Write(append(data, '\n'))
		}
	}
}

var _ Emitter = (*Bus)(nil)
