//go:build !nogui

package gui

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/wailsapp/wails/v3/pkg/application"

	"github.com/caoxg/quay/internal/applog"
	"github.com/caoxg/quay/internal/board"
	"github.com/caoxg/quay/internal/config"
	"github.com/caoxg/quay/internal/core"
	"github.com/caoxg/quay/internal/discover"
	"github.com/caoxg/quay/internal/hub"
	"github.com/caoxg/quay/internal/legal"
	"github.com/caoxg/quay/internal/mirror"
	"github.com/caoxg/quay/internal/profile"
	"github.com/caoxg/quay/internal/relay"
	"github.com/caoxg/quay/internal/selfupdate"
	"github.com/caoxg/quay/internal/source"
	"github.com/caoxg/quay/internal/version"
)

// Service is what the frontend calls. Anything that can wait on the network or on the user runs
// in the background and reports through "quay:op" events; methods here return quickly.
type Service struct {
	app *application.App

	mu       sync.Mutex
	sessions map[string]*core.Session
	cache    map[string]cached
	ops      map[string]context.CancelFunc
	prompts  map[string]chan reply
	bundles  map[string]*source.Bundle
	// relayRetry: boards whose relay is wanted and did not start, with when it was last tried
	relayRetry map[string]time.Time
}

type reply struct {
	value string
	ok    bool
}

// OpEvent is the payload of "quay:op".
type OpEvent struct {
	core.Event
	Kind   string `json:"kind,omitempty"`
	Board  string `json:"board,omitempty"`
	Folder string `json:"folder,omitempty"`
	Result any    `json:"result,omitempty"`
	Error  string `json:"error,omitempty"`
	Unsafe bool   `json:"unsafe,omitempty"` // refused by the app's safety probe; can be forced
}

// Prompt is the payload of "quay:prompt".
type Prompt struct {
	ID          string `json:"id"`
	Kind        string `json:"kind"` // secret | hostkey
	Title       string `json:"title"`
	Message     string `json:"message"`
	Host        string `json:"host,omitempty"`
	KeyType     string `json:"keyType,omitempty"`
	Fingerprint string `json:"fingerprint,omitempty"`
}

func newService() *Service {
	hub.Default = &hub.Loader{Dir: config.ProfileCacheDir(), Manual: func() bool {
		cfg, err := config.Load()
		return err == nil && cfg.ProfilesManual
	}}
	if hub.Default.Auto() {
		go func() { // a newer copy of the profiles repository, for the boards opened after this
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			_ = hub.Default.Refresh(ctx)
		}()
	}
	return &Service{
		sessions: map[string]*core.Session{},
		cache:    map[string]cached{},
		ops:      map[string]context.CancelFunc{},
		prompts:  map[string]chan reply{},
		bundles:  map[string]*source.Bundle{},

		relayRetry: map[string]time.Time{},
	}
}

func id() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func (s *Service) emit(name string, data any) {
	if s.app != nil {
		s.app.Event.Emit(name, data)
	}
}

// ---------------------------------------------------------------- sync calls

// Info describes the app for the frontend.
type Info struct {
	Version      string             `json:"version"`
	OS           string             `json:"os"`
	Lang         string             `json:"lang"`
	BackupsDir   string             `json:"backupsDir"`
	Catalog      []*profile.Profile `json:"catalog"`
	Mirror       string             `json:"mirror"`
	DebianMirror string             `json:"debianMirror"`
	Mirrors      []mirror.Mirror    `json:"mirrors"`  // mirrors in mainland China
	Official     mirror.Mirror      `json:"official"` // the origin servers, for comparison
	PublicKey    *PublicKey         `json:"publicKey"`
}

// PublicKey is this computer's SSH public key (~/.ssh/id_*.pub), if there is one.
type PublicKey struct {
	Path string `json:"path"`
	Blob string `json:"blob"` // its base64 part, to compare with the board's authorized keys
}

// Licenses is Quay's own notice and the licenses of the third-party code compiled into it.
type Licenses struct {
	Notice     string `json:"notice"`
	ThirdParty string `json:"thirdParty"`
}

func (s *Service) Licenses() Licenses {
	return Licenses{Notice: legal.Notice, ThirdParty: legal.ThirdParty}
}

func (s *Service) Info() Info {
	cfg, _ := config.Load()
	lang := ""
	if cfg != nil {
		lang = cfg.Lang
	}
	var catalog []*profile.Profile
	for _, p := range profile.Builtins() {
		if p.Source != nil {
			catalog = append(catalog, p)
		}
	}
	info := Info{Version: version.Version, OS: runtime.GOOS, Lang: lang, BackupsDir: config.BackupsDir(), Catalog: catalog,
		Mirror: core.DefaultMirror, DebianMirror: core.DefaultDebianMirror, Mirrors: mirror.China, Official: mirror.Official}
	if path, line, err := board.LocalPublicKey(""); err == nil {
		info.PublicKey = &PublicKey{Path: path, Blob: board.KeyBlob(line)}
	}
	return info
}

// BoardList is the saved boards plus the default.
type BoardList struct {
	Boards  []board.Target `json:"boards"`
	Default string         `json:"default"`
}

func (s *Service) Boards() (BoardList, error) {
	cfg, err := config.Load()
	if err != nil {
		return BoardList{}, err
	}
	if cfg.Boards == nil {
		cfg.Boards = []board.Target{}
	}
	return BoardList{Boards: cfg.Boards, Default: cfg.Default}, nil
}

func normalizeTarget(t *board.Target) error {
	switch t.Kind {
	case board.KindSSH:
		t.Host = strings.TrimSpace(t.Host)
		if t.Host == "" {
			return errors.New("enter the board's host name or IP address")
		}
		if t.User == "" {
			t.User = "arduino"
		}
		if t.Port == 0 {
			t.Port = 22
		}
	case board.KindADB:
		t.Host, t.User, t.Port = "", "", 0
	default:
		return fmt.Errorf("unknown connection type %q", t.Kind)
	}
	return nil
}

// SaveBoard changes how a saved board is reached: its network address (host, user, port, key; an
// empty host leaves only USB), its USB serial when t has one, and Via. Its name is the board's
// hostname and does not change here (see Rename); new boards come in through AddBoard.
func (s *Service) SaveBoard(t board.Target, oldName string, makeDefault bool) error {
	if oldName == "" {
		return errors.New("new boards are added with AddBoard")
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	old, ok := cfg.Board(oldName)
	if !ok {
		return fmt.Errorf("no saved board %q", oldName)
	}
	m := old
	m.Host, m.Via = strings.TrimSpace(t.Host), t.Via
	m.User, m.Port, m.KeyFile = "", 0, ""
	if m.Host != "" {
		m.User, m.Port, m.KeyFile = cmp.Or(strings.TrimSpace(t.User), "arduino"), cmp.Or(t.Port, 22), strings.TrimSpace(t.KeyFile)
	}
	if t.Serial != "" {
		m.Serial = t.Serial
	}
	switch {
	case t.LastIP != "":
		m.LastIP = t.LastIP
	case !strings.EqualFold(m.Host, old.Host):
		m.LastIP = ""
	}
	if err := m.CheckWays(); err != nil {
		return err
	}
	cfg.Put(m)
	if makeDefault || len(cfg.Boards) == 1 {
		cfg.Default = m.Name
	}
	s.dropSession(oldName)
	return cfg.Save()
}

// SetVia chooses the way to a board Quay can reach both ways: "" (the network first, then USB),
// board.KindSSH or board.KindADB. The next operation connects that way.
func (s *Service) SetVia(name, via string) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	t, ok := cfg.Board(name)
	if !ok {
		return fmt.Errorf("no saved board %q", name)
	}
	t.Via = via
	if err := t.CheckWays(); err != nil {
		return err
	}
	cfg.Put(t)
	if err := cfg.Save(); err != nil {
		return err
	}
	s.dropSession(name)
	return nil
}

