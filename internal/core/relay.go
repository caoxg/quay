package core

import (
	"context"
	"errors"
	"fmt"
	"net"
	"time"

	"github.com/caoxg/quay/internal/board"
	"github.com/caoxg/quay/internal/relay"
)

// StartRelay forwards relay.Port on the board's loopback back to p on this computer, over this
// session's own connection (SSH remote forwarding, or adb reverse over USB). With the board's apt
// hook installed (Relay), apt then downloads through this computer. Calling it again while it runs
// does nothing; Close stops it.
func (s *Session) StartRelay(p *relay.Proxy) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.relayStop != nil {
		return nil
	}
	switch t := s.T.(type) {
	case *board.SSH:
		ln, err := t.Client().Listen("tcp", fmt.Sprintf("127.0.0.1:%d", relay.Port))
		if err != nil {
			return fmt.Errorf("cannot open port %d on the board (another Quay may be relaying already): %w", relay.Port, err)
		}
		s.relayGen++
		gen := s.relayGen
		s.relayStop = func() { _ = ln.Close() }
		go func() { // SSH channels have no deadlines
			_ = p.ServeBridged(ln)
			s.relayEnded(gen) // the forward went (with the connection): not relaying any more
		}()
	case *board.ADB:
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := t.Reverse(ctx, relay.Port, ln.Addr().(*net.TCPAddr).Port); err != nil {
			ln.Close()
			return fmt.Errorf("adb reverse: %w", err)
		}
		go func() { _ = p.Serve(ln) }()
		s.relayStop = func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = t.RemoveReverse(ctx, relay.Port)
			_ = ln.Close()
		}
	default:
		return errors.New("the relay needs an SSH or USB connection")
	}
	return nil
}

// relayEnded forgets relay number gen of StartRelay, if it is still the current one.
func (s *Session) relayEnded(gen int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.relayGen == gen {
		s.relayStop = nil
	}
}

// StopRelay stops StartRelay's forwarding.
func (s *Session) StopRelay() {
	s.mu.Lock()
	stop := s.relayStop
	s.relayStop = nil
	s.mu.Unlock()
	if stop != nil {
		stop()
	}
}

// RelayActive reports whether this session is relaying.
func (s *Session) RelayActive() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.relayStop != nil
}
