package poller

import (
	"sync"

	"github.com/algonode/aimon/internal/model"
)

// hub is a tiny fan-out broadcaster for SSE subscribers. Sends are non-blocking:
// a slow client simply misses an intermediate frame and catches up on the next.
type hub struct {
	mu   sync.Mutex
	subs map[chan []model.Status]struct{}
}

func newHub() *hub {
	return &hub{subs: make(map[chan []model.Status]struct{})}
}

func (h *hub) subscribe() (<-chan []model.Status, func()) {
	ch := make(chan []model.Status, 4)
	h.mu.Lock()
	h.subs[ch] = struct{}{}
	h.mu.Unlock()

	cancel := func() {
		h.mu.Lock()
		if _, ok := h.subs[ch]; ok {
			delete(h.subs, ch)
			close(ch)
		}
		h.mu.Unlock()
	}
	return ch, cancel
}

func (h *hub) broadcast(snap []model.Status) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs {
		select {
		case ch <- snap:
		default: // subscriber buffer full — drop this frame for them
		}
	}
}