// Reconnect closes the board's connection, so the next operation connects afresh (by default
// the network first again, after a fallback to USB).
func (s *Service) Reconnect(name string) {
	s.dropSession(name)
}

// SetSkipDesktop remembers whether the board's system updates leave out the on-board desktop.
func (s *Service) SetSkipDesktop(name string, skip bool) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	t, ok := cfg.Board(name)
	if !ok {
		return fmt.Errorf("no saved board %q", name)
	}
	t.SkipDesktop = skip
	cfg.Put(t)
	if err := cfg.Save(); err != nil {
		return err
	}
	s.mu.Lock()
	if sess := s.sessions[strings.ToLower(name)]; sess != nil {
		sess.Target.SkipDesktop = skip
	}
	s.mu.Unlock()
	return nil
}

func (s *Service) RemoveBoard(name string) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	cfg.Remove(name)
	s.dropSession(name)
	return cfg.Save()
}

func (s *Service) SetDefault(name string) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	cfg.Default = name
	return cfg.Save()
}

func (s *Service) SetLang(lang string) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	cfg.Lang = lang
	return cfg.Save()
}

// USBDevices lists boards connected over USB (adb).
// USBDevice is a device on USB, with what it says about itself (Setup: whether it had its first
// setup) and the saved board it is, if any.
type USBDevice struct {
	board.ADBDevice
	Hostname string      `json:"hostname,omitempty"`
	Model    string      `json:"model,omitempty"`
	Setup    *core.Setup `json:"setup,omitempty"`
	Saved    string      `json:"saved,omitempty"`
}

func (s *Service) USBDevices() ([]USBDevice, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	list, err := board.ADBDevices(ctx)
	out := make([]USBDevice, len(list))
	cfg, _ := config.Load()
	var wg sync.WaitGroup
	for i, d := range list {
		out[i].ADBDevice = d
		if cfg != nil {
			if t, ok := cfg.ByBoardID(d.Serial); ok { // a UNO Q's adb serial is its serial number
				out[i].Saved = t.Name
			}
		}
		if d.State == "device" {
			wg.Add(1)
			go func(u *USBDevice) {
				defer wg.Done()
				dctx, cancel := context.WithTimeout(ctx, 3*time.Second)
				defer cancel()
				i := board.ADBDescribe(dctx, u.Serial)
				u.Hostname, u.Model = i.Hostname, i.Model
				if i.SSH != "" {
					u.Setup = &core.Setup{SSHActive: i.SSH == "active", Password: i.Password}
				}
			}(&out[i])
		}
	}
	wg.Wait()
	return out, err
}

// Respond answers a prompt.
func (s *Service) Respond(promptID, value string, ok bool) {
	s.mu.Lock()
	ch := s.prompts[promptID]
	delete(s.prompts, promptID)
	s.mu.Unlock()
	if ch != nil {
		ch <- reply{value: value, ok: ok}
	}
}

// Cancel stops a running operation.
func (s *Service) Cancel(opID string) {
	s.mu.Lock()
	c := s.ops[opID]
	s.mu.Unlock()
	if c != nil {
		c()
	}
}

func (s *Service) Disconnect(name string) { s.dropSession(name) }

// ---------------------------------------------------------------- dialogs & shell

func (s *Service) PickBackup() (string, error) {
	return s.app.Dialog.OpenFile().SetTitle("Choose a Quay backup").SetDirectory(config.BackupsDir()).
		AddFilter("Quay backup (*.tgz)", "*.tgz").PromptForSingleSelection()
}

func (s *Service) PickExport(name string) (string, error) {
	home, _ := os.UserHomeDir()
	return s.app.Dialog.SaveFile().SetMessage("Save the App Lab zip").SetDirectory(filepath.Join(home, "Downloads")).
		SetFilename(name).PromptForSingleSelection()
}

func (s *Service) PickFolder() (string, error) {
	return s.app.Dialog.OpenFile().SetTitle("Choose an Arduino App folder").CanChooseDirectories(true).
		CanChooseFiles(false).PromptForSingleSelection()
}

func (s *Service) PickKey() (string, error) {
	home, _ := os.UserHomeDir()
	return s.app.Dialog.OpenFile().SetTitle("Choose an SSH private key").SetDirectory(filepath.Join(home, ".ssh")).
		ShowHiddenFiles(true).PromptForSingleSelection()
}

func (s *Service) OpenURL(u string) error {
	if !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
		return errors.New("not a web address")
	}
	return s.app.Browser.OpenURL(u)
}

// Reveal shows a file (or folder) in the system file manager.
func (s *Service) Reveal(path string) error {
	if path == "" {
		path = config.BackupsDir()
		_ = os.MkdirAll(path, 0o755)
	}
	switch runtime.GOOS {
	case "darwin":
		return exec.Command("open", "-R", path).Start()
	case "windows":
		return exec.Command("explorer", "/select,", path).Start()
	default:
		if st, err := os.Stat(path); err == nil && !st.IsDir() {
			path = filepath.Dir(path)
		}
		return exec.Command("xdg-open", path).Start()
	}
}

// ---------------------------------------------------------------- prompter

type prompter struct{ s *Service }

func (p prompter) ask(ctx context.Context, pr Prompt) (reply, error) {
	pr.ID = id()
	ch := make(chan reply, 1)
	p.s.mu.Lock()
	p.s.prompts[pr.ID] = ch
	p.s.mu.Unlock()
	p.s.emit("quay:prompt", pr)
	select {
	case r := <-ch:
		return r, nil
	case <-ctx.Done():
		p.s.mu.Lock()
		delete(p.s.prompts, pr.ID)
		p.s.mu.Unlock()
		p.s.emit("quay:prompt-cancel", pr.ID)
		return reply{}, ctx.Err()
	}
}

func (p prompter) Secret(ctx context.Context, title, message string) (string, bool, error) {
	r, err := p.ask(ctx, Prompt{Kind: "secret", Title: title, Message: message})
	return r.value, r.ok, err
}

func (p prompter) TrustHostKey(ctx context.Context, host, keyType, fp string) (bool, error) {
	r, err := p.ask(ctx, Prompt{Kind: "hostkey", Title: "New board", Message: host, Host: host, KeyType: keyType, Fingerprint: fp})
	return r.ok, err
}

// ---------------------------------------------------------------- sessions

func (s *Service) dropSession(name string) {
	if name == "" {
		return
	}
	s.mu.Lock()
	sess := s.sessions[strings.ToLower(name)]
	delete(s.sessions, strings.ToLower(name))
	s.mu.Unlock()
	if sess != nil {
		_ = sess.Close()
	}
}

