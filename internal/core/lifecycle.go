package core

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/caoxg/quay/internal/board"
	"github.com/caoxg/quay/internal/profile"
)

// appState asks App CLI for one app's status ("" when App CLI does not know the folder).
func (o *op) appState(path string) (string, bool, error) {
	out, err := board.Output(o.ctx, o.s.T, "arduino-app-cli app list --format json")
	if err != nil {
		return "", false, err
	}
	var l struct {
		Apps []struct {
			ID      string `json:"id"`
			Status  string `json:"status"`
			Default bool   `json:"default"`
		} `json:"apps"`
	}
	if err := json.Unmarshal([]byte(out), &l); err != nil {
		return "", false, fmt.Errorf("app list: %w", err)
	}
	folder := path[strings.LastIndex(path, "/")+1:]
	for _, a := range l.Apps {
		id, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(a.ID, "="))
		if err != nil {
			continue
		}
		if string(id) == "user:"+folder {
			if a.Status == "" {
				a.Status = "stopped"
			}
			return a.Status, a.Default, nil
		}
	}
	return "", false, nil
}

func active(state string) bool {
	return state == "running" || state == "starting" || state == "stopping"
}

// waitUp waits until the app answers its health URL, or App CLI reports it running.
func (o *op) waitUp(path string, p *profile.Profile) error {
	timeout := 180 * time.Second
	if p != nil && p.Health != nil && p.Health.Timeout > 0 {
		timeout = time.Duration(p.Health.Timeout) * time.Second
	}
	deadline := time.Now().Add(timeout)
	for {
		if p != nil && p.Health != nil && p.Health.URL != "" {
			if _, ok := o.probe(p.Health.URL); ok {
				return nil
			}
		} else if st, _, err := o.appState(path); err == nil && st == "running" {
			return nil
		}
		if st, _, err := o.appState(path); err == nil && st == "failed" {
			return errors.New("the app failed to start — see its logs")
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("the app did not come up within %s — see its logs", timeout)
		}
		select {
		case <-o.ctx.Done():
			return o.ctx.Err()
		case <-time.After(1500 * time.Millisecond):
		}
	}
}

func (o *op) startApp(path string, p *profile.Profile) error {
	lw := &lines{o: o}
	err := o.s.T.Exec(o.ctx, "arduino-app-cli app start "+board.Quote(path), nil, lw, lw)
	lw.Flush()
	if err != nil {
		return fmt.Errorf("arduino-app-cli app start failed: %w", err)
	}
	return o.waitUp(path, p)
}

func (o *op) stopApp(path string) error {
	lw := &lines{o: o}
	err := o.s.T.Exec(o.ctx, "arduino-app-cli app stop "+board.Quote(path), nil, lw, lw)
	lw.Flush()
	if err != nil {
		return fmt.Errorf("arduino-app-cli app stop failed: %w", err)
	}
	for i := 0; i < 30; i++ {
		st, _, err := o.appState(path)
		if err == nil && !active(st) {
			return nil
		}
		select {
		case <-o.ctx.Done():
			return o.ctx.Err()
		case <-time.After(time.Second):
		}
	}
	return errors.New("the app did not stop")
}

// guardStop refuses when the app's safety probe says stopping is unsafe.
func (o *op) guardStop(a *App, force bool) error {
	if force || !active(a.Status) {
		return nil
	}
	if why := o.unsafe(a.Profile); why != "" {
		return &UnsafeError{Reason: why}
	}
	return nil
}

// UnsafeError means the app's safety probe says it must not be stopped right now; the caller may
// repeat the operation with force.
type UnsafeError struct{ Reason string }

func (e *UnsafeError) Error() string { return e.Reason }

func (s *Session) lookup(ctx context.Context, key string) (*Status, *App, error) {
	st, err := s.Status(ctx)
	if err != nil {
		return nil, nil, err
	}
	a, err := st.App(key)
	if err != nil {
		return st, nil, err
	}
	return st, a, nil
}

// Start starts an app.
func (s *Session) Start(ctx context.Context, sink Sink, key string) error {
	o := s.op(ctx, sink)
	_, a, err := s.lookup(ctx, key)
	if err != nil {
		return err
	}
	return o.step("start", "Start "+a.Name, func() error { return o.startApp(a.Path, a.Profile) })
}

// Stop stops an app (refused while its safety probe says no, unless force).
func (s *Session) Stop(ctx context.Context, sink Sink, key string, force bool) error {
	o := s.op(ctx, sink)
	_, a, err := s.lookup(ctx, key)
	if err != nil {
		return err
	}
	if err := o.step("safety", "Check it is safe to stop", func() error { return o.guardStop(a, force) }); err != nil {
		return err
	}
	return o.step("stop", "Stop "+a.Name, func() error {
		if !active(a.Status) {
			return errSkip
		}
		return o.stopApp(a.Path)
	})
}

// Restart stops (if running) and starts an app.
func (s *Session) Restart(ctx context.Context, sink Sink, key string, force bool) error {
	o := s.op(ctx, sink)
	_, a, err := s.lookup(ctx, key)
	if err != nil {
		return err
	}
	if err := o.step("safety", "Check it is safe to stop", func() error { return o.guardStop(a, force) }); err != nil {
		return err
	}
	if err := o.step("stop", "Stop "+a.Name, func() error {
		if !active(a.Status) {
			return errSkip
		}
		return o.stopApp(a.Path)
	}); err != nil {
		return err
	}
	return o.step("start", "Start "+a.Name, func() error { return o.startApp(a.Path, a.Profile) })
}

// Autostart makes key the app that runs at boot; key "" or "none" clears it.
func (s *Session) Autostart(ctx context.Context, sink Sink, key string) error {
	o := s.op(ctx, sink)
	if key == "" || key == "none" {
		return o.step("autostart", "Run no app at boot", func() error {
			_, err := o.run("arduino-app-cli properties set default none", nil)
			return err
		})
	}
	_, a, err := s.lookup(ctx, key)
	if err != nil {
		return err
	}
	return o.step("autostart", "Run "+a.Name+" at boot", func() error {
		_, err := o.run("arduino-app-cli properties set default "+board.Quote(a.Path), nil)
		return err
	})
}

// Logs streams an app's Python logs (follow until ctx ends) as log events.
func (s *Session) Logs(ctx context.Context, sink Sink, key string, follow bool, tail int) error {
	o := s.op(ctx, sink)
	_, a, err := s.lookup(ctx, key)
	if err != nil {
		return err
	}
	cmd := "arduino-app-cli app logs " + board.Quote(a.Path)
	if follow {
		cmd += " --follow"
	}
	if tail > 0 {
		cmd += fmt.Sprintf(" --tail %d", tail)
	}
	lw := &lines{o: o}
	err = s.T.Exec(ctx, cmd, nil, lw, lw)
	lw.Flush()
	if errors.Is(err, context.Canceled) || ctx.Err() != nil {
		return nil
	}
	return err
}

// Export writes App CLI's own zip (code + data/, importable in App Lab) to w.
func (s *Session) Export(ctx context.Context, sink Sink, key string, w io.Writer) error {
	o := s.op(ctx, sink)
	_, a, err := s.lookup(ctx, key)
	if err != nil {
		return err
	}
	return o.step("export", "Export "+a.Name+" with its data (App Lab zip)", func() error {
		var errb strings.Builder
		if err := s.T.Exec(ctx, "arduino-app-cli app export "+board.Quote(a.Path)+" - --include-data", nil, w, &errb); err != nil {
			return fmt.Errorf("export failed: %s", orErr(lastLine(errb.String()), err))
		}
		return nil
	})
}
