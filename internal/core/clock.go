package core

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// MaxClockSkew is how far the board's clock may be from this computer's before Quay sets it. The
// board has no battery-backed clock: until NTP runs it starts from its image's build date or its
// last shutdown, and then apt refuses Release files that are "not valid yet" and TLS refuses
// certificates from the future.
const MaxClockSkew = 10 * time.Minute

// clockWait is how long Quay gives the board's own NTP to set its clock.
var clockWait = 30 * time.Second

// boardClock is what the board says about its clock.
type boardClock struct {
	Skew   time.Duration // board minus this computer
	Time   time.Time     // the board's time
	NTP    bool          // the board runs NTP (systemd-timesyncd)
	Synced bool          // and it has set the clock
}

// parseClock reads `date +%s` followed by `timedatectl show` lines (NTP=yes, NTPSynchronized=no).
func parseClock(out string, now time.Time) (boardClock, error) {
	var c boardClock
	lines := strings.Split(strings.TrimSpace(out), "\n")
	secs, err := strconv.ParseInt(strings.TrimSpace(lines[0]), 10, 64)
	if err != nil {
		return c, fmt.Errorf("board clock: %q", lines[0])
	}
	c.Time = time.Unix(secs, 0).UTC()
	c.Skew = c.Time.Sub(now).Round(time.Second)
	for _, l := range lines[1:] {
		k, v, _ := strings.Cut(strings.TrimSpace(l), "=")
		switch k {
		case "NTP":
			c.NTP = v == "yes"
		case "NTPSynchronized":
			c.Synced = v == "yes"
		}
	}
	return c, nil
}

func (o *op) boardClock() (boardClock, error) {
	out, err := o.quiet("date +%s; timedatectl show -p NTP -p NTPSynchronized 2>/dev/null || true")
	if err != nil {
		return boardClock{}, err
	}
	return parseClock(out, time.Now())
}

// clock makes sure the board's clock is about right before the board downloads anything (apt,
// pip, a mirror test). When it is off, Quay waits a little for the board's NTP, then sets it from
// this computer's clock (sudo). A board whose NTP has synced is trusted over this computer.
func (o *op) clock() error {
	c, err := o.boardClock()
	if err != nil || c.Synced || c.Skew.Abs() < MaxClockSkew {
		return nil // a failed look must not block the operation; its own errors will tell
	}
	return o.step("clock", "Set the board's clock", func() error {
		o.warn(fmt.Sprintf("The board's clock says %s, %s %s this computer", c.Time.Format("2006-01-02 15:04 UTC"), aboutDuration(c.Skew), map[bool]string{true: "ahead of", false: "behind"}[c.Skew > 0]))
		if c.NTP {
			o.log("Waiting for the board's NTP to set it")
			for end := time.Now().Add(clockWait); time.Now().Before(end); {
				select {
				case <-o.ctx.Done():
					return o.ctx.Err()
				case <-time.After(3 * time.Second):
				}
				if c, err = o.boardClock(); err == nil && (c.Synced || c.Skew.Abs() < MaxClockSkew) {
					return nil
				}
			}
		}
		o.log("Setting it from this computer's clock")
		// The time is taken on the board when the script runs, plus how long ago Quay wrote it.
		return o.sudo("Set the board's clock", `date -u -s "@$(( $EPOCH + $(date +%s) - $BOARD ))" >/dev/null`, map[string]string{
			"EPOCH": strconv.FormatInt(time.Now().Unix(), 10),
			"BOARD": strconv.FormatInt(c.Time.Unix(), 10),
		})
	})
}

// aboutDuration says roughly how long d is: "151 days", "5 hours", "12 minutes".
func aboutDuration(d time.Duration) string {
	d = d.Abs()
	switch {
	case d >= 48*time.Hour:
		return fmt.Sprintf("%d days", int(d.Hours()/24))
	case d >= 2*time.Hour:
		return fmt.Sprintf("%d hours", int(d.Hours()))
	default:
		return fmt.Sprintf("%d minutes", int(d.Minutes()))
	}
}