// keep makes sess the board's session and watches it: when the board goes away (switched off, out
// of the Wi‑Fi, the cable out), the session is dropped and the window told at once (quay:lost,
// with the board's name), instead of at the next status check.
func (s *Service) keep(name string, sess *core.Session) {
	s.mu.Lock()
	s.sessions[strings.ToLower(name)] = sess
	s.mu.Unlock()
	lost := sess.Lost()
	if lost == nil {
		return
	}
	go func() {
		<-lost
		key := ""
		s.mu.Lock()
		for k, v := range s.sessions { // under its current name: the board may have been renamed
			if v == sess {
				key = k
				delete(s.sessions, k)
			}
		}
		s.mu.Unlock()
		if key == "" {
			return // dropped or replaced on purpose
		}
		reason := sess.T.(board.Watched).LostReason() // before Close, after which it is ""
		sess.Close()
		applog.Default.Printf("[connection %s] lost: %s", key, reason)
		name := key
		if cfg, err := config.Load(); err == nil {
			if t, ok := cfg.Board(key); ok {
				name = t.Name
			}
		}
		s.emit("quay:lost", map[string]string{"board": name, "reason": reason})
	}()
}

func (s *Service) session(ctx context.Context, name string, sink core.Sink) (*core.Session, error) {
	key := strings.ToLower(name)
	s.mu.Lock()
	sess := s.sessions[key]
	s.mu.Unlock()
	if sess != nil {
		s.retryRelay(ctx, key, sess)
		return sess, nil
	}
	cfg, err := config.Load()
	if err != nil {
		return nil, err
	}
	t, ok := cfg.Board(name)
	if !ok {
		return nil, fmt.Errorf("no saved board %q", name)
	}
	sink(core.Event{Type: "step", Step: "connect", State: "run", Text: "Connect to " + t.String(), Time: time.Now().UnixMilli()})
	sess, err = core.Connect(ctx, t, prompter{s}, config.KnownHostsFile())
	if err != nil {
		sink(core.Event{Type: "step", Step: "connect", State: "fail", Text: err.Error(), Time: time.Now().UnixMilli()})
		return nil, err
	}
	sink(core.Event{Type: "step", Step: "connect", State: "ok", Text: "Connected to " + sess.T.Describe(), Time: time.Now().UnixMilli()})
	s.keep(key, sess)
	if s.maybeRelay(ctx, sess) {
		s.mu.Lock()
		s.relayRetry[key] = time.Now()
		s.mu.Unlock()
	}
	return sess, nil
}

// maybeRelay starts relaying the board's downloads through this computer when the board has the
// hook (Relay on). It lasts as long as the session: while the app is open and connected. It
// reports whether the relay is wanted (the hook is there) and could not start.
func (s *Service) maybeRelay(ctx context.Context, sess *core.Session) (failed bool) {
	out, err := board.Output(ctx, sess.T, "test -f /etc/apt/apt.conf.d/80quay-relay && echo yes || true")
	if err != nil || strings.TrimSpace(out) != "yes" {
		return false
	}
	cfg, err := config.Load()
	if err != nil {
		return false
	}
	up, err := relay.Upstream(cfg.RelayUpstream)
	if err == nil {
		err = sess.StartRelay(&relay.Proxy{Upstream: up})
	}
	if err != nil {
		applog.Default.Printf("[relay %s] could not start: %v", sess.Target.Name, err)
		return true
	}
	return false
}

// retryRelay starts the relay again on a session whose relay is wanted but not running: a session
// left behind by an earlier Quay can hold the board's relay port until the board's sshd notices
// it is gone, and a forward ends with its connection. At most every 20 seconds per board.
func (s *Service) retryRelay(ctx context.Context, key string, sess *core.Session) {
	s.mu.Lock()
	due := s.relayRetry[key] != (time.Time{}) && time.Since(s.relayRetry[key]) > 20*time.Second && !sess.RelayActive()
	if due {
		s.relayRetry[key] = time.Now()
	}
	s.mu.Unlock()
	if !due {
		return
	}
	if s.maybeRelay(ctx, sess) {
		return
	}
	s.mu.Lock()
	delete(s.relayRetry, key)
	s.mu.Unlock()
	if sess.RelayActive() {
		applog.Default.Printf("[relay %s] running again", sess.Target.Name)
	}
}

// learn keeps a saved board in step with the board: its name is its hostname, and its serial
// number and address are remembered (config.Learn). It returns the board's name afterwards; the
// session moves along when the name changes.
func (s *Service) learn(name string, sess *core.Session, st *core.Status) string {
	cfg, err := config.Load()
	if err != nil {
		return name
	}
	newName, changed := cfg.Learn(name, st.Hostname, st.Serial, st.PrimaryIP())
	if !changed {
		return newName
	}
	t, _ := cfg.Board(newName)
	if t.Host != sess.Target.Host {
		sess.TrustAs(t.Host, config.KnownHostsFile())
	}
	if cfg.Save() != nil {
		return name
	}
	sess.Target = t
	if !strings.EqualFold(newName, name) {
		s.mu.Lock()
		if s.sessions[strings.ToLower(name)] == sess {
			delete(s.sessions, strings.ToLower(name))
			s.sessions[strings.ToLower(newName)] = sess
		}
		s.mu.Unlock()
	}
	return newName
}

// connectionLost reports errors that mean the session must be re-opened.
func connectionLost(err error) bool {
	if err == nil {
		return false
	}
	m := err.Error()
	for _, k := range []string{"EOF", "closed network connection", "broken pipe", "connection reset", "ssh session", "no route to host", "i/o timeout",
		"adb: device", "no devices/emulators found"} { // adb: the cable came out
		if strings.Contains(m, k) {
			return true
		}
	}
	return false
}

// run starts a background operation and returns its id.
func (s *Service) run(kind, boardName, folder string, fn func(ctx context.Context, sess *core.Session, sink core.Sink) (any, error)) string {
	opID := id()
	ctx, cancel := context.WithCancel(context.Background())
	s.mu.Lock()
	s.ops[opID] = cancel
	s.mu.Unlock()
	base := OpEvent{Kind: kind, Board: boardName, Folder: folder}
	tag := "[" + kind
	if boardName != "" {
		tag += " " + boardName
	}
	if folder != "" {
		tag += "/" + folder
	}
	tag += " " + opID[:6] + "]"
	send := func(e OpEvent) {
		e.Kind, e.Board, e.Folder = base.Kind, base.Board, base.Folder
		e.Op = opID
		if e.Time == 0 {
			e.Time = time.Now().UnixMilli()
		}
		record(tag, e)
		s.emit("quay:op", e)
	}
	sink := func(e core.Event) { send(OpEvent{Event: e}) }
	send(OpEvent{Event: core.Event{Type: "begin"}})
	go func() {
		defer func() {
			cancel()
			s.mu.Lock()
			delete(s.ops, opID)
			s.mu.Unlock()
			if r := recover(); r != nil {
				send(OpEvent{Event: core.Event{Type: "end", State: "fail"}, Error: fmt.Sprint("internal error: ", r)})
			}
		}()
		var sess *core.Session
		var err error
		if boardName != "" {
			sess, err = s.session(ctx, boardName, sink)
		}
		var res any
		if err == nil {
			res, err = fn(ctx, sess, sink)
			if boardName != "" && ctx.Err() == nil && (connectionLost(err) || sess.Gone()) {
				s.dropSession(boardName) // stale connection: reconnect once and retry
				if sess, err = s.session(ctx, boardName, sink); err == nil {
					res, err = fn(ctx, sess, sink)
				}
			}
		}
		switch {
		case err != nil && ctx.Err() != nil:
			send(OpEvent{Event: core.Event{Type: "end", State: "cancel"}, Error: "cancelled"})
		case err != nil:
			var unsafe *core.UnsafeError
			send(OpEvent{Event: core.Event{Type: "end", State: "fail"}, Error: err.Error(), Unsafe: errors.As(err, &unsafe)})
		default:
			send(OpEvent{Event: core.Event{Type: "end", State: "ok"}, Result: res})
		}
	}()
	return opID
}

