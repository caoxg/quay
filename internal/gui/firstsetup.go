package gui

import (
	"context"
	"strings"
	"time"

	"github.com/caoxg/quay/internal/board"
	"github.com/caoxg/quay/internal/config"
	"github.com/caoxg/quay/internal/core"
)

// FirstSetupResult is what FirstSetup did.
type FirstSetupResult struct {
	Board string `json:"board"` // the saved board's name afterwards (its new hostname)
	Host  string `json:"host"`  // NAME.local
	IP    string `json:"ip"`    // its Wi-Fi address, when it got onto Wi-Fi
	SSH   bool   `json:"ssh"`   // the saved board now goes over SSH instead of USB
	Note  string `json:"note"`  // why it does not, if it does not
}

// FirstSetup does over USB what App Lab's first setup does — the password, SSH, the name, Wi-Fi —
// with the commands the board image allows without a password, then switches the saved board from
// USB to SSH when this computer reaches it at NAME.local. uploadKey also puts this computer's SSH key
// on the board (creating one, as ssh-keygen would, when there is none), so later logins ask nothing.
func (s *Service) FirstSetup(boardName, name, pw, ssid, psk string, uploadKey bool) string {
	return s.run("firstsetup", boardName, "", func(ctx context.Context, sess *core.Session, sink core.Sink) (any, error) {
		name = strings.ToLower(strings.TrimSpace(name))
		if err := core.ValidHostname(name); err != nil {
			return nil, err
		}
		if err := core.ValidBoardPassword(pw); err != nil {
			return nil, err
		}
		if ssid != "" {
			if err := core.ValidSSID(ssid); err != nil {
				return nil, err
			}
			if psk != "" {
				if err := core.ValidPSK(psk); err != nil {
					return nil, err
				}
			}
		}
		if err := sess.SetPassword(ctx, sink, pw); err != nil {
			return nil, err
		}
		keys, err := sess.EnableSSH(ctx, sink)
		if err != nil {
			return nil, err
		}
		if err := sess.Rename(ctx, sink, name); err != nil {
			return nil, err
		}
		if uploadKey {
			if _, line, err := localOrNewKey(sess.Target.KeyFile, true); err != nil {
				sink(core.Event{Type: "warn", Text: "No SSH key to upload: " + err.Error(), Time: time.Now().UnixMilli()})
			} else if _, err := sess.UploadKey(ctx, sink, line); err != nil {
				return nil, err
			}
		}
		if ssid != "" {
			if err := sess.WifiAdd(ctx, sink, ssid, psk, false, false); err != nil {
				return nil, err
			}
		}
		res := FirstSetupResult{Host: name + ".local", Board: boardName}
		if st, err := sess.Status(ctx); err == nil {
			res.Board = s.learn(boardName, sess, st)
		}
		if err := board.PinHostKeys(config.KnownHostsFile(), res.Host, 22, keys); err != nil {
			return nil, err
		}
		ip, err := sess.WaitOnline(ctx, sink, 60)
		if err != nil {
			res.Note = err.Error()
			return res, nil
		}
		res.IP = ip

		// Over SSH from here on, if this computer reaches the board (same network).
		t := board.Target{Kind: board.KindSSH, Host: res.Host, User: "arduino", LastIP: ip}
		sink(core.Event{Type: "step", Step: "sshswitch", State: "run", Text: "Connect over SSH to " + res.Host, Time: time.Now().UnixMilli()})
		var c *core.Session
		for i := 0; i < 5; i++ { // NAME.local can take a few seconds to appear after the rename
			cctx, cancel := context.WithTimeout(ctx, 12*time.Second)
			c, err = core.Connect(cctx, t, setupPrompter(pw), config.KnownHostsFile())
			cancel()
			if err == nil || ctx.Err() != nil {
				break
			}
			time.Sleep(3 * time.Second)
		}
		if err != nil {
			res.Note = err.Error()
			sink(core.Event{Type: "step", Step: "sshswitch", State: "fail", Text: "No SSH connection to " + res.Host + " from this computer: " + err.Error(), Time: time.Now().UnixMilli()})
			return res, nil
		}
		c.Close()
		if err := s.SaveBoard(t, res.Board, false); err != nil {
			res.Note = err.Error()
			sink(core.Event{Type: "step", Step: "sshswitch", State: "fail", Text: "Could not save the SSH connection to " + res.Board + ": " + err.Error(), Time: time.Now().UnixMilli()})
			return res, nil
		}
		res.SSH = true
		sink(core.Event{Type: "step", Step: "sshswitch", State: "ok", Text: "Quay now reaches " + res.Board + " over SSH at " + res.Host, Time: time.Now().UnixMilli()})
		return res, nil
	})
}

// setupPrompter logs in with the password just set, and trusts only the host keys already pinned.
type setupPrompter string

func (p setupPrompter) Secret(context.Context, string, string) (string, bool, error) {
	return string(p), true, nil
}
func (setupPrompter) TrustHostKey(context.Context, string, string, string) (bool, error) {
	return false, nil
}
