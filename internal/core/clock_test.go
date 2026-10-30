package core

import (
	"testing"
	"time"
)

func TestParseClock(t *testing.T) {
	now := time.Date(2026, 10, 26, 15, 30, 0, 0, time.UTC)
	c, err := parseClock("1779967450\nNTP=yes\nNTPSynchronized=no\n", now)
	if err != nil {
		t.Fatal(err)
	}
	if !c.NTP || c.Synced || c.Skew > -MaxClockSkew || c.Time.Format("2006-01-02") != "2026-05-28" {
		t.Fatalf("got %+v", c)
	}
	if got := aboutDuration(c.Skew); got != "151 days" {
		t.Errorf("aboutDuration = %q", got)
	}
	// No timedatectl (a container): only the time.
	c, err = parseClock("1793028600\n", now)
	if err != nil || c.NTP || c.Synced || c.Skew.Abs() >= MaxClockSkew {
		t.Fatalf("got %+v, %v", c, err)
	}
	if _, err := parseClock("date: not found\n", now); err == nil {
		t.Error("want an error for output that is not a time")
	}
}
