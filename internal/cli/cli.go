// Package cli is Quay's command line. It renders core events on a terminal.
package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/crypto/ssh"
	"golang.org/x/term"

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
	"github.com/caoxg/quay/internal/source"
	"github.com/caoxg/quay/internal/sysprofile"
	"github.com/caoxg/quay/internal/version"
)

var (
	flagBoard string
	flagHost  string
	flagJSON  bool
)

// Execute runs the CLI with os.Args.
func Execute(gui func() error) int {
	root := &cobra.Command{
		Use:           "quay",
		Short:         "Install, update and back up Arduino Apps on an Arduino® UNO Q from git",
		Long:          "Quay deploys Arduino Apps from any git repository to an Arduino® UNO Q over SSH or USB,\nupdates them in place without losing data/, and backs them up. Run without arguments to open the app.",
		SilenceUsage:  true,
		SilenceErrors: true,
		Version:       version.Version,
	}
	isTTY = term.IsTerminal(int(os.Stdout.Fd())) && enableANSI(os.Stdout)
	hub.Default = &hub.Loader{Dir: config.ProfileCacheDir(), Manual: profilesManual}
	root.PersistentFlags().StringVarP(&flagBoard, "board", "b", "", "saved board to use (default: the default board)")
	root.PersistentFlags().StringVarP(&flagHost, "host", "H", "", "board address instead of a saved board: user@host[:port], adb or adb:SERIAL")
	root.PersistentFlags().BoolVar(&flagJSON, "json", false, "print machine-readable JSON events")
	root.PersistentPreRun = func(cmd *cobra.Command, _ []string) {
		logTag, logApp = "["+cmd.CommandPath()+"]", cmd.Name() == "logs"
		applog.Default.Printf("%s begin", logTag) // the arguments stay out: some carry passwords
	}

	root.AddCommand(boardCmd(), statusCmd(), refsCmd(), deployCmd(), updateCmd(),
		simpleCmd("start", "Start an app", func(ctx context.Context, s *core.Session, sink core.Sink, app string, force bool) error {
			return s.Start(ctx, sink, app)
		}),
		simpleCmd("stop", "Stop an app", func(ctx context.Context, s *core.Session, sink core.Sink, app string, force bool) error {
			return s.Stop(ctx, sink, app, force)
		}),
		simpleCmd("restart", "Restart an app", func(ctx context.Context, s *core.Session, sink core.Sink, app string, force bool) error {
			return s.Restart(ctx, sink, app, force)
		}),
		autostartCmd(), logsCmd(), prepareCmd(), mirrorCmd(), relayCmd(), wifiCmd(), hotspotCmd(), upgradeCmd(), renameCmd(), passwdCmd(), sshCmd(), uploadKeyCmd(),
		backupCmd(), restoreCmd(), exportCmd(), eraseCmd(),
		policyCmd(), openCmd(), shellCmd(), systemCmd(), profilesCmd(), selfUpdateCmd(), licensesCmd())
	if gui != nil {
		root.AddCommand(&cobra.Command{Use: "gui", Short: "Open the Quay app", RunE: func(*cobra.Command, []string) error { return gui() }})
	}
	cmd, err := root.ExecuteC()
	if err == nil {
		updateNotice(cmd)
	}
	if logTag != "" {
		if err != nil {
			applog.Default.Printf("%s end fail: %v", logTag, err)
		} else {
			applog.Default.Printf("%s end ok", logTag)
		}
	}
	if err != nil {
		if errors.Is(err, context.Canceled) {
			fmt.Fprintln(os.Stderr, "cancelled")
			return 130
		}
		msg := err.Error()
		var unsafe *core.UnsafeError
		if errors.As(err, &unsafe) {
			msg += " (use --force to override)"
		}
		fmt.Fprintln(os.Stderr, red("error: ")+msg)
		return 1
	}
	return 0
}

// ---------------------------------------------------------------- output

// isTTY is decided in Execute (not at init) so the Windows GUI build can attach its parent's
// console first.
var isTTY bool

func paint(code, s string) string {
	if !isTTY || os.Getenv("NO_COLOR") != "" {
		return s
	}
	return "\033[" + code + "m" + s + "\033[0m"
}
func red(s string) string    { return paint("31", s) }
func green(s string) string  { return paint("32", s) }
func yellow(s string) string { return paint("33", s) }
func cyan(s string) string   { return paint("36", s) }
func dim(s string) string    { return paint("2", s) }
func bold(s string) string   { return paint("1", s) }

type printer struct {
	mu      sync.Mutex
	running string // text of the step being shown on the current line
	eta     eta
}

// logTag names the running command in Quay's log; logApp is set for `quay logs`, whose output is
// the app's own log and stays out of Quay's.
var (
	logTag string
	logApp bool
)

