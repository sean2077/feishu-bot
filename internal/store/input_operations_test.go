package store

import (
	"context"
	"errors"
	"testing"

	"github.com/sean2077/feishu-bot/internal/core"
)

func queuedOperationInput(t *testing.T, s *Store, id, actor string, batch *core.ImageBatch, command *core.Command) core.Delivery {
	t.Helper()
	m := core.Message{ID: id, SenderID: actor, Time: 1, Text: "input"}
	if batch != nil {
		m.ImageOnly = true
		m.Resources = []core.Resource{{Kind: "image", Key: id}}
	}
	if _, err := s.Insert("r", m, "queued"); err != nil {
		t.Fatal(err)
	}
	d := core.Delivery{UUID: "uuid-" + id, RoomKey: "r", SessionID: "session", Message: m, ImageBatch: batch, Command: command}
	if err := s.Queue(d); err != nil {
		t.Fatal(err)
	}
	return d
}

func TestCancelQueuedInputOwnsRoomAuthorAndDispatchState(t *testing.T) {
	s := openTest(t)
	d := queuedOperationInput(t, s, "one", "author", nil, nil)
	for _, attempt := range []struct {
		room, actor string
		want        error
	}{{"other", "author", ErrInputNotFound}, {"r", "someone", ErrInputNotOwned}} {
		if _, err := s.CancelQueuedInput(context.Background(), attempt.room, d.UUID, attempt.actor, false, 2); !errors.Is(err, attempt.want) {
			t.Fatal(err)
		}
	}
	if err := s.Mark(d.UUID, "sent"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CancelQueuedInput(context.Background(), "r", d.UUID, "owner", true, 2); !errors.Is(err, ErrInputHandedOff) {
		t.Fatal("sent input was recalled", err)
	}
	var state string
	if err := s.DB.QueryRow("SELECT status FROM inbox WHERE uuid=?", d.UUID).Scan(&state); err != nil || state != "sent" {
		t.Fatal(state, err)
	}
}

func TestCancelInputBatchIsAtomicAndCompactBoundaryRemains(t *testing.T) {
	s := openTest(t)
	batch := &core.ImageBatch{ID: "images", Deadline: 1000, MaxImages: 8}
	a := queuedOperationInput(t, s, "a", "author", batch, nil)
	b := queuedOperationInput(t, s, "b", "author", batch, nil)
	c := queuedOperationInput(t, s, "compact", "owner", nil, &core.Command{Name: "compact"})
	if err := s.Mark(b.UUID, "sent"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CancelQueuedInput(context.Background(), "r", a.UUID, "author", false, 2); !errors.Is(err, ErrInputHandedOff) {
		t.Fatal(err)
	}
	var state string
	if err := s.DB.QueryRow("SELECT status FROM inbox WHERE uuid=?", a.UUID).Scan(&state); err != nil || state != "queued" {
		t.Fatal("partially cancelled a batch", state, err)
	}
	if err := s.Queue(b); err != nil {
		t.Fatal(err)
	}
	cancelled, err := s.CancelQueuedInput(context.Background(), "r", b.UUID, "author", false, 3)
	if err != nil || len(cancelled) != 2 {
		t.Fatal(cancelled, err)
	}
	pending, err := s.Pending("r")
	if err != nil || len(pending) != 1 || pending[0].UUID != c.UUID {
		t.Fatal("compact boundary changed", pending, err)
	}
	if _, err = s.CancelQueuedInput(context.Background(), "r", a.UUID, "author", false, 4); !errors.Is(err, ErrInputHandedOff) {
		t.Fatal("cancel replay succeeded", err)
	}
	var n int
	if err = s.DB.QueryRow("SELECT count(*) FROM operator_decisions WHERE kind='input'").Scan(&n); err != nil || n != 2 {
		t.Fatal(n, err)
	}
}

func TestCancelInputKeepsMinimalReplayMarkerAfterRetention(t *testing.T) {
	s := openTest(t)
	d := queuedOperationInput(t, s, "cancelled", "author", nil, nil)
	if _, err := s.CancelQueuedInput(context.Background(), "r", d.UUID, "author", false, 2); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Maintain(context.Background(), 100*dayMS, 1); err != nil {
		t.Fatal(err)
	}
	seen, err := s.Seen("r", d.Message.ID)
	if err != nil || !seen {
		t.Fatal("explicitly cancelled input became replayable", seen, err)
	}
	var body string
	if err = s.DB.QueryRow("SELECT message FROM inbox WHERE uuid=?", d.UUID).Scan(&body); err != nil || body != "{}" {
		t.Fatal("old body not minimized", body, err)
	}
}

func TestInputsAreBoundedAndPayloadCorruptionCannotAuthorizeCancellation(t *testing.T) {
	s := openTest(t)
	d := queuedOperationInput(t, s, "one", "author", nil, nil)
	queuedOperationInput(t, s, "two", "author", nil, nil)
	rows, more, err := s.Inputs(context.Background(), "r", 1)
	if err != nil || len(rows) != 1 || !more {
		t.Fatal(rows, more, err)
	}
	if _, err = s.DB.Exec("UPDATE inbox SET payload='{}' WHERE uuid=?", d.UUID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.CancelQueuedInput(context.Background(), "r", d.UUID, "author", false, 2); !errors.Is(err, ErrInputInvalid) {
		t.Fatal(err)
	}
}
