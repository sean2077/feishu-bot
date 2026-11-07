//go:build !linux && !windows && !darwin

package process

import (
	"context"
	"errors"
)

func residentRows(context.Context) ([]residentProcess, int, error) {
	return nil, 0, errors.New("process tree RSS observation unsupported on this platform")
}
