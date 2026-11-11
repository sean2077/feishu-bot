package core

import (
	"fmt"
	"os"
	"strings"
	"time"
)

// LocalZone resolves the human-facing time zone used by both the native input
// envelope and history results. TZ wins when it names a loadable zone; other
// platforms expose the local zone through time.Local, /etc/localtime or
// /etc/timezone. A fixed offset is the final fallback, so hosts without an IANA
// name still avoid reporting UTC incorrectly.
func LocalZone() *time.Location {
	if zone := os.Getenv("TZ"); zone != "" {
		if loc, err := time.LoadLocation(zone); err == nil {
			return loc
		}
	}
	if name := time.Local.String(); name != "Local" && name != "" {
		return time.Local
	}
	if link, err := os.Readlink("/etc/localtime"); err == nil {
		if _, zone, ok := strings.Cut(link, "zoneinfo/"); ok {
			if loc, err := time.LoadLocation(zone); err == nil {
				return loc
			}
		}
	}
	if data, err := os.ReadFile("/etc/timezone"); err == nil && len(data) < 200 {
		if zone := strings.TrimSpace(string(data)); zone != "" {
			if loc, err := time.LoadLocation(zone); err == nil {
				return loc
			}
		}
	}
	now := time.Now()
	_, offset := now.Zone()
	return time.FixedZone(offsetZoneName(now), offset)
}

// TimezoneName is the name reported in envelopes and history results. It
// matches the zone used by LocalTime.
func TimezoneName() string {
	return LocalZone().String()
}

// LocalTime formats a Unix-millisecond timestamp as RFC3339 with a numeric
// offset, for example 2026-11-11T15:05:00+08:00. Non-positive times stay empty.
func LocalTime(milliseconds int64) string {
	if milliseconds <= 0 {
		return ""
	}
	return time.UnixMilli(milliseconds).In(LocalZone()).Format(time.RFC3339)
}

// offsetZoneName names the current local offset as UTC±hh:mm. The value is only
// shown to the model, so it avoids the inverted-sign Etc/GMT form.
func offsetZoneName(now time.Time) string {
	_, offset := now.Zone()
	if offset == 0 {
		return "UTC"
	}
	sign := '+'
	if offset < 0 {
		sign = '-'
		offset = -offset
	}
	return fmt.Sprintf("UTC%c%02d:%02d", sign, offset/3600, offset%3600/60)
}
