package gateway

import (
	"context"
	"errors"
)

// Host-side actions have their own process group, independent of the native
// Claude worker. Keep their lifetime visible to admission and shutdown.
func (g *Gateway) beginAction(ctx context.Context, room, run string) (*actionExecution, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closing || ctx.Err() != nil {
		return nil, context.Canceled
	}
	for _, a := range g.actions {
		if a.room == room && a.unconfirmed {
			return nil, errors.New("project action process termination remains unconfirmed")
		}
	}
	a := &actionExecution{room: room, done: make(chan struct{})}
	g.actions[run] = a
	return a, nil
}
func (g *Gateway) endAction(run string, a *actionExecution, unconfirmed bool) {
	g.mu.Lock()
	a.unconfirmed = unconfirmed
	if !unconfirmed {
		delete(g.actions, run)
	}
	close(a.done)
	s := g.slots[a.room]
	g.mu.Unlock()
	if s != nil {
		s.mu.Lock()
		if unconfirmed {
			s.stopUnconfirmed = true
		}
		ended := !s.live && !s.stopUnconfirmed
		s.mu.Unlock()
		if ended {
			g.removeSlot(s)
		}
	}
	if !unconfirmed {
		g.kickWaiting()
	}
}
func (g *Gateway) waitRoomActions(ctx context.Context, room string) error {
	g.mu.Lock()
	var actions []*actionExecution
	for _, a := range g.actions {
		if a.room == room {
			actions = append(actions, a)
		}
	}
	g.mu.Unlock()
	for _, a := range actions {
		select {
		case <-a.done:
		case <-ctx.Done():
			return ctx.Err()
		}
		g.mu.Lock()
		unconfirmed := a.unconfirmed
		g.mu.Unlock()
		if unconfirmed {
			return errors.New("project action process termination remains unconfirmed")
		}
	}
	return nil
}