// routine operations run by themselves (the board page refreshes its status every 30 seconds):
// the log keeps only what is out of the ordinary in them, not a begin and an end twice a minute.
var routine = map[string]bool{"status": true, "check-updates": true, "network": true, "logs": true}

// record writes an operation's event to Quay's log.
func record(tag string, e OpEvent) {
	switch {
	case e.Type == "begin":
		if !routine[e.Kind] {
			applog.Default.Printf("%s begin", tag)
		}
	case e.Type == "end" && e.Error != "":
		applog.Default.Printf("%s end %s: %s", tag, e.State, e.Error)
	case e.Type == "end":
		if !routine[e.Kind] {
			applog.Default.Printf("%s end %s", tag, e.State)
		}
	case e.Type == "log" && e.Kind == "logs": // the app's own output, for its log viewer
	default:
		core.Record(tag, e.Event)
	}
}

// ---------------------------------------------------------------- operations (return op ids)

// BoardStatus is a board's status plus its saved name, which follows the board's hostname: when
// Board differs from the name the frontend asked about, the board was renamed.
type BoardStatus struct {
	*core.Status
	Board string `json:"board"`
	Via   string `json:"via"` // the way this connection took: ssh (network) or adb (USB)
	// Replaced is the saved entry AddBoard took over (same serial number, reached at another
	// address before), and ReplacedVia how it was reached.
	Replaced    string `json:"replaced,omitempty"`
	ReplacedVia string `json:"replacedVia,omitempty"`
	// Joined: AddBoard reached a saved board the other way, and Quay now knows both.
	Joined bool `json:"joined,omitempty"`
}

func (s *Service) Status(boardName string) string {
	return s.run("status", boardName, "", func(ctx context.Context, sess *core.Session, _ core.Sink) (any, error) {
		st, err := sess.Status(ctx)
		if err != nil {
			return nil, err
		}
		return BoardStatus{Status: st, Board: s.learn(boardName, sess, st), Via: sess.Via()}, nil
	})
}

// Found is a board found on the network, with the saved board it is, if any.
type Found struct {
	discover.Board
	Saved string `json:"saved,omitempty"`
}

// Scan looks for boards on the local network.
func (s *Service) Scan() string {
	return s.run("scan", "", "", func(ctx context.Context, _ *core.Session, _ core.Sink) (any, error) {
		found, err := discover.Browse(ctx, 2500*time.Millisecond)
		if err != nil {
			return nil, err
		}
		cfg, _ := config.Load()
		out := []Found{}
		for _, b := range found {
			f := Found{Board: b}
			if cfg != nil {
				if t, ok := cfg.ByBoardID(b.Serial); ok {
					f.Saved = t.Name
				}
			}
			out = append(out, f)
		}
		return out, nil
	})
}

// Seen says how a saved board Quay could not reach has shown up again: "network" when it
// announces itself on the local network (mDNS, as a board does a few seconds into booting),
// "usb" when it is plugged in, "" when neither. The window asks every few seconds while it shows
// that the board cannot be reached, and connects when it has shown up.
func (s *Service) Seen(name string) (string, error) {
	cfg, err := config.Load()
	if err != nil {
		return "", err
	}
	t, ok := cfg.Board(name)
	if !ok {
		return "", fmt.Errorf("no saved board %q", name)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if usb, ok := t.USB(); ok && t.Via != board.KindSSH {
		if devs, err := board.ADBDevices(ctx); err == nil { // no adb: no USB
			for _, d := range devs {
				if d.State == "device" && (usb.Serial == "" || d.Serial == usb.Serial) {
					return board.KindADB, nil
				}
			}
		}
	}
	if t.Host != "" && t.Via != board.KindADB {
		if _, ok, _ := discover.Find(ctx, 1500*time.Millisecond, func(b discover.Board) bool { return sameBoard(t, b) }); ok {
			return "network", nil
		}
	}
	return "", nil
}

// sameBoard says whether a board found on the network is the saved board t: by serial number,
// else by its host name or address.
func sameBoard(t board.Target, b discover.Board) bool {
	if t.BoardID != "" && b.Serial != "" {
		return strings.EqualFold(t.BoardID, b.Serial)
	}
	return strings.EqualFold(strings.TrimSuffix(t.Host, "."), b.Host) || slices.Contains(b.IPs, t.Host)
}

// AddBoard connects to a board and saves it under its own name (its hostname). Adding a board that
// is already saved (same serial number) adds this way to its entry (config.Adopt). uploadKey also
// uploads this computer's SSH public key, so later connections need no password.
func (s *Service) AddBoard(t board.Target, makeDefault, uploadKey bool) string {
	return s.run("addboard", "", "", func(ctx context.Context, _ *core.Session, sink core.Sink) (any, error) {
		if err := normalizeTarget(&t); err != nil {
			return nil, err
		}
		sink(core.Event{Type: "step", Step: "connect", State: "run", Text: "Connect to " + t.String()})
		sess, err := core.Connect(ctx, t, prompter{s}, config.KnownHostsFile())
		if err != nil {
			sink(core.Event{Type: "step", Step: "connect", State: "fail", Text: err.Error()})
			return nil, err
		}
		sink(core.Event{Type: "step", Step: "connect", State: "ok", Text: "Connected to " + sess.T.Describe()})
		st, err := sess.Status(ctx)
		if err != nil {
			sess.Close()
			return nil, err
		}
		cfg, err := config.Load()
		if err != nil {
			sess.Close()
			return nil, err
		}
		name, via, joined := cfg.Adopt(t, st.Hostname, st.Serial)
		replaced := ""
		if via != "" {
			replaced = name
		}
		name, _ = cfg.Learn(name, st.Hostname, st.Serial, st.PrimaryIP())
		if makeDefault || cfg.Default == "" {
			cfg.Default = name
		}
		if err := cfg.Save(); err != nil {
			sess.Close()
			return nil, err
		}
		sess.Target, _ = cfg.Board(name)
		// A fresh board gets the key in its first setup, which asks again with this choice.
		if uploadKey && !st.Setup.Needed() {
			if _, line, err := localOrNewKey(t.KeyFile, true); err == nil {
				if _, err := sess.UploadKey(ctx, sink, line); err == nil {
					st, _ = sess.Status(ctx) // shows the key as uploaded
				}
			}
		}
		s.dropSession(name)
		s.keep(name, sess)
		s.maybeRelay(ctx, sess)
		return BoardStatus{Status: st, Board: name, Via: sess.Via(), Replaced: replaced, ReplacedVia: via, Joined: joined}, nil
	})
}

// MirrorTest measures, from the board, how the mirrors in mainland China and the official servers
// serve it. Each result also arrives as an "info" event carrying it, as soon as it is in.
func (s *Service) MirrorTest(boardName string) string {
	return s.run("mirrortest", boardName, "", func(ctx context.Context, sess *core.Session, sink core.Sink) (any, error) {
		return sess.TestMirrors(ctx, sink, append(append([]mirror.Mirror{}, mirror.China...), mirror.Official))
	})
}

// KeyUpload is the result of UploadKey.
type KeyUpload struct {
	Path    string `json:"path"`
	Added   bool   `json:"added"`
	Created bool   `json:"created"` // the key pair was generated first
}

// UploadKey uploads this computer's SSH public key to the board (like ssh-copy-id). When there is
// no key and generate is set, it first creates ~/.ssh/id_ed25519, as ssh-keygen would.
func (s *Service) UploadKey(boardName string, generate bool) string {
	return s.run("uploadkey", boardName, "", func(ctx context.Context, sess *core.Session, sink core.Sink) (any, error) {
		_, _, missing := board.LocalPublicKey(sess.Target.KeyFile)
		path, line, err := localOrNewKey(sess.Target.KeyFile, generate)
		if err != nil {
			return nil, err
		}
		added, err := sess.UploadKey(ctx, sink, line)
		return KeyUpload{Path: path, Added: added, Created: missing != nil}, err
	})
}

// localOrNewKey is this computer's SSH public key, created first when there is none and generate
// is set.
func localOrNewKey(keyFile string, generate bool) (string, string, error) {
	path, line, err := board.LocalPublicKey(keyFile)
	if err != nil && generate {
		return board.GenerateKey()
	}
	return path, line, err
}

// SetPassword sets the board user's password (no current password needed; see core.SetPassword).
func (s *Service) SetPassword(boardName, pw string) string {
	return s.run("passwd", boardName, "", func(ctx context.Context, sess *core.Session, sink core.Sink) (any, error) {
		return nil, sess.SetPassword(ctx, sink, pw)
	})
}

// Rename renames the board itself (its hostname); the saved board follows.
func (s *Service) Rename(boardName, newName string) string {
	return s.run("rename", boardName, "", func(ctx context.Context, sess *core.Session, sink core.Sink) (any, error) {
		if err := sess.Rename(ctx, sink, newName); err != nil {
			return nil, err
		}
		st, err := sess.Status(ctx)
		if err != nil {
			return nil, err
		}
		return BoardStatus{Status: st, Board: s.learn(boardName, sess, st), Via: sess.Via()}, nil
	})
}

// SystemCheck lists the board's pending system updates; refresh runs apt-get update first.
func (s *Service) SystemCheck(boardName string, refresh bool) string {
	return s.run("syscheck", boardName, "", func(ctx context.Context, sess *core.Session, sink core.Sink) (any, error) {
		return sess.CheckUpgrades(ctx, sink, refresh)
	})
}

// SystemCheckAuto is SystemCheck for the board page: it refreshes missing or old package lists
// by itself when the board allows that without a password (core.CheckUpgradesFresh).
func (s *Service) SystemCheckAuto(boardName string) string {
	return s.run("syscheck", boardName, "", func(ctx context.Context, sess *core.Session, sink core.Sink) (any, error) {
		return sess.CheckUpgradesFresh(ctx, sink)
	})
}

// SystemUpdate installs the board's system updates. When the update's service restarts drop the
// connection, it reconnects as soon as the board answers and follows the update to its end.
func (s *Service) SystemUpdate(boardName string, opt core.UpgradeOptions) string {
	return s.run("sysupdate", boardName, "", func(ctx context.Context, sess *core.Session, sink core.Sink) (any, error) {
		res, err := sess.Upgrade(ctx, sink, opt)
		if !errors.Is(err, core.ErrDropped) {
			return res, err
		}
		sink(core.Event{Type: "warn", Text: "The connection dropped: the update restarts the board's services (sshd among them). Reconnecting…", Time: time.Now().UnixMilli()})
		s.dropSession(boardName)
		for end := time.Now().Add(90 * time.Second); time.Now().Before(end); {
			if ns, cerr := s.session(ctx, boardName, sink); cerr == nil {
				return ns.FollowUpgrade(ctx, sink, opt)
			}
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(3 * time.Second):
			}
		}
		return nil, err
	})
}

