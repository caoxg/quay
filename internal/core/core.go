// Package core implements Quay's operations on a board. It reports progress as Events so the
// CLI and the GUI render the same thing; it never prints.
package core

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/caoxg/quay/internal/applog"
	"github.com/caoxg/quay/internal/board"
	"github.com/caoxg/quay/internal/remote"
)

// Event is one progress report.
type Event struct {
	Op    string `json:"op,omitempty"`    // set by the caller (GUI operation id)
	Type  string `json:"type"`            // step | log | warn | info | progress | debug (Quay's log only)
	Step  string `json:"step,omitempty"`  // stable key, e.g. "upload"
	State string `json:"state,omitempty"` // run | ok | fail | skip (steps)
	Text  string `json:"text,omitempty"`
	Data  any    `json:"data,omitempty"`
	Time  int64  `json:"time"`
}

// Sink receives events. It must be safe to call from several goroutines.
type Sink func(Event)

// Record writes e to Quay's log (applog) under tag, which names the operation. Progress ticks stay
// out, and so does the data the screens draw from.
func Record(tag string, e Event) {
	switch e.Type {
	case "progress", "noop", "":
	case "step":
		applog.Default.Printf("%s step %s %s: %s", tag, e.Step, e.State, e.Text)
	default:
		applog.Default.Printf("%s %s: %s", tag, e.Type, e.Text)
	}
}

// Session is a connection to one board.
type Session struct {
	T      board.Transport
	Target board.Target
	Prompt board.Prompter

	mu        sync.Mutex
	sudoPass  string
	relayStop func() // set while StartRelay forwards the board's relay port
	relayGen  int    // counts StartRelay's forwards, so one that ends knows whether it is current
}

// Connect opens a session.
func Connect(ctx context.Context, t board.Target, p board.Prompter, knownHosts string) (*Session, error) {
	tr, err := board.Open(ctx, t, p, knownHosts)
	if err != nil {
		return nil, err
	}
	return &Session{T: tr, Target: t, Prompt: p}, nil
}

func (s *Session) Close() error {
	s.StopRelay()
	return s.T.Close()
}

// op is a running operation: context + sink + helpers.
type op struct {
	ctx  context.Context
	s    *Session
	sink Sink
}

func (s *Session) op(ctx context.Context, sink Sink) *op {
	if sink == nil {
		sink = func(Event) {}
	}
	return &op{ctx: ctx, s: s, sink: sink}
}

func (o *op) emit(e Event) {
	e.Time = time.Now().UnixMilli()
	o.sink(e)
}

func (o *op) step(key, text string, fn func() error) error {
	o.emit(Event{Type: "step", Step: key, State: "run", Text: text})
	err := fn()
	switch {
	case errors.Is(err, errSkip):
		o.emit(Event{Type: "step", Step: key, State: "skip", Text: text})
		return nil
	case err != nil:
		o.emit(Event{Type: "step", Step: key, State: "fail", Text: err.Error()})
		return err
	}
	o.emit(Event{Type: "step", Step: key, State: "ok", Text: text})
	return nil
}

var errSkip = errors.New("skip")

func (o *op) log(line string)  { o.emit(Event{Type: "log", Text: line}) }
func (o *op) warn(text string) { o.emit(Event{Type: "warn", Text: text}) }
func (o *op) info(text string, data any) {
	o.emit(Event{Type: "info", Text: text, Data: data})
}

// lines is an io.Writer that emits each line as a log event.
type lines struct {
	o   *op
	buf bytes.Buffer
	mu  sync.Mutex
	// filter can swallow marker lines (return false)
	filter func(string) bool
}

func (w *lines) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf.Write(p)
	for {
		b := w.buf.Bytes()
		i := bytes.IndexByte(b, '\n')
		if i < 0 {
			break
		}
		line := strings.TrimRight(string(b[:i]), "\r")
		w.buf.Next(i + 1)
		if w.filter == nil || w.filter(line) {
			w.o.log(line)
		}
	}
	return len(p), nil
}

func (w *lines) Flush() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if rest := strings.TrimSpace(w.buf.String()); rest != "" && (w.filter == nil || w.filter(rest)) {
		w.o.log(rest)
	}
	w.buf.Reset()
}

// run executes a user script, streaming its output as log lines, and returns all stdout.
func (o *op) run(script string, filter func(string) bool) (string, error) {
	var out bytes.Buffer
	lw := &lines{o: o, filter: filter}
	var errb bytes.Buffer
	err := o.s.T.Run(o.ctx, script, io.MultiWriter(&out, lw), io.MultiWriter(&errb, lw))
	lw.Flush()
	if err != nil {
		msg := lastLine(errb.String())
		if msg == "" {
			msg = lastLine(out.String())
		}
		if msg != "" {
			return out.String(), &runError{msg: msg, err: err}
		}
		return out.String(), err
	}
	return out.String(), nil
}

// runError says what a failed script printed last, and keeps why it failed (a dropped connection,
// an exit status) for errors.Is/As and dropped.
type runError struct {
	msg string
	err error
}

