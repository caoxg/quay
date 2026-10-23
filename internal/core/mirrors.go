package core

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/caoxg/quay/internal/board"
	"github.com/caoxg/quay/internal/mirror"
	"github.com/caoxg/quay/internal/remote"
)

// TestMirrors measures from the board itself how each mirror serves it: whether its Debian index
// (with security updates) and PyPI index answer, how soon, and the Debian download speed. Each
// result is also sent to sink as an "info" event carrying the mirror.Result, as soon as it is in.
// The results come back best first.
func (s *Session) TestMirrors(ctx context.Context, sink Sink, list []mirror.Mirror) ([]mirror.Result, error) {
	o := s.op(ctx, sink)
	payload, err := json.Marshal(map[string]any{"mirrors": list, "seconds": 2.5})
	if err != nil {
		return nil, err
	}
	var results []mirror.Result
	err = o.step("test", fmt.Sprintf("Test %d mirrors from the board", len(list)), func() error {
		w := &lineFunc{fn: func(l string) {
			var r mirror.Result
			if json.Unmarshal([]byte(l), &r) == nil && r.ID != "" {
				results = append(results, r)
				o.info(r.Summary(), r)
			}
		}}
		var errb bytes.Buffer
		err := s.T.Exec(ctx, "python3 - "+base64.StdEncoding.EncodeToString(payload), strings.NewReader(remote.MirrorTest), w, &errb)
		w.flush()
		if err != nil {
			return fmt.Errorf("mirror test: %s", orErr(lastLine(errb.String()), err))
		}
		return nil
	})
	return mirror.Rank(results), err
}

// lineFunc calls fn for each complete line written to it.
type lineFunc struct {
	buf []byte
	fn  func(string)
}

func (w *lineFunc) Write(p []byte) (int, error) {
	w.buf = append(w.buf, p...)
	for {
		i := bytes.IndexByte(w.buf, '\n')
		if i < 0 {
			return len(p), nil
		}
		w.fn(strings.TrimSpace(string(w.buf[:i])))
		w.buf = w.buf[i+1:]
	}
}

func (w *lineFunc) flush() {
	if len(bytes.TrimSpace(w.buf)) > 0 {
		w.fn(strings.TrimSpace(string(w.buf)))
	}
	w.buf = nil
}

// UploadKey adds a public key (an authorized_keys line, such as ~/.ssh/id_ed25519.pub) to the
// login user's ~/.ssh/authorized_keys on the board, as ssh-copy-id does; added is false when it
// was there already.
func (s *Session) UploadKey(ctx context.Context, sink Sink, line string) (added bool, err error) {
	line = strings.TrimSpace(line)
	blob := board.KeyBlob(line)
	if blob == "" {
		return false, fmt.Errorf("not an SSH public key")
	}
	o := s.op(ctx, sink)
	err = o.step("key", "Upload the SSH public key", func() error {
		out, err := o.quiet(remote.Script(remote.UploadKey, map[string]string{"KEY": line, "BLOB": blob}))
		if err != nil {
			return err
		}
		added = strings.Contains(out, "QUAY_ADDED")
		if !added {
			o.info("The key was already on the board", nil)
		}
		return nil
	})
	return added, err
}
