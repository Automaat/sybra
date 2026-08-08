package db

import "time"

// Timestamps cross the dialect seam as microseconds since the Unix epoch, and
// booleans as 0/1 integers. Both engines agree on integer round-tripping;
// they do not agree on timestamp text formats, timezone handling, or whether
// a boolean column scans into a Go bool, so stores never rely on either.

// TimeValue converts a time to its stored form. The zero time stores as 0 so an absent timestamp reads back as the zero time on both engines.
func TimeValue(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UTC().UnixMicro()
}

// TimeFrom converts a stored timestamp back to UTC. 0 maps to the zero time.
func TimeFrom(us int64) time.Time {
	if us == 0 {
		return time.Time{}
	}
	return time.UnixMicro(us).UTC()
}

// BoolValue converts a bool to its stored form.
func BoolValue(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

// BoolFrom converts a stored boolean back to a Go bool.
func BoolFrom(v int64) bool { return v != 0 }
