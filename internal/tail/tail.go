// Package tail keeps the end of a stream: enough of a child process's output
// to explain why it failed.
package tail

import "sync"

// DefaultMax is how much a Buffer with no Max keeps.
const DefaultMax = 4 << 10

// Buffer keeps the last Max bytes written to it, or DefaultMax when Max is
// zero. It is safe for concurrent use.
type Buffer struct {
	Max int

	mu sync.Mutex
	b  []byte
}

func (t *Buffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	limit := t.Max
	if limit <= 0 {
		limit = DefaultMax
	}
	t.b = append(t.b, p...)
	if over := len(t.b) - limit; over > 0 {
		t.b = append(t.b[:0], t.b[over:]...)
	}
	return len(p), nil
}

// String returns what is kept.
func (t *Buffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.b)
}