func (e *runError) Error() string { return e.msg }
func (e *runError) Unwrap() error { return e.err }

// quiet runs a user script without streaming. Its callers often take a failure as an answer, so a
// failure also goes out as a "debug" event, for Quay's log only: a script that broke looks the same.
func (o *op) quiet(script string) (string, error) {
	out, err := board.Output(o.ctx, o.s.T, script)
	if err != nil && o.ctx.Err() == nil {
		_, file, line, _ := runtime.Caller(1)
		o.emit(Event{Type: "debug", Text: fmt.Sprintf("a background script (%s:%d) failed: %v", filepath.Base(file), line, err)})
	}
	return out, err
}

// lastLine is the last line of s that says something: apt's machine-readable status lines
// (dlstatus:, pmstatus: from APT::Status-Fd) are progress, not what went wrong.
func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		l := strings.TrimSpace(lines[i])
		if l != "" && !strings.HasPrefix(l, "dlstatus:") && !strings.HasPrefix(l, "pmstatus:") {
			return l
		}
	}
	return ""
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

var folderRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,100}$`)

// ValidFolder reports whether name is a safe folder name under ~/ArduinoApps.
func ValidFolder(name string) error {
	if !folderRe.MatchString(name) || name == "." || name == ".." {
		return fmt.Errorf("invalid app folder name %q (letters, digits, . _ - only)", name)
	}
	return nil
}

// sudo runs a root script. It tries passwordless sudo, then the SSH login password, then asks.
func (o *op) sudo(title, body string, vars map[string]string) error {
	return o.sudoWith(title, body, vars, nil)
}

// sudoFiltered is sudo for a command whose output lines pass through filter (which may swallow
// them, e.g. progress markers).
func (o *op) sudoFiltered(title, body string, filter func(string) bool) error {
	return o.sudoWith(title, body, nil, filter)
}

func (o *op) sudoWith(title, body string, vars map[string]string, filter func(string) bool) error {
	path := "/tmp/quay-root-" + randHex(6) + ".sh"
	if err := o.s.T.Upload(o.ctx, strings.NewReader(remote.RootScript(body, vars)), path); err != nil {
		return err
	}
	defer func() { _ = o.s.T.Exec(context.Background(), "rm -f "+board.Quote(path), nil, nil, nil) }()

	lw := &lines{o: o, filter: filter}
	defer lw.Flush()
	if o.s.T.Exec(o.ctx, "sudo -n true", nil, nil, nil) == nil {
		var errb bytes.Buffer
		if err := o.s.T.Exec(o.ctx, "sudo -n bash "+board.Quote(path), nil, lw, io.MultiWriter(lw, &errb)); err != nil {
			return fmt.Errorf("%s failed: %s", title, orErr(lastLine(errb.String()), err))
		}
		return nil
	}

	o.s.mu.Lock()
	pass := o.s.sudoPass
	o.s.mu.Unlock()
	if pass == "" {
		if sp, ok := o.s.T.(interface{ LoginPassword() string }); ok {
			pass = sp.LoginPassword()
		}
	}
	for attempt := 0; attempt < 3; attempt++ {
		if pass == "" {
			if o.s.Prompt == nil {
				return errors.New("administrator (sudo) password required")
			}
			v, ok, err := o.s.Prompt.Secret(o.ctx, "Administrator password", fmt.Sprintf("%s needs sudo. Password for %s:", title, userOf(o.s)))
			if err != nil {
				return err
			}
			if !ok {
				return context.Canceled
			}
			pass = v
		}
		var errb bytes.Buffer
		err := o.s.T.Exec(o.ctx, "sudo -S -p '' bash "+board.Quote(path), strings.NewReader(pass+"\n"), lw, io.MultiWriter(lw, &errb))
		if err == nil {
			o.s.mu.Lock()
			o.s.sudoPass = pass
			o.s.mu.Unlock()
			return nil
		}
		e := errb.String()
		if strings.Contains(e, "incorrect password") || strings.Contains(e, "Sorry, try again") || strings.Contains(e, "no password was provided") {
			o.warn("sudo: wrong password")
			pass = ""
			continue
		}
		return fmt.Errorf("%s failed: %s", title, orErr(lastLine(e), err))
	}
	return errors.New("sudo: too many wrong passwords")
}

func userOf(s *Session) string {
	if s.Target.Kind == board.KindSSH && s.Target.User != "" {
		return s.Target.User + "@" + s.Target.Host
	}
	return "the board's user"
}

func orErr(msg string, err error) string {
	if msg != "" {
		return msg
	}
	return err.Error()
}

// scanLines splits s into trimmed non-empty lines.
func scanLines(s string) []string {
	var out []string
	sc := bufio.NewScanner(strings.NewReader(s))
	for sc.Scan() {
		if l := strings.TrimSpace(sc.Text()); l != "" {
			out = append(out, l)
		}
	}
	return out
}