// RelayInfo is the relay's proxy setting on this computer.
type RelayInfo struct {
	Upstream  string `json:"upstream"`  // configured; empty = system settings
	Effective string `json:"effective"` // what is used now; empty = direct
	Error     string `json:"error,omitempty"`
}

// RelayCheck is what is wrong with a proxy address typed for the relay, if anything (relay.Check).
type RelayCheck struct {
	Problem string `json:"problem,omitempty"` // format | scheme | port | unreachable
	Detail  string `json:"detail,omitempty"`
}

// CheckRelayUpstream checks a proxy address before it is saved.
func (s *Service) CheckRelayUpstream(upstream string) RelayCheck {
	p, d := relay.Check(upstream)
	return RelayCheck{Problem: p, Detail: d}
}

func relayCheckErr(upstream string) error {
	if p, d := relay.Check(upstream); p != "" {
		return fmt.Errorf("proxy %q: %s (%s)", strings.TrimSpace(upstream), p, d)
	}
	return nil
}

// SetRelayUpstream changes the proxy the relay chains through; relays running now switch at once.
func (s *Service) SetRelayUpstream(upstream string) (RelayInfo, error) {
	upstream = strings.TrimSpace(upstream)
	if err := relayCheckErr(upstream); err != nil {
		return RelayInfo{}, err
	}
	cfg, err := config.Load()
	if err != nil {
		return RelayInfo{}, err
	}
	cfg.RelayUpstream = upstream
	if err := cfg.Save(); err != nil {
		return RelayInfo{}, err
	}
	s.mu.Lock()
	var running []*core.Session
	for _, sess := range s.sessions {
		if sess.RelayActive() {
			running = append(running, sess)
		}
	}
	s.mu.Unlock()
	for _, sess := range running {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		sess.StopRelay()
		s.maybeRelay(ctx, sess)
		cancel()
	}
	return s.RelaySettings(), nil
}

func (s *Service) RelaySettings() RelayInfo {
	cfg, err := config.Load()
	if err != nil {
		return RelayInfo{Error: err.Error()}
	}
	info := RelayInfo{Upstream: cfg.RelayUpstream}
	up, err := relay.Upstream(cfg.RelayUpstream)
	if err != nil {
		info.Error = err.Error()
	} else if up != nil {
		info.Effective = up.String()
	}
	return info
}

// Relay turns the board's download relay on (with this computer's upstream proxy setting) or off.
func (s *Service) Relay(boardName string, on bool, upstream string) string {
	return s.run("relay", boardName, "", func(ctx context.Context, sess *core.Session, sink core.Sink) (any, error) {
		if on {
			if err := relayCheckErr(upstream); err != nil {
				return nil, err
			}
			cfg, err := config.Load()
			if err != nil {
				return nil, err
			}
			cfg.RelayUpstream = strings.TrimSpace(upstream)
			if err := cfg.Save(); err != nil {
				return nil, err
			}
			sess.StopRelay() // restart with the new upstream
		}
		if err := sess.Relay(ctx, sink, on); err != nil {
			return nil, err
		}
		if on {
			s.maybeRelay(ctx, sess)
		} else {
			sess.StopRelay()
		}
		return nil, nil
	})
}

// Network reads the board's Wi-Fi: saved networks and the automatic hotspot.
func (s *Service) Network(boardName string) string {
	return s.run("network", boardName, "", func(ctx context.Context, sess *core.Session, _ core.Sink) (any, error) {
		return sess.Network(ctx)
	})
}

// WifiScan lists the Wi-Fi networks in range of the board.
func (s *Service) WifiScan(boardName string) string {
	return s.run("wifiscan", boardName, "", func(ctx context.Context, sess *core.Session, _ core.Sink) (any, error) {
		return sess.WifiScan(ctx)
	})
}

