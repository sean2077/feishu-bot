//go:build !windows && !linux && !darwin

package process

// StartTime is unavailable on this platform. Callers fall back to Alive and
// treat a stored zero start time as unverifiable.
func StartTime(pid int) (int64, bool) { return 0, false }
