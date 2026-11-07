//go:build windows

package cli

import (
	"context"
	"errors"
	"testing"
)

func TestTaskLookupHRESULTOnlyConfirmsTaskAbsence(t *testing.T) {
	for _, tt := range []struct {
		name    string
		hr      uint32
		present bool
		failed  bool
	}{
		{"found", 0, true, false},
		{"missing-task", 0x80070002, false, false},
		{"denied", 0x80070005, false, true},
		{"scheduler-unavailable", 0x800706ba, false, true},
		{"invalid-task", 0x8004130d, false, true},
		{"missing-folder", 0x80070003, false, true},
		{"unexpected-success", 1, false, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			present, err := taskLookupStatus(tt.hr)
			if present != tt.present || (err != nil) != tt.failed {
				t.Fatal(present, err)
			}
		})
	}
}

func TestWindowsCancelledTaskSnapshotFailsClosed(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	manager := windowsManager{}
	if present, err := manager.Present(ctx, "feishu-bot-test-cancelled"); present || !errors.Is(err, context.Canceled) {
		t.Fatal("cancelled query was treated as task absence", present, err)
	}
	if _, err := manager.Snapshot(ctx, "feishu-bot-test-cancelled"); !errors.Is(err, context.Canceled) {
		t.Fatal("cancelled snapshot was accepted", err)
	}
}