func (p *printer) sink(e core.Event) {
	if !(logApp && e.Type == "log") {
		core.Record(logTag, e)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if flagJSON {
		b, _ := json.Marshal(e)
		fmt.Println(string(b))
		return
	}
	clear := func() {
		if p.running != "" && isTTY {
			fmt.Print("\r\033[K")
		} else if p.running != "" {
			fmt.Println()
		}
		p.running = ""
	}
	switch e.Type {
	case "step":
		switch e.State {
		case "run":
			clear()
			fmt.Print(cyan("• ") + e.Text + dim(" …"))
			p.running = e.Text
		case "ok":
			clear()
			fmt.Println(green("✓ ") + e.Text)
		case "skip":
			clear()
			fmt.Println(dim("– " + e.Text + " (skipped)"))
		case "fail":
			clear()
			fmt.Println(red("✗ ") + e.Text)
		}
	case "log":
		if p.running != "" {
			if isTTY {
				fmt.Print("\r\033[K")
			} else {
				fmt.Println()
			}
		}
		fmt.Println(dim("  │ ") + e.Text)
		if p.running != "" {
			fmt.Print(cyan("• ") + p.running + dim(" …"))
		}
	case "progress":
		pr, ok := e.Data.(core.Progress)
		if !ok || p.running == "" || !isTTY {
			return
		}
		fmt.Print("\r\033[K" + cyan("• ") + p.running + dim(fmt.Sprintf(" … %3.0f%%%s", pr.Percent, p.eta.left(e.Step, pr.Percent))))
	case "warn":
		clear()
		fmt.Println(yellow("! ") + e.Text)
	case "info":
		clear()
		fmt.Println(cyan("ℹ ") + e.Text)
	}
}

// ---------------------------------------------------------------- prompts

type tty struct{}

func (tty) Secret(ctx context.Context, title, message string) (string, bool, error) {
	in, out, err := openTTY()
	if err != nil {
		return "", false, fmt.Errorf("%s: no terminal to ask for a password", title)
	}
	defer in.Close()
	defer out.Close()
	fmt.Fprint(out, "\r\033[K"+bold(message)+" ")
	b, err := term.ReadPassword(int(in.Fd()))
	fmt.Fprintln(out)
	if err != nil {
		return "", false, err
	}
	return string(b), true, nil
}

func (tty) TrustHostKey(ctx context.Context, host, keyType, fp string) (bool, error) {
	ans, err := ask(fmt.Sprintf("First connection to %s.\n  %s key fingerprint: %s\nTrust this board? [y/N] ", host, keyType, fp))
	if err != nil {
		return false, err
	}
	return strings.EqualFold(ans, "y") || strings.EqualFold(ans, "yes"), nil
}

func ask(q string) (string, error) {
	in, out, err := openTTY()
	if err != nil {
		return "", errors.New("no terminal to confirm on; pass --yes")
	}
	defer in.Close()
	defer out.Close()
	fmt.Fprint(out, "\r\033[K"+q)
	line, err := bufio.NewReader(in).ReadString('\n')
	return strings.TrimSpace(line), err
}

// ---------------------------------------------------------------- plumbing

func signalContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

func target() (board.Target, error) {
	if flagHost != "" {
		return board.ParseTarget(flagHost)
	}
	cfg, err := config.Load()
	if err != nil {
		return board.Target{}, err
	}
	name := flagBoard
	if name == "" {
		name = os.Getenv("QUAY_BOARD")
	}
	if name == "" {
		name = cfg.Default
	}
	if name == "" {
		return board.Target{}, errors.New("no board configured: quay board scan, then quay board add duck.local (or pass --host)")
	}
	t, ok := cfg.Board(name)
	if !ok {
		return board.Target{}, fmt.Errorf("no saved board %q (quay board ls)", name)
	}
	return t, nil
}

func withSession(fn func(ctx context.Context, s *core.Session, sink core.Sink) error) error {
	ctx, cancel := signalContext()
	defer cancel()
	t, err := target()
	if err != nil {
		return err
	}
	s, err := core.Connect(ctx, t, tty{}, config.KnownHostsFile())
	if err != nil {
		return err
	}
	defer s.Close()
	p := &printer{}
	err = fn(ctx, s, p.sink)
	p.sink(core.Event{Type: "noop"})
	return err
}

// learn keeps the saved board in step with the board itself: its name in Quay is its hostname, and
// its serial number and address are remembered (config.Learn).
func learn(s *core.Session, st *core.Status) {
	if flagHost != "" || st == nil {
		return
	}
	cfg, err := config.Load()
	if err != nil {
		return
	}
	old := s.Target.Name
	name, changed := cfg.Learn(old, st.Hostname, st.Serial, st.PrimaryIP())
	if !changed {
		return
	}
	if t, ok := cfg.Board(name); ok && !strings.EqualFold(t.Host, s.Target.Host) {
		s.TrustAs(t.Host, config.KnownHostsFile())
	}
	if cfg.Save() == nil && !strings.EqualFold(name, old) && !flagJSON {
		fmt.Println(dim("(the saved board " + old + " is now called " + name + ", the board's own name)"))
	}
}

// relayIfInstalled starts relaying the board's downloads through this computer when the board has
// the relay hook (quay relay on).
func relayIfInstalled(ctx context.Context, s *core.Session) {
	if out, err := board.Output(ctx, s.T, "test -f /etc/apt/apt.conf.d/80quay-relay && echo yes || true"); err != nil || strings.TrimSpace(out) != "yes" {
		return
	}
	cfg, _ := config.Load()
	explicit := ""
	if cfg != nil {
		explicit = cfg.RelayUpstream
	}
	up, err := relay.Upstream(explicit)
	if err != nil {
		fmt.Fprintln(os.Stderr, yellow("! ")+"relay: "+err.Error())
		return
	}
	if err := s.StartRelay(&relay.Proxy{Upstream: up}); err != nil {
		fmt.Fprintln(os.Stderr, yellow("! ")+"relay: "+err.Error())
		return
	}
	if !flagJSON {
		via := "directly"
		if up != nil {
			via = "via " + up.String()
		}
		fmt.Println(cyan("ℹ ") + "the board downloads through this computer (" + via + ")")
	}
}

// ---------------------------------------------------------------- commands

func boardCmd() *cobra.Command {
	c := &cobra.Command{Use: "board", Short: "Manage saved boards"}
	var key string
	var makeDefault, uploadKey bool
	add := &cobra.Command{
		Use:   "add ADDRESS",
		Short: "Save a board, named after its hostname (ADDRESS: host, user@host[:port], adb or adb:SERIAL)",
		Long: "Connects to the board and saves it under the board's own name (its hostname, as App Lab and\n" +
			"NAME.local show it). Adding a board that is already saved updates its address; added over USB, it\n" +
			"keeps its network address, and the other way round (see quay board via).",
		Example: "  quay board scan\n  quay board add duck.local\n  quay board add arduino@192.168.1.42\n  quay board add adb",
		Args:    cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, a []string) error {
			t, err := board.ParseTarget(a[0])
			if err != nil {
				return err
			}
			t.KeyFile = key
			ctx, cancel := signalContext()
			defer cancel()
			s, err := core.Connect(ctx, t, tty{}, config.KnownHostsFile())
			if err != nil {
				return err
			}
			defer s.Close()
			st, err := s.Status(ctx)
			if err != nil {
				return err
			}
			cfg, err := config.Load()
			if err != nil {
				return err
			}
			name, _, joined := cfg.Adopt(t, st.Hostname, st.Serial)
			name, _ = cfg.Learn(name, st.Hostname, st.Serial, st.PrimaryIP())
			if makeDefault {
				cfg.Default = name
			}
			if err := cfg.Save(); err != nil {
				return err
			}
			if uploadKey {
				path, line, err := board.LocalPublicKey(t.KeyFile)
				if err != nil {
					if path, line, err = board.GenerateKey(); err == nil {
						fmt.Println(dim("created " + strings.TrimSuffix(path, ".pub") + " (no SSH key on this computer yet)"))
					}
				}
				if err != nil {
					fmt.Fprintln(os.Stderr, yellow("! ")+err.Error())
				} else if added, err := s.UploadKey(ctx, nil, line); err != nil {
					fmt.Fprintln(os.Stderr, yellow("! ")+"upload "+path+": "+err.Error())
				} else if added {
					fmt.Println(dim("uploaded " + path))
				}
			}
			saved, _ := cfg.Board(name)
			model := st.Model
			if model == "" {
				model = "unknown board"
			}
			fmt.Printf("saved %s → %s  %s%s\n", bold(name), saved.Ways(), dim(model+", serial "+st.Serial),
				map[bool]string{true: dim("  (default)")}[strings.EqualFold(cfg.Default, name)])
			if joined {
				fmt.Println(dim("Quay now reaches " + name + " both ways: the network first, USB when the network does not answer (quay board via)"))
			}
			return nil
		},
	}
	add.Flags().StringVar(&key, "key", "", "SSH private key file")
	add.Flags().BoolVar(&uploadKey, "upload-key", false, "also upload your SSH public key (~/.ssh/id_*.pub; created if there is none)")
	add.Flags().BoolVar(&makeDefault, "default", false, "make it the default board")
	ls := &cobra.Command{Use: "ls", Short: "List saved boards", RunE: func(*cobra.Command, []string) error {
		cfg, err := config.Load()
		if err != nil {
			return err
		}
		if len(cfg.Boards) == 0 {
			fmt.Println("no boards yet: quay board add duck arduino@duck.local")
			return nil
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		for _, b := range cfg.Boards {
			mark := " "
			if strings.EqualFold(b.Name, cfg.Default) {
				mark = "*"
			}
			fmt.Fprintf(w, "%s %s\t%s\n", mark, b.Name, b.Ways())
		}
		return w.Flush()
	}}
	rm := &cobra.Command{Use: "rm NAME", Short: "Forget a board", Args: cobra.ExactArgs(1), RunE: func(_ *cobra.Command, a []string) error {
		cfg, err := config.Load()
		if err != nil {
			return err
		}
		if !cfg.Remove(a[0]) {
			return fmt.Errorf("no saved board %q", a[0])
		}
		return cfg.Save()
	}}
	def := &cobra.Command{Use: "default NAME", Short: "Set the default board", Args: cobra.ExactArgs(1), RunE: func(_ *cobra.Command, a []string) error {
		cfg, err := config.Load()
		if err != nil {
			return err
		}
		t, ok := cfg.Board(a[0])
		if !ok {
			return fmt.Errorf("no saved board %q", a[0])
		}
		cfg.Default = t.Name
		return cfg.Save()
	}}
	devices := &cobra.Command{Use: "usb", Short: "List boards connected over USB (adb)", RunE: func(cmd *cobra.Command, _ []string) error {
		list, err := board.ADBDevices(cmd.Context())
		if err != nil {
			return err
		}
		if len(list) == 0 {
			fmt.Println("no USB devices")
		}
		for _, d := range list {
			fmt.Printf("%s\t%s\n", d.Serial, d.State)
		}
		return nil
	}}
	scan := &cobra.Command{Use: "scan", Short: "Find boards on the local network", Args: cobra.NoArgs, RunE: func(*cobra.Command, []string) error {
		ctx, cancel := signalContext()
		defer cancel()
		found, err := discover.Browse(ctx, 2500*time.Millisecond)
		if err != nil {
			return err
		}
		if flagJSON {
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			return enc.Encode(found)
		}
		if len(found) == 0 {
			fmt.Println("no boards found on this network (boards on USB: quay board usb)")
			return nil
		}
		cfg, _ := config.Load()
		w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(w, dim("NAME\tADDRESS\tIP\tSERIAL\tMODEL\t"))
		for _, b := range found {
			saved := ""
			if cfg != nil {
				if t, ok := cfg.ByBoardID(b.Serial); ok {
					saved = dim("saved as " + t.Name)
				}
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", bold(b.Name), b.Address(), strings.Join(b.IPs, " "), b.Serial, b.Model, saved)
		}
		_ = w.Flush()
		fmt.Println(dim("add one with: quay board add " + found[0].Address()))
		return nil
	}}
	via := &cobra.Command{
		Use:   "via NAME auto|network|usb",
		Short: "Choose how Quay reaches a board it knows both ways",
		Long: "auto (the default) tries the network first, then USB when the network does not answer and the\n" +
			"board is plugged in; network or usb uses that way only.",
		Args: cobra.ExactArgs(2),
		RunE: func(_ *cobra.Command, a []string) error {
			v, ok := map[string]string{"auto": "", "network": board.KindSSH, "wifi": board.KindSSH, "ssh": board.KindSSH, "usb": board.KindADB, "adb": board.KindADB}[strings.ToLower(a[1])]
			if !ok {
				return fmt.Errorf("choose auto, network or usb, not %q", a[1])
			}
			cfg, err := config.Load()
			if err != nil {
				return err
			}
			t, ok := cfg.Board(a[0])
			if !ok {
				return fmt.Errorf("no saved board %q", a[0])
			}
			t.Via = v
			if err := t.CheckWays(); err != nil {
				return err
			}
			cfg.Put(t)
			if err := cfg.Save(); err != nil {
				return err
			}
			fmt.Printf("%s → %s\n", bold(t.Name), t.Ways())
			return nil
		},
	}
	c.AddCommand(add, scan, ls, rm, def, devices, via)
	return c
}

func statusCmd() *cobra.Command {
	return &cobra.Command{Use: "status", Short: "Show the board and its apps", Args: cobra.NoArgs, RunE: func(*cobra.Command, []string) error {
		return withSession(func(ctx context.Context, s *core.Session, _ core.Sink) error {
			st, err := s.Status(ctx)
			if err != nil {
				return err
			}
			if flagJSON {
				enc := json.NewEncoder(os.Stdout)
				enc.SetIndent("", "  ")
				return enc.Encode(st)
			}
			printStatus(st)
			learn(s, st)
			return nil
		})
	}}
}

func printStatus(st *core.Status) {
	model := st.Model
	if model == "" {
		model = "unknown board"
	}
	fmt.Printf("%s  %s\n", bold(st.Hostname), dim(model+"  ·  "+st.Target))
	var ips []string
	for _, ip := range st.IPs {
		ips = append(ips, ip.IP+" ("+ip.Iface+")")
	}
	if len(ips) > 0 {
		fmt.Printf("  network   %s\n", strings.Join(ips, ", "))
	}
	if st.Serial != "" {
		fmt.Printf("  serial    %s\n", st.Serial)
	}
	if st.AppCLI {
		fmt.Printf("  app cli   %s\n", st.AppCLIVersion)
	} else {
		fmt.Printf("  app cli   %s\n", red("not found"))
	}
	if st.Mirror.Pip != "" {
		fmt.Printf("  pip index %s%s\n", st.Mirror.Pip, map[bool]string{true: dim("  (set by quay)")}[st.Mirror.Managed])
	}
	if st.Mirror.Debian != "" && !strings.Contains(st.Mirror.Debian, "deb.debian.org") {
		fmt.Printf("  debian    %s%s\n", st.Mirror.Debian, map[bool]string{true: dim("  (set by quay)")}[st.Mirror.Managed])
	}
	if st.Relay.Installed {
		fmt.Printf("  relay     %s\n", "downloads go through Quay's computer while it is connected")
	}
	fmt.Println()
	if len(st.Apps) == 0 {
		fmt.Println("  no apps yet — quay deploy <git-repo>")
	}
	for _, a := range st.Apps {
		icon := a.Icon
		if icon == "" {
			icon = "▪"
		}
		stat := a.Status
		switch stat {
		case "running":
			stat = green("● running")
		case "failed":
			stat = red("● failed")
		case "starting", "stopping":
			stat = yellow("● " + stat)
		default:
			stat = dim("○ " + stat)
		}
		extra := ""
		if a.Default {
			extra += cyan("  [runs at boot]")
		}
		fmt.Printf("  %s %s  %s%s\n", icon, bold(a.Name), stat, extra)
		fmt.Printf("     %s\n", dim("~/ArduinoApps/"+a.Folder))
		if a.Deploy != nil {
			c := a.Deploy.Commit
			if len(c) > 7 {
				c = c[:7]
			}
			fmt.Printf("     %s %s  %s\n", dim("from"), a.Deploy.Repo, dim("@ "+a.Deploy.Ref+" "+c+"  "+a.Deploy.Subject))
		}
		if a.Profile != nil {
			p := "profile " + a.Profile.Name + " (" + a.Profile.Origin + ")"
			if a.SystemReady != nil && !*a.SystemReady {
				p += yellow("  — system setup needed: quay prepare " + a.Folder)
			}
			fmt.Printf("     %s\n", dim(p))
		} else if a.Match != nil {
			fmt.Printf("     %s\n", dim("not installed by Quay: a plain app here; deploy it with Quay to use the "+a.Match.Name+" profile"))
		}
		if len(a.Data) > 0 {
			fmt.Printf("     %s %s\n", dim("data/"), strings.Join(a.Data, " "))
		}
		if a.WebURL != "" {
			fmt.Printf("     %s %s\n", dim("console"), a.WebURL)
		}
	}
	for _, b := range st.Broken {
		fmt.Printf("  %s %s: %s\n", red("✗"), b.Name, b.Error)
	}
}

func refsCmd() *cobra.Command {
	return &cobra.Command{Use: "refs REPO", Short: "List a repository's branches and tags", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, a []string) error {
		ctx, cancel := signalContext()
		defer cancel()
		info, err := source.ListRefs(ctx, a[0])
		if err != nil {
			return err
		}
		if flagJSON {
			return json.NewEncoder(os.Stdout).Encode(info)
		}
		for _, r := range info.Refs {
			mark := " "
			if r.Kind == "branch" && r.Name == info.DefaultBranch {
				mark = "*"
			}
			fmt.Printf("%s %-7s %-30s %s\n", mark, r.Kind, r.Name, dim(r.Commit[:7]))
		}
		return nil
	}}
}

func deployCmd() *cobra.Command {
	var ref, subdir, folder string
	var noStart, force, noBackup bool
	c := &cobra.Command{
		Use:     "deploy REPO|DIR",
		Short:   "Install or update an app from a git repository (or a local folder)",
		Long:    "Deploy fetches the app on this computer, uploads it, keeps its data/, runs its quay.yaml\nhooks and starts it. It does not change what runs at boot (see `quay autostart`).",
		Example: "  quay deploy LuwuDynamics/xgoduck_runtime_arduino\n  quay deploy https://github.com/me/app --ref v1.2.0\n  quay deploy ./my-app",
		Args:    cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, a []string) error {
			return withSession(func(ctx context.Context, s *core.Session, sink core.Sink) error {
				f := core.FetchOptions{Repo: a[0], Ref: ref, Subdir: subdir}
				if st, err := os.Stat(a[0]); err == nil && st.IsDir() {
					f = core.FetchOptions{Dir: a[0]}
				}
				b, err := core.Fetch(ctx, sink, config.RepoCacheDir(), f)
				if err != nil {
					return err
				}
				res, err := s.Deploy(ctx, sink, b, core.DeployOptions{Folder: folder, Start: !noStart, Force: force, Backup: !noBackup, BackupDir: config.BackupsDir()})
				if err != nil {
					return err
				}
				done(res)
				return nil
			})
		},
	}
	c.Flags().StringVar(&ref, "ref", "", "branch, tag or commit (default: the default branch)")
	c.Flags().StringVar(&subdir, "subdir", "", "app folder inside the repository")
	c.Flags().StringVar(&folder, "folder", "", "install folder under ~/ArduinoApps")
	c.Flags().BoolVar(&noStart, "no-start", false, "do not start the app")
	c.Flags().BoolVar(&force, "force", false, "ignore the app's safety check")
	c.Flags().BoolVar(&noBackup, "no-backup", false, "skip backing up data/ first")
	return c
}

