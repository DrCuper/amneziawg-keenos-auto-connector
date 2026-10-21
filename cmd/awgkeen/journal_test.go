package main

import (
	"testing"
	"time"
)

func TestParseSince(t *testing.T) {
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.Local)
	for in, want := range map[string]time.Time{
		"24h":        now.Add(-24 * time.Hour),
		"90m":        now.Add(-90 * time.Minute),
		"7d":         now.Add(-7 * 24 * time.Hour),
		"2026-10-20": time.Date(2026, 10, 20, 0, 0, 0, 0, time.Local),
	} {
		got, err := parseSince(in, now)
		if err != nil || !got.Equal(want) {
			t.Errorf("%s: %v %v", in, got, err)
		}
	}
	if _, err := parseSince("yesterday", now); err == nil {
		t.Error("expected error")
	}
}
