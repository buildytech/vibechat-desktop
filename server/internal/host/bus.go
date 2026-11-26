package host

import (
	"sync"

	"github.com/buildytech/vibechat-desktop/server/internal/agent"
)

// Bus fans agent events out to the SSE subscribers of a session. Slow subscribers drop events (the transcript is the
// source of truth; the stream is live trace only).
type Bus struct {
	mu   sync.Mutex
	subs map[string]map[chan agent.Event]struct{}
}

// NewBus is an empty bus.
func NewBus() *Bus { return &Bus{subs: map[string]map[chan agent.Event]struct{}{}} }

// Subscribe returns a channel of session events and its cancel func.
func (b *Bus) Subscribe(sid string) (<-chan agent.Event, func()) {
	ch := make(chan agent.Event, 512)
	b.mu.Lock()
	if b.subs[sid] == nil {
		b.subs[sid] = map[chan agent.Event]struct{}{}
	}
	b.subs[sid][ch] = struct{}{}
	b.mu.Unlock()
	return ch, func() {
		b.mu.Lock()
		delete(b.subs[sid], ch)
		if len(b.subs[sid]) == 0 {
			delete(b.subs, sid)
		}
		b.mu.Unlock()
	}
}

// Publish sends e to every subscriber of sid.
func (b *Bus) Publish(sid string, e agent.Event) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for ch := range b.subs[sid] {
		select {
		case ch <- e:
		default:
		}
	}
}
