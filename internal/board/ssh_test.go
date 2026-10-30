package board

import (
	"context"
	"errors"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"
)

// freePort returns a local port nothing listens on (for a moment).
func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func TestDialBoardReportsEveryAddressOfBothRounds(t *testing.T) {
	defer func(d time.Duration) { redialDelay = d }(redialDelay)
	redialDelay = 50 * time.Millisecond
	port := freePort(t)
	tg := Target{Host: "localhost", Port: port, LastIP: "127.0.0.1"}
	_, err := dialBoard(context.Background(), tg, net.JoinHostPort(tg.Host, strconv.Itoa(port)), 2)
	var u *UnreachableError
	if !errors.As(err, &u) {
		t.Fatalf("got %v, want an UnreachableError", err)
	}
	if len(u.Rounds) != 2 || len(u.Rounds[0]) != 2 {
		t.Fatalf("rounds %+v, want two rounds of two addresses (the name, then the last known IP)", u.Rounds)
	}
	msg := err.Error()
	for _, want := range []string{"cannot reach localhost:" + strconv.Itoa(port) + " (twice, 50ms apart): localhost:", "; 127.0.0.1:" + strconv.Itoa(port) + ": connect: connection refused"} {
		if !strings.Contains(msg, want) {
			t.Errorf("%q lacks %q", msg, want)
		}
	}
}

func TestDialBoardSecondRoundConnects(t *testing.T) {
	defer func(d time.Duration) { redialDelay = d }(redialDelay)
	redialDelay = 300 * time.Millisecond
	port := freePort(t)
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	up := make(chan net.Listener, 1)
	go func() { // the board comes back between the two rounds
		time.Sleep(100 * time.Millisecond)
		l, err := net.Listen("tcp", addr)
		if err != nil {
			up <- nil
			return
		}
		up <- l
	}()
	conn, err := dialBoard(context.Background(), Target{Host: "127.0.0.1", Port: port}, addr, 2)
	l := <-up
	if l == nil {
		t.Skip("the port was taken in between")
	}
	defer l.Close()
	if err != nil {
		t.Fatalf("second round: %v", err)
	}
	conn.Close()
}

func TestDialBoardStopsWhenCancelled(t *testing.T) {
	defer func(d time.Duration) { redialDelay = d }(redialDelay)
	redialDelay = time.Hour
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(100*time.Millisecond, cancel)
	port := freePort(t)
	start := time.Now()
	_, err := dialBoard(ctx, Target{Host: "127.0.0.1", Port: port}, net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), 2)
	if err == nil || time.Since(start) > 5*time.Second {
		t.Fatalf("err %v after %s", err, time.Since(start))
	}
}
