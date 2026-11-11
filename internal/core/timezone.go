package core

import (
	"fmt"
	"os"
	"strings"
	"time"
)

// LocalZoneInfo resolves both the location used for formatting and the name
// shown in the input envelope and history results. TZ wins when it names a
// loadable zone; time.Local (including Windows' full zone rules under the name
// "Local"), /etc/localtime and /etc/timezone are tried next. The final fallback
// keeps time.Local for formatting, including its historical DST rules, while
// the display name is the current numeric offset.
func LocalZoneInfo() (*time.Location, string) {
	if zone := os.Getenv("TZ"); zone != "" {
		if loc, err := time.LoadLocation(zone); err == nil {
			return loc, zone
		}
	}
	if name := time.Local.String(); name != "Local" && name != "" {
		return time.Local, name
	}
	if link, err := os.Readlink("/etc/localtime"); err == nil {
		if _, zone, ok := strings.Cut(link, "zoneinfo/"); ok {
			if loc, err := time.LoadLocation(zone); err == nil {
				return loc, zone
			}
		}
	}
	if data, err := os.ReadFile("/etc/timezone"); err == nil && len(data) < 200 {
		if zone := strings.TrimSpace(string(data)); zone != "" {
			if loc, err := time.LoadLocation(zone); err == nil {
				return loc, zone
			}
		}
	}
	return time.Local, offsetZoneName(time.Now())
}

// LocalZone returns the location used for formatting. It may be time.Local
// without an IANA name; callers should use TimezoneName for display.
func LocalZone() *time.Location {
	loc, _ := LocalZoneInfo()
	return loc
}

// TimezoneName is the name reported in envelopes and history results.
func TimezoneName() string {
	_, name := LocalZoneInfo()
	return name
}

// LocalTime formats a Unix-millisecond timestamp as RFC3339 with a numeric
// offset, for example 2026-11-11T15:05:00+08:00. Non-positive times stay empty.
func LocalTime(milliseconds int64) string {
	if milliseconds <= 0 {
		return ""
	}
	loc, _ := LocalZoneInfo()
	return LocalTimeIn(loc, milliseconds)
}

// LocalTimeIn formats with an already resolved location. Batch callers should
// resolve LocalZoneInfo once and reuse it for every message.
func LocalTimeIn(loc *time.Location, milliseconds int64) string {
	if milliseconds <= 0 {
		return ""
	}
	if loc == nil {
		loc = time.Local
	}
	return time.UnixMilli(milliseconds).In(loc).Format(time.RFC3339)
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
