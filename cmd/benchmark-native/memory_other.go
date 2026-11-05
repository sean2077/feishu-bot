//go:build !linux && !windows

package main

import "errors"

func platformMemoryRows() ([]processMemory, int, string, error) {
	return nil, 0, "unsupported platform", errors.New("process-tree memory collector unavailable")
}