func done(res *core.DeployResult) {
	if flagJSON || res == nil {
		return
	}
	c := res.Commit
	if len(c) > 7 {
		c = c[:7]
	}
	msg := green("done") + " — " + res.Folder
	if c != "" {
		msg += " at " + c
	}
	if res.Previous != "" && res.Previous != res.Commit {
		msg += dim(" (was " + res.Previous[:min(7, len(res.Previous))] + ")")
	}
	fmt.Println(msg)
	if res.NeedsSetup {
		fmt.Println(yellow("next: ") + "quay prepare " + res.Folder + dim("   (one-time system setup, needs sudo)"))
	}
}

func updateCmd() *cobra.Command {
	var ref string
	var force, noBackup bool
	c := &cobra.Command{
		Use:   "update APP",
		Short: "Update an app from the repository it was installed from",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, a []string) error {
			return withSession(func(ctx context.Context, s *core.Session, sink core.Sink) error {
				res, err := s.Update(ctx, sink, config.RepoCacheDir(), a[0], ref, core.DeployOptions{Force: force, Backup: !noBackup, BackupDir: config.BackupsDir()})
				if err != nil {
					return err
				}
				if res.Previous != res.Commit {
					done(res)
				}
				return nil
			})
		},
	}
	c.Flags().StringVar(&ref, "ref", "", "switch to this branch, tag or commit")
	c.Flags().BoolVar(&force, "force", false, "redeploy even if unchanged; ignore the safety check")
	c.Flags().BoolVar(&noBackup, "no-backup", false, "skip backing up data/ first")
	return c
}

