package runner

import (
	"context"
	"sync"
)

// RunHandle exposes streaming events independently from terminal completion.
// Callers that do not need events may call Wait directly; production continues
// because an internal queue decouples process reads from event consumption.
type RunHandle struct {
	events <-chan Event
	cancel context.CancelFunc
	done   chan struct{}

	mu     sync.RWMutex
	result Result
	err    error
}

func (h *RunHandle) Events() <-chan Event { return h.events }

func (h *RunHandle) Cancel() {
	if h != nil && h.cancel != nil {
		h.cancel()
	}
}

func (h *RunHandle) Wait() (Result, error) {
	if h == nil {
		return Result{}, &RunError{Kind: ErrorInvalidRequest, Op: "wait"}
	}
	<-h.done
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.result, h.err
}

func (h *RunHandle) complete(result Result, err error) {
	h.mu.Lock()
	h.result = result
	h.err = err
	h.mu.Unlock()
	close(h.done)
}

func pumpEvents(in <-chan Event, out chan<- Event) {
	defer close(out)
	queue := make([]Event, 0, 64)
	for in != nil || len(queue) > 0 {
		if len(queue) == 0 {
			event, ok := <-in
			if !ok {
				in = nil
				continue
			}
			queue = append(queue, event)
			continue
		}

		select {
		case event, ok := <-in:
			if !ok {
				in = nil
			} else {
				queue = append(queue, event)
			}
		case out <- queue[0]:
			queue[0] = Event{}
			queue = queue[1:]
		}
	}
}
