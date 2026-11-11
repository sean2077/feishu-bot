package core

import (
	"context"
	"errors"
)

// ErrCardNotEntity reports that a message is not backed by a CardKit card
// entity (for example an older message card), so per-element updates are not
// available.
var ErrCardNotEntity = errors.New("card is not a CardKit entity")

type CardElementUpdate struct {
	ElementID string
	Element   Args
}

// CardElementUpdater is implemented by transports that can replace one CardKit
// element without rewriting the whole card.
type CardElementUpdater interface {
	UpdateElements(context.Context, string, []CardElementUpdate) error
}