// WifiAdd saves a Wi-Fi network on the board (psk "" for an open network; phone for a phone hotspot).
func (s *Service) WifiAdd(boardName, ssid, psk string, hidden, phone bool) string {
	return s.run("wifi", boardName, "", func(ctx context.Context, sess *core.Session, sink core.Sink) (any, error) {
		return nil, sess.WifiAdd(ctx, sink, ssid, psk, hidden, phone)
	})
}

// WifiPhone marks a saved network as a phone hotspot or as an ordinary one.
func (s *Service) WifiPhone(boardName, uuid string, phone bool) string {
	return s.run("wifi", boardName, "", func(ctx context.Context, sess *core.Session, sink core.Sink) (any, error) {
		return nil, sess.WifiPhone(ctx, sink, uuid, phone)
	})
}

// WifiConnect switches the board to a saved Wi-Fi network now.
func (s *Service) WifiConnect(boardName, uuid string) string {
	return s.run("wifi", boardName, "", func(ctx context.Context, sess *core.Session, sink core.Sink) (any, error) {
		return nil, sess.WifiConnect(ctx, sink, uuid)
	})
}

// WifiRemove deletes a saved Wi-Fi network by its profile uuid.
func (s *Service) WifiRemove(boardName, uuid string) string {
	return s.run("wifi", boardName, "", func(ctx context.Context, sess *core.Session, sink core.Sink) (any, error) {
		return nil, sess.WifiRemove(ctx, sink, uuid)
	})
}

// Away sets the away plan (off, own, phone, both), saving the board's own hotspot first when ssid
// is given (psk "" keeps its password). Leaving the board's own hotspot out of the plan while it is
// open ends this connection a few seconds later.
func (s *Service) Away(boardName, mode, ssid, psk string) string {
	return s.run("away", boardName, "", func(ctx context.Context, sess *core.Session, sink core.Sink) (any, error) {
		if err := sess.Away(ctx, sink, mode, ssid, psk); err != nil {
			return nil, err
		}
		if mode == core.AwayOff || mode == core.AwayPhone {
			s.dropSession(boardName) // the board may change networks: reconnect next time
		}
		return nil, nil
	})
}

// HotspotNow opens the hotspot now (open) or closes it and joins a saved Wi-Fi. The board changes
// networks a few seconds later, so the session is dropped; the next operation reconnects.
func (s *Service) HotspotNow(boardName string, open bool) string {
	return s.run("hotspot", boardName, "", func(ctx context.Context, sess *core.Session, sink core.Sink) (any, error) {
		if err := sess.HotspotNow(ctx, sink, open); err != nil {
			return nil, err
		}
		s.dropSession(boardName)
		return nil, nil
	})
}

// ---------------------------------------------------------------- settings

// Settings is what the Settings dialog shows.
type Settings struct {
	ProfilesAuto    bool   `json:"profilesAuto"`    // fetch newer profiles by themselves
	ProfilesUpdated string `json:"profilesUpdated"` // the date of the copy in use
	ProfilesSource  string `json:"profilesSource"`  // "built in", or where it was fetched from
	LogDir          string `json:"logDir"`          // where Quay's log is written
}

// Settings returns the settings and the profiles copy in use.
func (s *Service) Settings() Settings {
	snap := hub.Get()
	return Settings{ProfilesAuto: hub.Default.Auto(), ProfilesUpdated: snap.Index.Updated, ProfilesSource: snap.Source, LogDir: applog.Default.Dir()}
}

// RevealLog shows today's log file in the system file manager, or the log folder before anything
// was written today.
func (s *Service) RevealLog() error {
	p := applog.Default.Path()
	if _, err := os.Stat(p); err != nil {
		p = applog.Default.Dir()
		if err := os.MkdirAll(p, 0o755); err != nil {
			return err
		}
	}
	return s.Reveal(p)
}

// SetProfilesAuto turns fetching newer profiles by themselves on or off.
func (s *Service) SetProfilesAuto(on bool) (Settings, error) {
	cfg, err := config.Load()
	if err != nil {
		return Settings{}, err
	}
	cfg.ProfilesManual = !on
	if err := cfg.Save(); err != nil {
		return Settings{}, err
	}
	return s.Settings(), nil
}

// RefreshProfiles fetches the newest profiles now (whatever the setting says).
func (s *Service) RefreshProfiles() string {
	return s.run("profiles", "", "", func(ctx context.Context, _ *core.Session, _ core.Sink) (any, error) {
		if err := hub.Default.Refresh(ctx); err != nil {
			return nil, err
		}
		return s.Settings(), nil
	})
}

// ---------------------------------------------------------------- Quay's own updates

// UpdateState is what the app shows about a newer Quay: from the last check (no network).
type UpdateState struct {
	Current   string              `json:"current"`
	Dev       bool                `json:"dev"` // a development build: nothing is offered
	Checked   int64               `json:"checked"`
	Available bool                `json:"available"`
	Latest    *selfupdate.Release `json:"latest,omitempty"`
	Kind      string              `json:"kind"` // how this Quay is installed (selfupdate.Kind…)
	Path      string              `json:"path"`
	Size      int64               `json:"size,omitempty"` // of the download, when known
	Error     string              `json:"error,omitempty"`
}

func (s *Service) updateState(c *selfupdate.Check) UpdateState {
	st := UpdateState{Current: version.Version, Dev: version.Version == "dev"}
	if t, err := selfupdate.New().Where(); err == nil {
		st.Kind, st.Path = t.Kind, t.Path
	}
	if c != nil {
		st.Checked, st.Latest, st.Error = c.Checked.UnixMilli(), c.Latest, c.Error
		st.Available = !st.Dev && c.Available()
		if c.Latest != nil {
			st.Size = c.Latest.Size(selfupdate.AssetName(st.Kind, c.Latest.Version))
		}
	}
	return st
}

// UpdateState returns the last check.
func (s *Service) UpdateState() UpdateState {
	return s.updateState(selfupdate.LoadCheck(selfupdate.CheckFile()))
}

// CheckUpdate asks GitHub for the newest release, unless the last check is recent (force: always).
func (s *Service) CheckUpdate(force bool) string {
	return s.run("updatecheck", "", "", func(ctx context.Context, _ *core.Session, _ core.Sink) (any, error) {
		file := selfupdate.CheckFile()
		c := selfupdate.LoadCheck(file)
		if force || c == nil || time.Since(c.Checked) > 12*time.Hour {
			rel, err := selfupdate.New().Latest(ctx)
			c = &selfupdate.Check{Checked: time.Now(), Current: version.Version, Latest: rel}
			if err != nil {
				c.Error = err.Error()
			}
			_ = c.Save(file)
		}
		return s.updateState(c), nil
	})
}

// InstallUpdate downloads, verifies and installs the release the last check found. The result
// says whether to Relaunch, or what is left for the user (the .deb).
func (s *Service) InstallUpdate() string {
	return s.run("selfupdate", "", "", func(ctx context.Context, _ *core.Session, sink core.Sink) (any, error) {
		c := selfupdate.LoadCheck(selfupdate.CheckFile())
		if c == nil || !c.Available() {
			return nil, errors.New("no newer Quay to install")
		}
		text := "Download, verify and install Quay " + c.Latest.Version
		sink(core.Event{Type: "step", Step: "selfupdate", State: "run", Text: text})
		res, err := selfupdate.New().Install(ctx, c.Latest, func(done, total int64) {
			pct := 0.0
			if total > 0 {
				pct = 100 * float64(done) / float64(total)
			}
			sink(core.Event{Type: "progress", Step: "selfupdate", Data: map[string]any{"percent": pct, "done": done, "total": total, "unit": "bytes"}})
		})
		if err != nil {
			sink(core.Event{Type: "step", Step: "selfupdate", State: "fail", Text: err.Error()})
			return nil, err
		}
		sink(core.Event{Type: "step", Step: "selfupdate", State: "ok", Text: text})
		return res, nil
	})
}

