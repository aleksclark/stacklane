package fake

import (
	"context"
	"sync"

	"github.com/aleksclark/stacklane/internal/dockerapi"
)

// Fake is an in-memory dockerapi.Client for tests.
type Fake struct {
	mu         sync.RWMutex
	containers []dockerapi.Container
	subs       []chan dockerapi.Event
	closed     bool
}

// NewFake returns an empty Fake client.
func NewFake() *Fake {
	return &Fake{}
}

// SetContainers replaces the running container set returned by ListRunning.
func (f *Fake) SetContainers(cs []dockerapi.Container) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.containers = cloneContainers(cs)
}

// Emit broadcasts an event to all active Events subscribers.
func (f *Fake) Emit(ev dockerapi.Event) {
	f.mu.RLock()
	subs := make([]chan dockerapi.Event, len(f.subs))
	copy(subs, f.subs)
	f.mu.RUnlock()
	for _, ch := range subs {
		select {
		case ch <- ev:
		default:
			// Drop if subscriber is slow; tests should drain promptly.
			select {
			case ch <- ev:
			default:
			}
		}
	}
}

// ListRunning implements dockerapi.Client.
func (f *Fake) ListRunning(ctx context.Context) ([]dockerapi.Container, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f.mu.RLock()
	defer f.mu.RUnlock()
	return cloneContainers(f.containers), nil
}

// Events implements dockerapi.Client.
func (f *Fake) Events(ctx context.Context) (<-chan dockerapi.Event, <-chan error) {
	events := make(chan dockerapi.Event, 64)
	errs := make(chan error, 1)

	f.mu.Lock()
	f.subs = append(f.subs, events)
	f.mu.Unlock()

	go func() {
		defer func() {
			f.mu.Lock()
			// remove subscription
			out := f.subs[:0]
			for _, ch := range f.subs {
				if ch != events {
					out = append(out, ch)
				}
			}
			f.subs = out
			f.mu.Unlock()
			close(events)
			close(errs)
		}()
		<-ctx.Done()
	}()

	return events, errs
}

// Close implements dockerapi.Client.
func (f *Fake) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
	return nil
}

func cloneContainers(in []dockerapi.Container) []dockerapi.Container {
	if in == nil {
		return nil
	}
	out := make([]dockerapi.Container, len(in))
	for i, c := range in {
		out[i] = c
		if c.Labels != nil {
			labels := make(map[string]string, len(c.Labels))
			for k, v := range c.Labels {
				labels[k] = v
			}
			out[i].Labels = labels
		}
		if c.Ports != nil {
			ports := make([]dockerapi.PortBinding, len(c.Ports))
			copy(ports, c.Ports)
			out[i].Ports = ports
		}
	}
	return out
}