func simpleCmd(name, short string, fn func(context.Context, *core.Session, core.Sink, string, bool) error) *cobra.Command {
	var force bool
	c := &cobra.Command{Use: name + " APP", Short: short, Args: cobra.ExactArgs(1), RunE: func(_ *cobra.Command, a []string) error {
		return withSession(func(ctx context.Context, s *core.Session, sink core.Sink) error { return fn(ctx, s, sink, a[0], force) })
	}}
	if name != "start" {
		c.Flags().BoolVar(&force, "force", false, "ignore the app's safety check")
	}
	return c
}

func autostartCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "autostart APP|none",
		Short:   "Choose the app that runs at boot",
		Example: "  quay autostart xgoduck_runtime_arduino\n  quay autostart none",
		Args:    cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, a []string) error {
			return withSession(func(ctx context.Context, s *core.Session, sink core.Sink) error { return s.Autostart(ctx, sink, a[0]) })
		},
	}
}

func logsCmd() *cobra.Command {
	var follow bool
	var tail int
	c := &cobra.Command{Use: "logs APP", Short: "Show an app's logs", Args: cobra.ExactArgs(1), RunE: func(_ *cobra.Command, a []string) error {
		ctx, cancel := signalContext()
		defer cancel()
		t, err := target()
		if err != nil {
			return err
		}
		s, err := core.Connect(ctx, t, tty{}, config.KnownHostsFile())
		if err != nil {
			return err
		}
		defer s.Close()
		return s.Logs(ctx, func(e core.Event) {
			if e.Type == "log" {
				fmt.Println(e.Text)
			}
		}, a[0], follow, tail)
	}}
	c.Flags().BoolVarP(&follow, "follow", "f", false, "keep streaming")
	c.Flags().IntVar(&tail, "tail", 200, "lines of history")
	return c
}

func prepareCmd() *cobra.Command {
	var revert, force bool
	c := &cobra.Command{
		Use:   "prepare APP",
		Short: "Apply the app's one-time system setup (sudo)",
		Long:  "Runs the `system` section of the app's quay.yaml as root — e.g. XGO-Duck sets the router\nserial link to 2 Mbps. --revert undoes it.",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, a []string) error {
			return withSession(func(ctx context.Context, s *core.Session, sink core.Sink) error {
				return s.Prepare(ctx, sink, a[0], revert, force)
			})
		},
	}
	c.Flags().BoolVar(&revert, "revert", false, "undo the system setup")
	c.Flags().BoolVar(&force, "force", false, "ignore the app's safety check")
	return c
}

func mirrorCmd() *cobra.Command {
	var use, index, debian string
	var noPyPI, noDebian bool
	c := &cobra.Command{
		Use:   "mirror cn|off|test|list",
		Short: "Use mirrors in mainland China for PyPI and Debian packages",
		Long: "`cn` points the board's pip and uv at a PyPI mirror, and apt's Debian packages (most of a system\n" +
			"update) at a Debian mirror. By default it first tests the mirrors from the board and takes the\n" +
			"fastest; apt also gets the next fastest ones and then the official server to fall back to when\n" +
			"a mirror fails, and pip one extra index. `--use` picks a mirror yourself (`quay mirror list`).\n" +
			"`test` only measures, `off` restores the original configuration. Needs sudo, except test and list.\n" +
			"Arduino's own packages have no mirror; see `quay relay`.",
		Example: "  quay mirror test\n  quay mirror cn\n  quay mirror cn --use ustc\n  quay mirror cn --no-debian\n" +
			"  quay mirror cn --index https://pypi.example.cn/simple --debian https://debian.example.cn/debian\n  quay mirror off",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, a []string) error {
			mode := strings.ToLower(a[0])
			switch mode {
			case "list":
				w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
				for _, m := range mirror.China {
					fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", bold(m.ID), m.Name, dim(m.Debian), dim(m.PyPI))
				}
				return w.Flush()
			case "test":
				return withSession(func(ctx context.Context, s *core.Session, sink core.Sink) error {
					rs, err := s.TestMirrors(ctx, sink, append(append([]mirror.Mirror{}, mirror.China...), mirror.Official))
					sink(core.Event{Type: "noop"})
					if err != nil {
						return err
					}
					printMirrorResults(rs)
					return nil
				})
			case "off":
				return withSession(func(ctx context.Context, s *core.Session, sink core.Sink) error {
					return s.Mirror(ctx, sink, core.MirrorOptions{})
				})
			case "cn":
			default:
				return errors.New("use `quay mirror cn`, `off`, `test` or `list`")
			}
			if noPyPI && noDebian {
				return errors.New("nothing to do: both mirrors are turned off")
			}
			return withSession(func(ctx context.Context, s *core.Session, sink core.Sink) error {
				var picked []mirror.Mirror
				switch {
				case strings.EqualFold(use, "auto"):
					rs, err := s.TestMirrors(ctx, sink, mirror.China)
					if err != nil {
						return err
					}
					if picked = mirror.Pick(rs, 3); len(picked) == 0 {
						return errors.New("no mirror answered from the board; check its network, or pick one with --use")
					}
					sink(core.Event{Type: "info", Text: "fastest from the board: " + picked[0].Name})
				default:
					m, ok := mirror.ByID(use)
					if !ok || m.ID == mirror.Official.ID {
						return fmt.Errorf("unknown mirror %q (quay mirror list)", use)
					}
					picked = []mirror.Mirror{m}
					for _, o := range mirror.China { // fallbacks without a test: the next ones in the list
						if o.ID != m.ID && len(picked) < 3 {
							picked = append(picked, o)
						}
					}
				}
				m := core.MirrorOptions{PyPI: picked[0].PyPI, Debian: picked[0].Debian}
				for _, o := range picked[1:] {
					m.DebianFallback = append(m.DebianFallback, o.Debian)
				}
				if len(picked) > 1 {
					m.PyPIFallback = picked[1].PyPI
				}
				if cmd.Flags().Changed("index") {
					m.PyPI, m.PyPIFallback = index, ""
				}
				if cmd.Flags().Changed("debian") {
					m.Debian = debian
				}
				if noPyPI {
					m.PyPI, m.PyPIFallback = "", ""
				}
				if noDebian {
					m.Debian, m.DebianFallback = "", nil
				}
				return s.Mirror(ctx, sink, m)
			})
		},
	}
	c.Flags().StringVar(&use, "use", "auto", "mirror to use: auto (fastest from the board) or an id from `quay mirror list`")
	c.Flags().StringVar(&index, "index", "", "a PyPI mirror URL of your own")
	c.Flags().StringVar(&debian, "debian", "", "a Debian mirror URL of your own (security updates at URL-security)")
	c.Flags().BoolVar(&noPyPI, "no-pypi", false, "leave pip and uv alone")
	c.Flags().BoolVar(&noDebian, "no-debian", false, "leave apt alone")
	return c
}