// Relaunch starts the updated Quay once this one has quit, and quits.
func (s *Service) Relaunch() error {
	t, err := selfupdate.New().Where()
	if err != nil {
		return err
	}
	if err := selfupdate.Relaunch(t); err != nil {
		return err
	}
	if s.app != nil {
		go s.app.Quit()
	}
	return nil
}

// SystemFix applies a system profile finding's built-in action (see core.SystemFix).
func (s *Service) SystemFix(boardName, id string) string {
	return s.run("sysfix", boardName, "", func(ctx context.Context, sess *core.Session, sink core.Sink) (any, error) {
		return nil, sess.SystemFix(ctx, sink, id)
	})
}

// Refs lists a repository's branches and tags.
func (s *Service) Refs(repo string) string {
	return s.run("refs", "", "", func(ctx context.Context, _ *core.Session, _ core.Sink) (any, error) {
		return source.ListRefs(ctx, repo)
	})
}

// InspectRequest names what to deploy.
type InspectRequest struct {
	Repo   string `json:"repo"`
	Ref    string `json:"ref"`
	Subdir string `json:"subdir"`
	Dir    string `json:"dir"`
}

// Inspection is what the install dialog previews.
type Inspection struct {
	Key     string           `json:"key"`
	Bundle  *source.Bundle   `json:"bundle"`
	Profile *profile.Profile `json:"profile,omitempty"`
	Folder  string           `json:"folder"`
}

// Inspect fetches the app on this computer and describes it (kept for Deploy).
func (s *Service) Inspect(req InspectRequest) string {
	return s.run("inspect", "", "", func(ctx context.Context, _ *core.Session, sink core.Sink) (any, error) {
		b, err := core.Fetch(ctx, sink, config.RepoCacheDir(), core.FetchOptions{Repo: req.Repo, Ref: req.Ref, Subdir: req.Subdir, Dir: req.Dir})
		if err != nil {
			return nil, err
		}
		p, err := profile.Lookup(b.QuayYAML, b.Repo, b.App.Name)
		if err != nil {
			return nil, err
		}
		folder := b.Folder
		if p != nil && p.Folder != "" {
			folder = p.Folder
		}
		key := id()
		s.mu.Lock()
		s.bundles[key] = b
		s.mu.Unlock()
		return Inspection{Key: key, Bundle: b, Profile: p, Folder: folder}, nil
	})
}

// DeployRequest deploys a bundle from Inspect.
type DeployRequest struct {
	Board  string `json:"board"`
	Key    string `json:"key"`
	Folder string `json:"folder"`
	Start  bool   `json:"start"`
	Backup bool   `json:"backup"`
	Force  bool   `json:"force"`
	Setup  bool   `json:"setup"`
}

func (s *Service) Deploy(req DeployRequest) string {
	return s.run("deploy", req.Board, req.Folder, func(ctx context.Context, sess *core.Session, sink core.Sink) (any, error) {
		s.mu.Lock()
		b := s.bundles[req.Key]
		s.mu.Unlock()
		if b == nil {
			return nil, errors.New("the fetched app expired; check it again")
		}
		return sess.Deploy(ctx, sink, b, core.DeployOptions{Folder: req.Folder, Start: req.Start, Backup: req.Backup, Force: req.Force, Setup: req.Setup, BackupDir: config.BackupsDir()})
	})
}

// Update installs commit, the one the user saw in Changes (newer or older), recorded under ref
// (empty: the branch the app came from) so later updates follow that branch.
func (s *Service) Update(boardName, folder, ref, commit string, force bool) string {
	return s.run("update", boardName, folder, func(ctx context.Context, sess *core.Session, sink core.Sink) (any, error) {
		return sess.Update(ctx, sink, config.RepoCacheDir(), folder, ref, core.DeployOptions{Force: force, Backup: true, BackupDir: config.BackupsDir(), Commit: commit})
	})
}

func isBranch(info *source.RemoteInfo, name string) bool {
	for _, r := range info.Refs {
		if r.Kind == "branch" && r.Name == name {
			return true
		}
	}
	return false
}

// UpdateChanges is what updating an app to a branch or tag would change, shown before updating.
type UpdateChanges struct {
	Refs   *source.RemoteInfo `json:"refs"`   // the repository's branches and tags, to pick another
	Ref    string             `json:"ref"`    // the branch or tag that would be installed
	Commit string             `json:"commit"` // its tip, or the earlier commit asked for ("" when the ref is gone: see Error)
	// History is the branch's latest commits (newest first, GitHub only), to go back to one.
	History []source.Change `json:"history,omitempty"`
	// Compare is what moving from the installed commit to Commit changes; nil when the host
	// cannot tell (only GitHub is asked), with Error saying why.
	Compare *source.Comparison `json:"compare,omitempty"`
	// Removed are the installed commits Commit lacks (newest first), when it goes back.
	Removed []source.Change `json:"removed,omitempty"`
	Error   string          `json:"error,omitempty"` // why Commit or Compare is missing
}

// Changes looks up what ref of repo points to right now (empty or a commit hash: the default
// branch, as Update does), or takes at, an earlier commit of it, and lists the commits between
// base, the installed commit, and that.
func (s *Service) Changes(repo, ref, at, base string) string {
	return s.run("changes", "", "", func(ctx context.Context, _ *core.Session, _ core.Sink) (any, error) {
		info, err := source.ListRefs(ctx, repo)
		if err != nil {
			return nil, err
		}
		out := &UpdateChanges{Refs: info}
		for _, r := range info.Refs {
			if r.Name == ref {
				out.Ref, out.Commit = r.Name, r.Commit
			}
		}
		if out.Commit == "" {
			if ref != "" && !core.LooksLikeCommit(ref) { // the list still lets the user pick another
				out.Ref, out.Error = ref, fmt.Sprintf("%s has no branch or tag %q any more", info.URL, ref)
				return out, nil
			}
			for _, r := range info.Refs {
				if r.Kind == "branch" && r.Name == info.DefaultBranch {
					out.Ref, out.Commit = r.Name, r.Commit
				}
			}
			if out.Commit == "" {
				return nil, fmt.Errorf("%s has no branches", info.URL)
			}
		}
		if isBranch(info, out.Ref) {
			// Keyed by the tip, so a branch that moves on is asked again.
			if v, err := s.remember("history "+repo+" "+out.Commit, time.Hour, func() (any, error) { return source.History(ctx, repo, out.Commit, 30) }); err == nil {
				out.History = v.([]source.Change)
			}
		}
		if at != "" {
			if !core.LooksLikeCommit(at) {
				return nil, fmt.Errorf("%q is not a commit", at)
			}
			out.Commit = at
		}
		if base == "" || base == out.Commit {
			return out, nil
		}
		// Commits between two fixed hashes never change.
		v, err := s.remember("compare "+repo+" "+base+" "+out.Commit, time.Hour, func() (any, error) { return source.Compare(ctx, repo, base, out.Commit) })
		if err != nil {
			out.Error = err.Error()
			return out, nil
		}
		out.Compare = v.(*source.Comparison)
		if out.Compare.Behind > 0 {
			if v, err := s.remember("compare "+repo+" "+out.Commit+" "+base, time.Hour, func() (any, error) { return source.Compare(ctx, repo, out.Commit, base) }); err == nil {
				out.Removed = v.(*source.Comparison).Commits
			}
		}
		return out, nil
	})
}

