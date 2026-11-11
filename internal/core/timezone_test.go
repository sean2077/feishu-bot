package core

import (
	"testing"
	"time"
)

func TestLocalTimeUsesConfiguredZone(t *testing.T) {
	t.Setenv("TZ", "Asia/Singapore")
	instant := time.Date(2026, 11, 11, 7, 5, 0, 0, time.UTC)
	if got := LocalTime(instant.UnixMilli()); got != "2026-11-11T15:05:00+08:00" {
		t.Fatalf("localTime = %q", got)
	}
	if got := TimezoneName(); got != "Asia/Singapore" {
		t.Fatalf("timezone = %q", got)
	}
	if got := LocalTime(0); got != "" {
		t.Fatalf("zero time localTime = %q", got)
	}
	if got := LocalTime(-1); got != "" {
		t.Fatalf("negative time localTime = %q", got)
	}
}

func TestOffsetZoneNamesLocalOffsetInsteadOfUTC(t *testing.T) {
	for _, tc := range []struct {
		offset int
		want   string
	}{
		{0, "UTC"},
		{8 * 3600, "UTC+08:00"},
		{-5 * 3600, "UTC-05:00"},
		{5*3600 + 1800, "UTC+05:30"},
		{-(3*3600 + 1800), "UTC-03:30"},
	} {
		now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.FixedZone("Local", tc.offset))
		if got := offsetZoneName(now); got != tc.want {
			t.Fatalf("offset %d: %q want %q", tc.offset, got, tc.want)
		}
	}
}