func printMirrorResults(rs []mirror.Result) {
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, dim("MIRROR\tDEBIAN\tLATENCY\tPYPI\t"))
	for _, r := range rs {
		name := r.ID
		if m, ok := mirror.ByID(r.ID); ok {
			name = m.Name
		}
		deb, lat, pypi := "", "", ""
		if d := r.Debian; d != nil {
			switch {
			case !d.OK:
				deb = red("failed")
			case !d.Security:
				deb = yellow(mirror.Speed(d.Speed) + " (no security)")
			default:
				deb = mirror.Speed(d.Speed)
			}
			if d.OK {
				lat = fmt.Sprintf("%d ms", d.MS)
			}
		}
		if p := r.PyPI; p != nil {
			if p.OK {
				pypi = fmt.Sprintf("%d ms", p.MS)
			} else {
				pypi = red("failed")
			}
		}
		mark := "  "
		if r.Works() && r.ID != mirror.Official.ID && name != "" {
			mark = green("✓ ")
		}
		fmt.Fprintf(w, "%s%s\t%s\t%s\t%s\t\n", mark, name, deb, lat, pypi)
	}
	_ = w.Flush()
}

func uploadKeyCmd() *cobra.Command {
	var pub string
	var yes bool
	c := &cobra.Command{
		Use:   "upload-key",
		Short: "Upload your SSH public key to the board, so logins need no password (like ssh-copy-id)",
		Long: "Appends ~/.ssh/id_ed25519.pub (or id_ecdsa.pub, id_rsa.pub, or the board's --key file + .pub)\n" +
			"to the board's ~/.ssh/authorized_keys, unless it is there already. On a computer without an SSH\n" +
			"key it offers to create ~/.ssh/id_ed25519 first, as ssh-keygen would (no passphrase).",
		Example: "  quay upload-key\n  quay upload-key --pub ~/.ssh/work.pub",
		Args:    cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			return withSession(func(ctx context.Context, s *core.Session, sink core.Sink) error {
				path, line, err := board.LocalPublicKey(s.Target.KeyFile)
				if pub != "" {
					path, line, err = board.LocalPublicKey(pub)
				} else if err != nil {
					if !yes {
						ans, aerr := ask("This computer has no SSH key yet. Create ~/.ssh/id_ed25519 (as ssh-keygen would, no passphrase) and upload it? [y/N] ")
						if aerr != nil {
							return aerr
						}
						if !strings.EqualFold(ans, "y") && !strings.EqualFold(ans, "yes") {
							return errors.New("cancelled")
						}
					}
					if path, line, err = board.GenerateKey(); err == nil && !flagJSON {
						fmt.Println(green("✓ ") + "created " + strings.TrimSuffix(path, ".pub"))
					}
				}
				if err != nil {
					return err
				}
				added, err := s.UploadKey(ctx, sink, line)
				sink(core.Event{Type: "noop"})
				if err == nil && !flagJSON {
					if added {
						fmt.Println(green("✓ ") + "uploaded " + path + " to " + s.T.Describe())
					} else {
						fmt.Println(green("✓ ") + path + " was already on " + s.T.Describe())
					}
				}
				return err
			})
		},
	}
	c.Flags().StringVar(&pub, "pub", "", "public key file to upload")
	c.Flags().BoolVarP(&yes, "yes", "y", false, "create a key without asking when there is none")
	return c
}

func relayCmd() *cobra.Command {
	var upstream string
	c := &cobra.Command{
		Use:   "relay on|off",
		Short: "Let the board download through this computer while Quay is connected",
		Long: "Installs a small apt hook on the board (needs sudo): while Quay is connected — during\n" +
			"`quay upgrade`, and whenever the Quay app is open — the board's package downloads go through\n" +
			"this computer, and through its proxy if it has one (system settings or --upstream). Otherwise\n" +
			"the board downloads directly, as before. `off` removes the hook.",
		Example: "  quay relay on\n  quay relay on --upstream http://127.0.0.1:7890\n  quay relay off",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, a []string) error {
			mode := strings.ToLower(a[0])
			if mode != "on" && mode != "off" {
				return errors.New("use `quay relay on` or `quay relay off`")
			}
			cfg, err := config.Load()
			if err != nil {
				return err
			}
			if cmd.Flags().Changed("upstream") {
				if p, d := relay.Check(upstream); p != "" {
					return fmt.Errorf("proxy %q: %s (%s); give host:port, or an http://, https:// or socks5:// URL, of a proxy that works from this computer", upstream, p, d)
				}
				cfg.RelayUpstream = upstream
				if err := cfg.Save(); err != nil {
					return err
				}
			}
			return withSession(func(ctx context.Context, s *core.Session, sink core.Sink) error {
				if err := s.Relay(ctx, sink, mode == "on"); err != nil || mode == "off" {
					return err
				}
				relayIfInstalled(ctx, s)
				out, err := board.Output(ctx, s.T, "/usr/local/lib/quay/apt-proxy")
				if err == nil && strings.HasPrefix(strings.TrimSpace(out), "http://127.0.0.1:") {
					fmt.Println(green("✓ ") + "checked: apt on the board reaches this computer")
				} else {
					fmt.Println(yellow("! ") + "the hook is installed, but the board cannot reach this computer yet: " + strings.TrimSpace(out))
				}
				return nil
			})
		},
	}
	c.Flags().StringVar(&upstream, "upstream", "", "proxy on this computer to chain through, e.g. http://127.0.0.1:7890 (empty: system settings)")
	return c
}

func renameCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "rename NAME",
		Short: "Rename the board (its hostname: NAME.local, and the name App Lab shows)",
		Long: "Renames the board the way App Lab does (arduino-app-cli system set-name). Its network name\n" +
			"becomes NAME.local, and Quay's saved board follows.",
		Example: "  quay rename goose\n  quay -b duck rename goose",
		Args:    cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, a []string) error {
			return withSession(func(ctx context.Context, s *core.Session, sink core.Sink) error {
				if err := s.Rename(ctx, sink, a[0]); err != nil {
					return err
				}
				st, err := s.Status(ctx)
				if err != nil {
					return err
				}
				learn(s, st)
				return nil
			})
		},
	}
}

func sshCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "ssh [on]",
		Short: "Show whether the board's SSH server is on; on: turn it on, as App Lab's first setup does",
		Long: "A board that never had App Lab's first setup has no SSH server and no usable password: reach it\n" +
			"over USB (quay -H adb …), set a password (quay passwd), save its Wi-Fi (quay wifi add) and run\n" +
			"quay ssh on. Quay trusts the host keys it reads over USB, so the first SSH login asks nothing.",
		Example:   "  quay -H adb ssh\n  quay -H adb ssh on\n  quay board add duck.local",
		Args:      cobra.MaximumNArgs(1),
		ValidArgs: []string{"on"},
		RunE: func(_ *cobra.Command, a []string) error {
			if len(a) == 1 && a[0] != "on" {
				return fmt.Errorf("unknown argument %q (only: on)", a[0])
			}
			return withSession(func(ctx context.Context, s *core.Session, sink core.Sink) error {
				st, err := s.Status(ctx)
				if err != nil {
					return err
				}
				if len(a) == 0 {
					if st.Setup == nil {
						return errors.New("the board did not say")
					}
					fmt.Printf("SSH server: %s (%s at boot)\nPassword:   %s\n", map[bool]string{true: "on", false: "off"}[st.Setup.SSHActive],
						map[bool]string{true: "starts", false: "does not start"}[st.Setup.SSHEnabled], passwordState(st.Setup.Password))
					if st.Setup.Needed() {
						fmt.Println("This board has not had its first setup: quay passwd, then quay ssh on.")
					}
					return nil
				}
				keys, err := s.EnableSSH(ctx, sink)
				if err != nil {
					return err
				}
				host := st.Hostname + ".local"
				if err := board.PinHostKeys(config.KnownHostsFile(), host, 22, keys); err != nil {
					return err
				}
				fmt.Printf("SSH is on: ssh arduino@%s (Quay trusts its host keys). quay board add %s saves it.\n", host, host)
				return nil
			})
		},
	}
	return c
}

