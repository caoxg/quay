package core

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode"
)

// ArduinoPasswd is the board image's helper that sets the password of the user with uid 1000 (arduino)
// from a line on its standard input. The image lets that user run it through sudo without a password
// (/etc/sudoers.d/10-arduino-passwd, from the arduino-app-cli package): App Lab's first setup sets the
// password with it, and anyone with a USB cable (adb runs as arduino) can set a new one.
const ArduinoPasswd = "/usr/local/bin/arduino-passwd"

// ValidBoardPassword checks a new password for the board's user. arduino-passwd reads it with a plain
// `read`, which drops a backslash and spaces at either end, so those are refused rather than changed.
func ValidBoardPassword(pw string) error {
	switch {
	case pw == "":
		return errors.New("the password is empty")
	case len(pw) > 128:
		return errors.New("the password is longer than 128 characters")
	case strings.ContainsRune(pw, '\\'):
		return errors.New(`the password cannot contain a backslash (\): the board's arduino-passwd would drop it`)
	case strings.TrimSpace(pw) != pw:
		return errors.New("the password cannot start or end with a space")
	}
	for _, r := range pw {
		if unicode.IsControl(r) {
			return errors.New("the password cannot contain control characters")
		}
	}
	return nil
}

// SetPassword sets the board user's password with the image's arduino-passwd, which needs no current
// password, then checks that sudo takes the new one. Over USB it works on a board whose password
// nobody knows, and on a fresh board before App Lab's first setup.
func (s *Session) SetPassword(ctx context.Context, sink Sink, pw string) error {
	if err := ValidBoardPassword(pw); err != nil {
		return err
	}
	o := s.op(ctx, sink)
	if err := o.step("password", "Set the board's password", func() error {
		var errb bytes.Buffer
		err := o.s.T.Exec(o.ctx, "sudo -n "+ArduinoPasswd, strings.NewReader(pw+"\n"), nil, &errb)
		if err == nil {
			return nil
		}
		if o.s.T.Exec(o.ctx, "test -x "+ArduinoPasswd, nil, nil, nil) != nil {
			return fmt.Errorf("this board's image has no %s; change the password on the board with passwd", ArduinoPasswd)
		}
		if strings.Contains(errb.String(), "password is required") {
			return fmt.Errorf("this board does not let its user run %s without a password; change the password on the board with passwd", ArduinoPasswd)
		}
		return fmt.Errorf("setting the password failed: %s", orErr(lastLine(errb.String()), err))
	}); err != nil {
		return err
	}
	return o.step("verify", "Check that sudo takes the new password", func() error {
		var errb bytes.Buffer
		if err := o.s.T.Exec(o.ctx, "sudo -k; sudo -S -p '' true", strings.NewReader(pw+"\n"), nil, &errb); err != nil {
			return fmt.Errorf("sudo does not take the new password: %s", orErr(lastLine(errb.String()), err))
		}
		o.s.mu.Lock()
		o.s.sudoPass = pw
		o.s.mu.Unlock()
		return nil
	})
}