func (s *Service) Start(boardName, folder string) string {
	return s.run("start", boardName, folder, func(ctx context.Context, sess *core.Session, sink core.Sink) (any, error) {
		return nil, sess.Start(ctx, sink, folder)
	})
}

func (s *Service) Stop(boardName, folder string, force bool) string {
	return s.run("stop", boardName, folder, func(ctx context.Context, sess *core.Session, sink core.Sink) (any, error) {
		return nil, sess.Stop(ctx, sink, folder, force)
	})
}

func (s *Service) Restart(boardName, folder string, force bool) string {
	return s.run("restart", boardName, folder, func(ctx context.Context, sess *core.Session, sink core.Sink) (any, error) {
		return nil, sess.Restart(ctx, sink, folder, force)
	})
}

// Autostart sets the boot app ("" = none).
func (s *Service) Autostart(boardName, folder string) string {
	return s.run("autostart", boardName, folder, func(ctx context.Context, sess *core.Session, sink core.Sink) (any, error) {
		return nil, sess.Autostart(ctx, sink, folder)
	})
}

func (s *Service) Prepare(boardName, folder string, revert, force bool) string {
	kind := "prepare"
	if revert {
		kind = "revert"
	}
	return s.run(kind, boardName, folder, func(ctx context.Context, sess *core.Session, sink core.Sink) (any, error) {
		return nil, sess.Prepare(ctx, sink, folder, revert, force)
	})
}

// Mirror sets ("" = restores) the board's PyPI mirror.
// Mirror sets the PyPI and Debian mirrors; both empty restores the original configuration.
func (s *Service) Mirror(boardName string, m core.MirrorOptions) string {
	return s.run("mirror", boardName, "", func(ctx context.Context, sess *core.Session, sink core.Sink) (any, error) {
		return nil, sess.Mirror(ctx, sink, m)
	})
}

func (s *Service) Backup(boardName, folder string) string {
	return s.run("backup", boardName, folder, func(ctx context.Context, sess *core.Session, sink core.Sink) (any, error) {
		return sess.Backup(ctx, sink, folder, config.BackupsDir())
	})
}

func (s *Service) Restore(boardName, folder, file string, force bool) string {
	return s.run("restore", boardName, folder, func(ctx context.Context, sess *core.Session, sink core.Sink) (any, error) {
		return nil, sess.Restore(ctx, sink, folder, file, force)
	})
}

func (s *Service) Export(boardName, folder, path string) string {
	return s.run("export", boardName, folder, func(ctx context.Context, sess *core.Session, sink core.Sink) (any, error) {
		f, err := os.Create(path)
		if err != nil {
			return nil, err
		}
		err = sess.Export(ctx, sink, folder, f)
		f.Close()
		if err != nil {
			os.Remove(path)
			return nil, err
		}
		return path, nil
	})
}

// EraseRequest mirrors core.EraseOptions.
type EraseRequest struct {
	Board        string `json:"board"`
	Folder       string `json:"folder"`
	Backup       bool   `json:"backup"`
	Force        bool   `json:"force"`
	RevertSystem bool   `json:"revertSystem"`
}

func (s *Service) Erase(req EraseRequest) string {
	return s.run("erase", req.Board, req.Folder, func(ctx context.Context, sess *core.Session, sink core.Sink) (any, error) {
		return sess.Erase(ctx, sink, req.Folder, core.EraseOptions{Backup: req.Backup, Force: req.Force, RevertSystem: req.RevertSystem, BackupDir: config.BackupsDir()})
	})
}

// Logs streams an app's logs until cancelled (when follow is set).
func (s *Service) Logs(boardName, folder string, follow bool, tail int) string {
	return s.run("logs", boardName, folder, func(ctx context.Context, sess *core.Session, sink core.Sink) (any, error) {
		return nil, sess.Logs(ctx, sink, folder, follow, tail)
	})
}

// UpdateInfo says whether an installed app's branch has moved on.
type UpdateInfo struct {
	Folder string `json:"folder"`
	Repo   string `json:"repo"`
	Ref    string `json:"ref,omitempty"`
	Latest string `json:"latest"`
	Behind bool   `json:"behind"`
	// Commits is how many commits the installed version is behind (-1 when unknown).
	Commits int `json:"commits"`
	// Subject and Date describe the latest commit (GitHub only; empty elsewhere).
	Subject string    `json:"subject,omitempty"`
	Date    time.Time `json:"date,omitempty"`
	// Unknown: the app was not installed by Quay, so its version is not known; the profile names
	// the repository it comes from, and reinstalling through Quay makes updates trackable.
	Unknown bool `json:"unknown,omitempty"`
}

type cached struct {
	at time.Time
	v  any
}

// remember caches slow lookups (git ls-remote, GitHub API) between the 30-second refreshes.
func (s *Service) remember(key string, ttl time.Duration, fn func() (any, error)) (any, error) {
	s.mu.Lock()
	c, ok := s.cache[key]
	s.mu.Unlock()
	if ok && time.Since(c.at) < ttl {
		return c.v, nil
	}
	v, err := fn()
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.cache[key] = cached{time.Now(), v}
	s.mu.Unlock()
	return v, nil
}

// CheckUpdates compares each app with the latest commit of the branch it came from (no board
// access): Quay-installed apps by their record, others by their profile's source repository.
func (s *Service) CheckUpdates(apps []core.App) string {
	return s.run("check-updates", "", "", func(ctx context.Context, _ *core.Session, _ core.Sink) (any, error) {
		out := []UpdateInfo{}
		for _, a := range apps {
			u := UpdateInfo{Folder: a.Folder, Commits: -1}
			switch {
			case a.Deploy != nil && a.Deploy.Repo != "" && !strings.HasPrefix(a.Deploy.Repo, "local:"):
				u.Repo, u.Ref = a.Deploy.Repo, a.Deploy.Ref
			case a.Deploy == nil && a.Match != nil && a.Match.Source != nil && a.Match.Source.Repo != "":
				u.Repo, u.Ref, u.Unknown = a.Match.Source.Repo, a.Match.Source.Ref, true
			default:
				continue
			}
			v, err := s.remember("ref "+u.Repo+"@"+u.Ref, 5*time.Minute, func() (any, error) { return source.RemoteCommit(ctx, u.Repo, u.Ref) })
			if err != nil || v.(string) == "" {
				continue
			}
			u.Latest = v.(string)
			base := ""
			if !u.Unknown {
				base = a.Deploy.Commit
				u.Behind = u.Latest != base
				if !u.Behind {
					u.Commits = 0
				}
			}
			if d, err := s.remember("commit "+u.Repo+" "+u.Latest+" "+base, time.Hour, func() (any, error) { return source.DescribeCommit(ctx, u.Repo, u.Latest, base) }); err == nil {
				c := d.(*source.Commit)
				u.Subject, u.Date = c.Subject, c.Date
				if c.Ahead >= 0 {
					u.Commits = c.Ahead
				}
			}
			out = append(out, u)
		}
		return out, nil
	})
}