func passwordState(p string) string {
	switch p {
	case "P":
		return "set"
	case "L":
		return "locked (none usable)"
	case "NP":
		return "none"
	}
	return "unknown"
}

func passwdCmd() *cobra.Command {
	var stdin bool
	c := &cobra.Command{
		Use:   "passwd",
		Short: "Set the board's password (no current password needed over USB)",
		Long: "Sets the password of the board's user (arduino), for logging in and for sudo, with the board\n" +
			"image's own arduino-passwd — the way App Lab's first setup does. It needs no current password, so\n" +
			"over USB it also works on a board whose password nobody knows.",
		Example: "  quay -H adb passwd\n  printf '%s\\n' \"$PW\" | quay passwd --password-stdin",
		Args:    cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			pw, err := readNewPassword(stdin)
			if err != nil {
				return err
			}
			return withSession(func(ctx context.Context, s *core.Session, sink core.Sink) error {
				return s.SetPassword(ctx, sink, pw)
			})
		},
	}
	c.Flags().BoolVar(&stdin, "password-stdin", false, "read the new password from standard input")
	return c
}

// readNewPassword reads a new board password from standard input, or twice from the terminal.
func readNewPassword(stdin bool) (string, error) {
	if stdin {
		line, err := bufio.NewReader(os.Stdin).ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return "", err
		}
		pw := strings.TrimRight(line, "\r\n")
		return pw, core.ValidBoardPassword(pw)
	}
	pw, ok, err := tty{}.Secret(context.Background(), "New password", "New password for the board:")
	if err != nil || !ok {
		return "", orCanceled(err)
	}
	if err := core.ValidBoardPassword(pw); err != nil {
		return "", err
	}
	again, ok, err := tty{}.Secret(context.Background(), "New password", "Again:")
	if err != nil || !ok {
		return "", orCanceled(err)
	}
	if again != pw {
		return "", errors.New("the two passwords differ")
	}
	return pw, nil
}

func orCanceled(err error) error {
	if err != nil {
		return err
	}
	return context.Canceled
}

// rememberSkipDesktop saves the choice for the saved board t is (a board reached ad hoc is not).
func rememberSkipDesktop(t board.Target, skip bool) {
	cfg, err := config.Load()
	if err != nil {
		return
	}
	b, ok := cfg.ByBoardID(t.BoardID)
	if !ok || t.BoardID == "" {
		if b, ok = cfg.Board(t.Name); !ok {
			return
		}
	}
	if b.SkipDesktop != skip {
		b.SkipDesktop = skip
		cfg.Put(b)
		_ = cfg.Save()
	}
}

func upgradeCmd() *cobra.Command {
	var check, arduinoOnly, skipDesktop, withDesktop, yes, force, noRefresh, downloadOnly bool
	c := &cobra.Command{
		Use:   "upgrade",
		Short: "Update the board's system packages",
		Long: "Lists the pending updates of the board's Debian and Arduino packages, then installs them with\n" +
			"the board's own `arduino-app-cli system update`. --arduino-only is the update App Lab runs: the\n" +
			"arduino-* packages. Everything goes in two rounds, App Lab's packages first, so the apt rules they\n" +
			"bring (Arduino's pins) already decide the rest. The job keeps running on the board if the\n" +
			"connection drops; running quay upgrade again follows it.\n\n" +
			"Faster: `quay mirror cn` moves the Debian downloads to a mirror, `quay relay on` sends the board's\n" +
			"downloads through this computer, and --skip-desktop leaves out the on-board App Lab and Chromium,\n" +
			"which a board used from a computer does not need. Quay remembers that for the board: later\n" +
			"updates leave them out too, until --with-desktop brings them back.\n\n" +
			"Quay first downloads the packages into the board's cache, then installs them. --download-only\n" +
			"stops after the download (running apps are not touched); a later quay upgrade installs them\n" +
			"without downloading again.",
		Example: "  quay upgrade --check\n  quay upgrade --skip-desktop\n  quay upgrade --download-only\n  quay upgrade --arduino-only --yes",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if skipDesktop && withDesktop {
				return errors.New("--skip-desktop and --with-desktop contradict each other")
			}
			return withSession(func(ctx context.Context, s *core.Session, sink core.Sink) error {
				// The board's remembered choice, unless a flag makes (and remembers) another.
				if cmd.Flags().Changed("skip-desktop") || withDesktop {
					rememberSkipDesktop(s.Target, skipDesktop)
				} else {
					skipDesktop = s.Target.SkipDesktop
				}
				relayIfInstalled(ctx, s)
				u, err := s.CheckUpgrades(ctx, sink, !noRefresh)
				if err != nil {
					return err
				}
				sink(core.Event{Type: "noop"})
				if flagJSON && check {
					b, _ := json.Marshal(u)
					fmt.Println(string(b))
					return nil
				}
				if !flagJSON {
					printUpgrades(u, core.UpgradeOptions{ArduinoOnly: arduinoOnly, SkipDesktop: skipDesktop})
				}
				if check || u.Total.Count == 0 {
					return nil
				}
				if !yes {
					q := "Install the updates now? [y/N] "
					if downloadOnly {
						q = "Download the updates now? [y/N] "
					}
					ans, err := ask(q)
					if err != nil {
						return err
					}
					if !strings.EqualFold(ans, "y") && !strings.EqualFold(ans, "yes") {
						return errors.New("cancelled")
					}
				}
				opt := core.UpgradeOptions{ArduinoOnly: arduinoOnly, SkipDesktop: skipDesktop, Force: force, DownloadOnly: downloadOnly}
				res, err := s.Upgrade(ctx, sink, opt)
				if err != nil {
					return err
				}
				sink(core.Event{Type: "noop"})
				// The update changed apt's version rules, which change more packages: asked, not done.
				if res.Rules != nil && !flagJSON {
					fmt.Println(yellow("! ") + "The update changed apt's version rules:")
					for _, f := range res.Rules.Files {
						if f.Package != "" {
							fmt.Printf("    %s %s\n", f.Path, dim("(from "+f.Package+")"))
						} else {
							fmt.Println("    " + f.Path)
						}
					}
					fmt.Println("  Under them, these packages change too:")
					for _, p := range res.Rules.Packages {
						line := fmt.Sprintf("    %s %s → %s", p.Name, p.From, p.To)
						if p.Downgrade {
							line += yellow("  downgrade")
						}
						fmt.Println(line)
					}
					again := yes
					if !again {
						ans, err := ask("Install them now? [y/N] ")
						if err != nil {
							return err
						}
						again = strings.EqualFold(ans, "y") || strings.EqualFold(ans, "yes")
					}
					if !again {
						fmt.Println(dim("left as they are; quay upgrade installs them later"))
					} else if res, err = s.Upgrade(ctx, sink, opt); err != nil {
						return err
					}
					sink(core.Event{Type: "noop"})
				}
				if !flagJSON && res.Downloaded {
					fmt.Println(green("downloaded") + " — run quay upgrade again to install")
				} else if !flagJSON {
					msg := green("done") + " — " + res.AppCLIVersion
					if res.Remaining > 0 {
						msg += dim(fmt.Sprintf("  (%d updates left out)", res.Remaining))
					}
					fmt.Println(msg)
				}
				return nil
			})
		},
	}
	c.Flags().BoolVar(&check, "check", false, "only list the updates")
	c.Flags().BoolVar(&arduinoOnly, "arduino-only", false, "only Arduino's packages, as App Lab's update (arduino-*: App CLI, router, …)")
	c.Flags().BoolVar(&skipDesktop, "skip-desktop", false, "leave out the on-board App Lab and Chromium, now and in later updates of this board")
	c.Flags().BoolVar(&withDesktop, "with-desktop", false, "include the on-board desktop again (undoes --skip-desktop for this board)")
	c.Flags().BoolVarP(&yes, "yes", "y", false, "do not ask for confirmation")
	c.Flags().BoolVar(&force, "force", false, "ignore the running apps' safety checks")
	c.Flags().BoolVar(&noRefresh, "no-refresh", false, "use the package lists as they are (no apt-get update)")
	c.Flags().BoolVar(&downloadOnly, "download-only", false, "only download the packages into the board's cache; install later")
	return c
}

