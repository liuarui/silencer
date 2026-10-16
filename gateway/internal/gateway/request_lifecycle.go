package gateway

import "sync"

// requestDrain keeps a completion signal for each uninterrupted batch of
// in-flight requests. A later batch cannot reopen a retired engine's signal.
type requestDrain struct {
	mu     sync.Mutex
	active int
	idle   chan struct{}
}

func (d *requestDrain) begin() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.active == 0 {
		d.idle = make(chan struct{})
	}
	d.active++
}

func (d *requestDrain) end() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.active--
	if d.active == 0 {
		close(d.idle)
	}
}

func (d *requestDrain) drained() <-chan struct{} {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.idle == nil {
		d.idle = make(chan struct{})
		close(d.idle)
	}
	return d.idle
}