func printUpgrades(u *core.Upgrades, opt core.UpgradeOptions) {
	if u.Total.Count == 0 {
		fmt.Println(green("✓ ") + "the board is up to date" + dim("  ("+u.AppCLIVersion+")"))
		return
	}
	fmt.Printf("%s, %s to download\n", bold(fmt.Sprintf("%d updates", u.Total.Count)), mb(u.Total.Size))
	for _, g := range u.Sources {
		fmt.Printf("  %-24s %4d packages  %8s\n", g.Host, g.Count, mb(g.Size))
	}
	var arduino []string
	for _, p := range u.Packages {
		if p.Arduino && p.From != "" {
			arduino = append(arduino, p.Name+" "+p.From+" → "+p.To)
		}
	}
	sort.Strings(arduino)
	if len(arduino) > 0 {
		fmt.Println("  Arduino:  " + strings.Join(arduino, ", "))
	}
	if u.Desktop.Count > 0 {
		hint := "--skip-desktop leaves them out"
		if opt.SkipDesktop {
			hint = "left out for this board; --with-desktop brings them back"
		}
		fmt.Printf("  Desktop (App Lab, Chromium): %d packages, %s  %s\n", u.Desktop.Count, mb(u.Desktop.Size), dim(hint))
	}
	if p := u.Plans[opt.PlanKey()]; p != nil && (opt.ArduinoOnly || opt.SkipDesktop) {
		fmt.Printf("  to install: %d packages, %s\n", p.Count, mb(p.Size))
	}
	if len(u.Running) > 0 {
		fmt.Println(yellow("  running apps may restart: ") + strings.Join(u.Running, ", "))
	}
	if u.RebootRequired {
		fmt.Println(yellow("  a restart of the board is pending from an earlier update"))
	}
}

func mb(n int64) string { return fmt.Sprintf("%.0f MB", float64(n)/1048576) }

func backupCmd() *cobra.Command {
	var dir string
	c := &cobra.Command{Use: "backup APP", Short: "Copy an app's data/ to this computer", Args: cobra.ExactArgs(1), RunE: func(_ *cobra.Command, a []string) error {
		return withSession(func(ctx context.Context, s *core.Session, sink core.Sink) error {
			p, err := s.Backup(ctx, sink, a[0], dir)
			if err == nil && p != "" && !flagJSON {
				fmt.Println(p)
			}
			return err
		})
	}}
	c.Flags().StringVarP(&dir, "output", "o", config.BackupsDir(), "folder for the backup")
	return c
}

func restoreCmd() *cobra.Command {
	var force bool
	c := &cobra.Command{Use: "restore APP FILE", Short: "Put a backup back into an app's data/", Args: cobra.ExactArgs(2), RunE: func(_ *cobra.Command, a []string) error {
		return withSession(func(ctx context.Context, s *core.Session, sink core.Sink) error {
			return s.Restore(ctx, sink, a[0], a[1], force)
		})
	}}
	c.Flags().BoolVar(&force, "force", false, "restore a backup from another board; ignore the safety check")
	return c
}

func exportCmd() *cobra.Command {
	var out string
	c := &cobra.Command{Use: "export APP", Short: "Save the app with its data as an App Lab zip", Args: cobra.ExactArgs(1), RunE: func(_ *cobra.Command, a []string) error {
		if out == "" {
			out = a[0] + "-" + time.Now().Format("20060102-150405") + ".zip"
		}
		f, err := os.Create(out)
		if err != nil {
			return err
		}
		err = withSession(func(ctx context.Context, s *core.Session, sink core.Sink) error { return s.Export(ctx, sink, a[0], f) })
		f.Close()
		if err != nil {
			os.Remove(out)
			return err
		}
		abs, _ := filepath.Abs(out)
		fmt.Println(abs)
		return nil
	}}
	c.Flags().StringVarP(&out, "output", "o", "", "zip file (default: APP-DATE.zip)")
	return c
}

func eraseCmd() *cobra.Command {
	var yes, noBackup, force, revertSystem bool
	c := &cobra.Command{
		Use:   "erase APP",
		Short: "Remove an app from the board (asks first)",
		Long:  "Stops the app, backs up data/, removes its containers and caches (arduino-app-cli app destroy),\nclears it as the boot app, runs its uninstall hook and deletes the folder.",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, a []string) error {
			return withSession(func(ctx context.Context, s *core.Session, sink core.Sink) error {
				st, err := s.Status(ctx)
				if err != nil {
					return err
				}
				app, err := st.App(a[0])
				if err != nil {
					return err
				}
				if !yes {
					fmt.Printf("\n%s %s %s\n", red("This will remove"), bold(app.Name), "from "+st.Hostname+":")
					fmt.Printf("  folder    ~/ArduinoApps/%s\n", app.Folder)
					if app.Deploy != nil {
						fmt.Printf("  version   %s @ %s %.7s\n", app.Deploy.Repo, app.Deploy.Ref, app.Deploy.Commit)
					}
					if len(app.Data) > 0 {
						b := "will be backed up first"
						if noBackup {
							b = red("will be LOST (--no-backup)")
						}
						fmt.Printf("  data/     %s — %s\n", strings.Join(app.Data, " "), b)
					}
					if app.Default {
						fmt.Println("  boot      it runs at boot; no app will after this")
					}
					ans, err := ask(fmt.Sprintf("Type the folder name (%s) to confirm: ", app.Folder))
					if err != nil {
						return err
					}
					if ans != app.Folder {
						return errors.New("not confirmed — nothing was changed")
					}
				}
				backup, err := s.Erase(ctx, sink, app.Folder, core.EraseOptions{Backup: !noBackup, BackupDir: config.BackupsDir(), Force: force, RevertSystem: revertSystem})
				if err == nil && !flagJSON {
					fmt.Println(green("erased ") + app.Folder)
					if backup != "" {
						fmt.Println(dim("data backup: ") + backup)
					}
				}
				return err
			})
		},
	}
	c.Flags().BoolVarP(&yes, "yes", "y", false, "do not ask for confirmation")
	c.Flags().BoolVar(&noBackup, "no-backup", false, "do not back up data/")
	c.Flags().BoolVar(&force, "force", false, "ignore the app's safety check")
	c.Flags().BoolVar(&revertSystem, "revert-system", false, "also undo the app's system setup (sudo)")
	return c
}

func shellCmd() *cobra.Command {
	return &cobra.Command{Use: "shell", Short: "Open a shell on the board", Args: cobra.NoArgs, RunE: func(*cobra.Command, []string) error {
		t, err := target()
		if err != nil {
			return err
		}
		ctx, cancel := signalContext()
		defer cancel()
		s, err := core.Connect(ctx, t, tty{}, config.KnownHostsFile())
		if err != nil {
			return err
		}
		defer s.Close()
		if a, ok := s.T.(*board.ADB); ok {
			c := a.Command("shell")
			c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
			return c.Run()
		}
		sshT, ok := s.T.(*board.SSH)
		if !ok {
			return errors.New("shell needs SSH or USB")
		}
		return interactive(sshT.Client())
	}}
}

func interactive(c *ssh.Client) error {
	sess, err := c.NewSession()
	if err != nil {
		return err
	}
	defer sess.Close()
	fd := int(os.Stdin.Fd())
	if term.IsTerminal(fd) {
		w, h, _ := term.GetSize(fd)
		kind := os.Getenv("TERM")
		if kind == "" {
			kind = "xterm-256color"
		}
		if err := sess.RequestPty(kind, h, w, ssh.TerminalModes{ssh.ECHO: 1}); err != nil {
			return err
		}
		stop := watchResize(fd, func(w, h int) { _ = sess.WindowChange(h, w) })
		defer stop()
		old, err := term.MakeRaw(fd)
		if err != nil {
			return err
		}
		defer term.Restore(fd, old)
	}
	sess.Stdin, sess.Stdout, sess.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := sess.Shell(); err != nil {
		return err
	}
	err = sess.Wait()
	var ee *ssh.ExitError
	if errors.As(err, &ee) || errors.Is(err, io.EOF) {
		return nil
	}
	return err
}

func openCmd() *cobra.Command {
	var printOnly bool
	c := &cobra.Command{Use: "open APP", Short: "Open the app's own web console in the browser", Args: cobra.ExactArgs(1),
		Long: "Opens the console the app serves on the board: the port in its quay.yaml `web` section, else\nthe first port listed in its app.yaml.",
		RunE: func(cmd *cobra.Command, a []string) error {
			return withSession(func(ctx context.Context, s *core.Session, _ core.Sink) error {
				st, err := s.Status(ctx)
				if err != nil {
					return err
				}
				app, err := st.App(a[0])
				if err != nil {
					return err
				}
				if app.WebURL == "" {
					return fmt.Errorf("%s has no web console (no ports in app.yaml, no web section in quay.yaml)", app.Name)
				}
				if app.Status != "running" && app.Status != "starting" {
					fmt.Fprintln(os.Stderr, yellow("! ")+app.Name+" is "+app.Status+"; start it first: quay start "+app.Folder)
				}
				fmt.Println(app.WebURL)
				if printOnly {
					return nil
				}
				return openBrowser(app.WebURL)
			})
		}}
	c.Flags().BoolVar(&printOnly, "print", false, "only print the URL")
	return c
}

// openBrowser opens a URL with the desktop's default browser.
func openBrowser(u string) error {
	var c *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		c = exec.Command("open", u)
	case "windows":
		c = exec.Command("rundll32", "url.dll,FileProtocolHandler", u)
	default:
		c = exec.Command("xdg-open", u)
	}
	return c.Start()
}

func licensesCmd() *cobra.Command {
	return &cobra.Command{Use: "licenses", Short: "Show Quay's license and the licenses of the software it includes", Args: cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			fmt.Println(legal.Notice)
			fmt.Println()
			fmt.Print(legal.ThirdParty)
			return nil
		}}
}

func profilesCmd() *cobra.Command {
	var refresh bool
	var auto string
	c := &cobra.Command{
		Use:   "profiles",
		Short: "List Quay's app and system profiles (from the quay-profiles repository)",
		Long: "App profiles tell Quay what an app needs when the app has no quay.yaml of its own; system profiles\n" +
			"recognise board systems other than the stock image (Duck OS, …) and what they do differently.\n" +
			"Both come from github.com/caoxg/quay-profiles, independently of Quay releases: Quay has a copy\n" +
			"built in and fetches a newer one about twice a day (--auto off: only when asked). --refresh\n" +
			"fetches now.",
		Example: "  quay profiles\n  quay profiles --refresh\n  quay profiles --auto off",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if cmd.Flags().Changed("auto") {
				if auto != "on" && auto != "off" {
					return errors.New("--auto on or --auto off")
				}
				cfg, err := config.Load()
				if err != nil {
					return err
				}
				cfg.ProfilesManual = auto == "off"
				if err := cfg.Save(); err != nil {
					return err
				}
			}
			if refresh {
				ctx, cancel := signalContext()
				defer cancel()
				if err := hub.Default.Refresh(ctx); err != nil {
					fmt.Println(yellow("! ") + "could not fetch the profiles: " + err.Error())
				}
			}
			snap := hub.Get()
			how := "fetched by themselves about twice a day"
			if profilesManual() {
				how = "fetched only when asked (quay profiles --refresh)"
			}
			fmt.Println(dim("profiles of " + snap.Index.Updated + ", " + snap.Source + "; " + how))
			fmt.Println(bold("App profiles"))
			for _, p := range profile.Latest() {
				fmt.Printf("  %s  %s\n", bold(p.Name), dim("("+p.ID+")"))
				if p.Description != "" {
					fmt.Printf("    %s\n", p.Description)
				}
				if p.Source != nil {
					fmt.Printf("    %s %s\n", dim("repo"), p.Source.Repo)
				}
				var from []string
				for _, v := range profile.Builtins() {
					if v.ID == p.ID && v.From != "" {
						from = append(from, v.From[:7])
					}
				}
				if len(from) > 0 {
					fmt.Printf("    %s %s\n", dim("versions from commits"), strings.Join(from, ", "))
				}
				if p.System != nil {
					fmt.Printf("    %s %s\n", dim("system"), firstLine(p.System.Description))
				}
			}
			set := sysprofile.FromSnapshot(snap)
			fmt.Println(bold("System profiles"))
			for _, p := range set.Profiles {
				fmt.Printf("  %s  %s\n", bold(p.Name), dim("("+p.ID+")"))
				if p.Description != "" {
					fmt.Printf("    %s\n", p.Description)
				}
			}
			for _, pr := range set.Problems {
				fmt.Println(yellow("! ") + pr)
			}
			return nil
		},
	}
	c.Flags().BoolVar(&refresh, "refresh", false, "fetch the newest profiles first")
	c.Flags().StringVar(&auto, "auto", "", "on: fetch newer profiles by themselves (the default); off: only when asked")
	return c
}

// profilesManual is the setting that stops Quay fetching newer profiles by itself.
func profilesManual() bool {
	cfg, err := config.Load()
	return err == nil && cfg.ProfilesManual
}

func systemCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "system [fix ID]",
		Short: "Show which system the board runs and what a system profile says about it",
		Long: "Recognises board systems other than the stock Arduino image (Duck OS, …) with Quay's system\n" +
			"profiles (quay profiles), and lists what they do differently. `fix ID` applies the built-in\n" +
			"action a finding offers (needs sudo).",
		Example: "  quay system\n  quay system fix duck-setup-hotspot",
		Args:    cobra.MaximumNArgs(2),
		RunE: func(_ *cobra.Command, a []string) error {
			if len(a) > 0 && (a[0] != "fix" || len(a) != 2) {
				return errors.New("use `quay system` or `quay system fix ID`")
			}
			return withSession(func(ctx context.Context, s *core.Session, sink core.Sink) error {
				if len(a) == 2 {
					return s.SystemFix(ctx, sink, a[1])
				}
				st, err := s.Status(ctx)
				if err != nil {
					return err
				}
				n, err := s.Network(ctx)
				if err != nil {
					return err
				}
				if flagJSON {
					return printJSON(map[string]any{"system": st.System, "findings": append(st.Findings, n.Findings...)})
				}
				if st.System == nil {
					fmt.Println("system: " + bold(orDash(st.OS.Pretty)) + dim("  (no system profile recognises it: the stock image, or one Quay does not know)"))
					return nil
				}
				d := st.System
				line := bold(strings.TrimSpace(d.Name + " " + d.Version))
				if d.Base != "" {
					line += dim("  based on Arduino's image " + d.Base)
				}
				fmt.Println("system: " + line)
				if d.Vendor != "" {
					fmt.Println(dim("  by " + d.Vendor + "  " + d.Homepage))
				}
				for _, f := range append(st.Findings, n.Findings...) {
					fmt.Println(yellow("! ") + f.Text)
					if f.Action != nil {
						fmt.Printf("  %s %s\n", dim(f.Action.Label+":"), "quay system fix "+f.ID)
					}
				}
				return nil
			})
		},
	}
	return c
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.Index(s, ". "); i > 0 {
		return s[:i+1]
	}
	return s
}

// eta estimates the time left of a step from its progress so far.
type eta struct {
	step  string
	start time.Time
	from  float64
}

func (e *eta) left(step string, percent float64) string {
	now := time.Now()
	if step != e.step {
		*e = eta{step: step, start: now, from: percent}
		return ""
	}
	done := percent - e.from
	el := now.Sub(e.start)
	if done < 1 || el < 3*time.Second || percent >= 100 {
		return ""
	}
	rest := time.Duration(float64(el) / done * (100 - percent)).Round(time.Second)
	if rest >= time.Minute {
		return fmt.Sprintf(" · about %d min left", int(rest.Minutes()+0.5))
	}
	return fmt.Sprintf(" · %ds left", int(rest.Seconds()))
}
