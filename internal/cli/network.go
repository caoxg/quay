package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/caoxg/quay/internal/core"
)

func wifiCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "wifi",
		Short: "List, add and remove the Wi-Fi networks the board joins",
		Long: "The board joins an ordinary saved Wi-Fi whenever one is in range. A network saved as a phone\n" +
			"hotspot (--phone) is joined only as the away plan says (quay hotspot). Needs sudo, except ls and scan.",
		Example: "  quay wifi\n  quay wifi scan\n  quay wifi add home\n  quay wifi add \"Wei's iPhone\" --phone\n  quay wifi set \"Wei's iPhone\" --phone=false\n  quay wifi rm \"Wei's iPhone\"",
		Args:    cobra.NoArgs,
		RunE:    func(*cobra.Command, []string) error { return wifiList() },
	}
	ls := &cobra.Command{Use: "ls", Short: "List the saved Wi-Fi networks", Args: cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error { return wifiList() }}
	scan := &cobra.Command{Use: "scan", Short: "List the Wi-Fi networks in range", Args: cobra.NoArgs, RunE: func(*cobra.Command, []string) error {
		return withSession(func(ctx context.Context, s *core.Session, sink core.Sink) error {
			seen, err := s.WifiScan(ctx)
			if err != nil || flagJSON {
				if err == nil {
					err = printJSON(seen)
				}
				return err
			}
			if len(seen) == 0 {
				fmt.Println(dim("no Wi-Fi in range (the board cannot scan while it runs a hotspot)"))
			}
			w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
			for _, n := range seen {
				fmt.Fprintf(w, "%s\t%d%%\t%s\n", n.SSID, n.Signal, dim(orDash(n.Security)))
			}
			return w.Flush()
		})
	}}
	var hidden, open, stdin, phone bool
	add := &cobra.Command{
		Use:     "add NAME",
		Short:   "Save a Wi-Fi network (asks for its password)",
		Example: "  quay wifi add home\n  quay wifi add \"Wei's iPhone\" --phone\n  echo secret | quay wifi add home --password-stdin\n  quay wifi add cafe --open",
		Args:    cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, a []string) error {
			if err := core.ValidSSID(a[0]); err != nil {
				return err
			}
			psk := ""
			if !open {
				var err error
				if psk, err = readPSK(stdin, "Password for "+a[0]+":"); err != nil {
					return err
				}
			}
			return withSession(func(ctx context.Context, s *core.Session, sink core.Sink) error {
				return s.WifiAdd(ctx, sink, a[0], psk, hidden, phone)
			})
		},
	}
	add.Flags().BoolVar(&phone, "phone", false, "a phone's hotspot: joined only as the away plan says (quay hotspot)")
	add.Flags().BoolVar(&hidden, "hidden", false, "the network does not announce its name")
	add.Flags().BoolVar(&open, "open", false, "the network has no password")
	add.Flags().BoolVar(&stdin, "password-stdin", false, "read the password from standard input")
	var setPhone bool
	set := &cobra.Command{
		Use:     "set NAME --phone[=false]",
		Short:   "Mark a saved network as a phone hotspot, or as an ordinary Wi-Fi",
		Example: "  quay wifi set \"Wei's iPhone\" --phone\n  quay wifi set office --phone=false",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, a []string) error {
			if !cmd.Flags().Changed("phone") {
				return errors.New("say --phone or --phone=false")
			}
			return withProfile(a[0], func(ctx context.Context, s *core.Session, sink core.Sink, p core.WifiProfile) error {
				return s.WifiPhone(ctx, sink, p.UUID, setPhone)
			})
		},
	}
	set.Flags().BoolVar(&setPhone, "phone", false, "a phone's hotspot (false: an ordinary Wi-Fi)")
	connect := &cobra.Command{Use: "connect NAME", Short: "Switch the board to a saved network now (Quay loses the board unless this computer is on it too)",
		Example: "  quay wifi connect \"Wei's iPhone\"   # try the phone hotspot at home; turn it off to send the board back", Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, a []string) error {
			return withProfile(a[0], func(ctx context.Context, s *core.Session, sink core.Sink, p core.WifiProfile) error {
				return s.WifiConnect(ctx, sink, p.UUID)
			})
		}}
	rm := &cobra.Command{Use: "rm NAME", Short: "Remove a saved Wi-Fi network (not the one in use)", Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, a []string) error {
			return withProfile(a[0], func(ctx context.Context, s *core.Session, sink core.Sink, p core.WifiProfile) error {
				return s.WifiRemove(ctx, sink, p.UUID)
			})
		}}
	c.AddCommand(ls, scan, add, set, connect, rm)
	return c
}

// withProfile finds a saved network by name or id and runs fn on it.
func withProfile(name string, fn func(ctx context.Context, s *core.Session, sink core.Sink, p core.WifiProfile) error) error {
	return withSession(func(ctx context.Context, s *core.Session, sink core.Sink) error {
		n, err := s.Network(ctx)
		if err != nil {
			return err
		}
		var match []core.WifiProfile
		for _, p := range n.Profiles {
			if p.UUID == name || p.SSID == name || p.ID == name {
				match = append(match, p)
			}
		}
		switch len(match) {
		case 0:
			return fmt.Errorf("no saved Wi-Fi %q (quay wifi ls)", name)
		case 1:
			return fn(ctx, s, sink, match[0])
		}
		return fmt.Errorf("%q is saved more than once; name one by its uuid (quay --json wifi ls)", name)
	})
}

func wifiList() error {
	return withSession(func(ctx context.Context, s *core.Session, sink core.Sink) error {
		n, err := s.Network(ctx)
		if err != nil || flagJSON {
			if err == nil {
				err = printJSON(n)
			}
			return err
		}
		if !n.NMCLI {
			return errors.New("the board has no NetworkManager (nmcli)")
		}
		if len(n.Profiles) == 0 {
			fmt.Println(dim("no saved Wi-Fi"))
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		for _, p := range n.Profiles {
			mark, notes := "  ", []string{}
			if p.Active {
				mark = green("* ")
				notes = append(notes, "in use")
			}
			if p.Phone {
				notes = append(notes, "phone hotspot")
			} else if !p.Autoconnect {
				notes = append(notes, "not joined automatically")
			}
			if p.Mode == "ap" {
				notes = append(notes, "a hotspot of the board's own")
			}
			if p.Hidden {
				notes = append(notes, "hidden")
			}
			if !p.Secure {
				notes = append(notes, "no password")
			}
			fmt.Fprintf(w, "%s%s\t%s\n", mark, p.SSID, dim(strings.Join(notes, ", ")))
		}
		return w.Flush()
	})
}

// awayHelp says what each away plan does, for `quay hotspot`.
var awayHelp = map[string]string{
	core.AwayOff:   "stays offline",
	core.AwayBoard: "opens its own hotspot",
	core.AwayPhone: "joins a saved phone hotspot",
	core.AwayBoth:  "joins a saved phone hotspot in range, else opens its own hotspot",
}

func hotspotCmd() *cobra.Command {
	var name string
	var newPassword, stdin bool
	c := &cobra.Command{
		Use:   "hotspot [board|phone|both|off|open|close]",
		Short: "Choose how the board stays reachable away from its saved Wi-Fi",
		Long: "The away plan: what the board does when none of its ordinary saved Wi-Fi is in range — out of\n" +
			"the house, say.\n\n" +
			"  board   it opens a hotspot of its own; join it from a phone and open http://" + core.HotspotAddress + "\n" +
			"  phone   it joins a phone's hotspot saved with `quay wifi add NAME --phone`\n" +
			"  both    it joins a phone hotspot in range, else opens its own hotspot\n" +
			"  off     it stays offline (phone hotspots are not joined either)\n\n" +
			"board and both install a small service on the board (needs sudo; it asks for the hotspot's\n" +
			"password the first time). While nobody is on the board's hotspot, the service looks for saved\n" +
			"Wi-Fi every 5 minutes and goes back to it. `open` and `close` switch the board's own hotspot now\n" +
			"(this connection may drop). Without an argument: show the plan.",
		Example: "  quay hotspot both\n  quay hotspot board --name duck-hotspot --new-password\n  quay hotspot open\n  quay hotspot\n  quay hotspot off",
		Args:    cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, a []string) error {
			mode := ""
			if len(a) == 1 {
				mode = strings.ToLower(a[0])
			}
			switch mode {
			case "":
				return hotspotStatus()
			case "open", "close":
				return withSession(func(ctx context.Context, s *core.Session, sink core.Sink) error {
					if err := s.HotspotNow(ctx, sink, mode == "open"); err != nil {
						return err
					}
					if mode == "open" {
						fmt.Println(cyan("ℹ ") + "the board opens its hotspot in a few seconds; this connection to it may drop. Join the hotspot and use http://" + core.HotspotAddress)
					} else {
						fmt.Println(cyan("ℹ ") + "the board closes its hotspot in a few seconds and joins a saved Wi-Fi in range (or opens the hotspot again)")
					}
					return nil
				})
			case core.AwayOff, core.AwayBoard, core.AwayPhone, core.AwayBoth:
			default:
				return errors.New("use `quay hotspot board`, `phone`, `both`, `off`, `open` or `close`")
			}
			return withSession(func(ctx context.Context, s *core.Session, sink core.Sink) error {
				n, err := s.Network(ctx)
				if err != nil {
					return err
				}
				if !n.NMCLI {
					return errors.New("the board has no NetworkManager (nmcli)")
				}
				ssid, psk := "", ""
				if mode == core.AwayBoard || mode == core.AwayBoth {
					if !n.AP {
						return errors.New("this board's Wi-Fi cannot run a hotspot")
					}
					if name != "" || newPassword || stdin || !n.Hotspot.Configured {
						if ssid = name; ssid == "" {
							ssid = n.Hotspot.SSID
						}
						if ssid == "" {
							st, err := s.Status(ctx)
							if err != nil {
								return err
							}
							ssid = st.Hostname
						}
						if newPassword || stdin || !n.Hotspot.Configured {
							if psk, err = readPSK(stdin, "Password for hotspot "+ssid+" (8 or more characters):"); err != nil {
								return err
							}
						}
					}
				}
				if err := s.Away(ctx, sink, mode, ssid, psk); err != nil {
					return err
				}
				phones := 0
				for _, p := range n.Profiles {
					if p.Phone {
						phones++
					}
				}
				if (mode == core.AwayPhone || mode == core.AwayBoth) && phones == 0 {
					fmt.Println(yellow("! ") + "no phone hotspot is saved yet: quay wifi add NAME --phone")
				}
				return nil
			})
		},
	}
	c.Flags().StringVar(&name, "name", "", "the board's hotspot name (default: as before, else the board's name)")
	c.Flags().BoolVar(&newPassword, "new-password", false, "ask for a new hotspot password (asked anyway the first time)")
	c.Flags().BoolVar(&stdin, "password-stdin", false, "read the hotspot password from standard input")
	return c
}

func hotspotStatus() error {
	return withSession(func(ctx context.Context, s *core.Session, sink core.Sink) error {
		n, err := s.Network(ctx)
		if err != nil || flagJSON {
			if err == nil {
				err = printJSON(n)
			}
			return err
		}
		fmt.Printf("away from saved Wi-Fi, the board %s  %s\n", bold(awayHelp[n.Away]), dim("("+n.Away+")"))
		var phones []string
		for _, p := range n.Profiles {
			if p.Phone {
				phones = append(phones, p.SSID)
			}
		}
		if n.Away == core.AwayPhone || n.Away == core.AwayBoth || len(phones) > 0 {
			fmt.Println("phone hotspots: " + orDash(strings.Join(phones, ", ")))
		}
		h := n.Hotspot
		if h.Configured {
			fmt.Printf("the board's hotspot: %q", h.SSID)
			if n.Away == core.AwayBoard || n.Away == core.AwayBoth {
				fmt.Print("  " + dim("("+hotspotNow(h)+")"))
			}
			fmt.Println()
		} else if !n.AP {
			fmt.Println(dim("this board's Wi-Fi cannot run a hotspot"))
		}
		if n.Away != core.AwayBoard && n.Away != core.AwayBoth {
			return nil
		}
		fmt.Println("on the board's hotspot, the board is " + bold(core.HotspotAddress))
		st, err := s.Status(ctx)
		if err != nil {
			return err
		}
		// The installed apps' web consoles (app.yaml ports or their profile), on the hotspot.
		for _, a := range st.Apps {
			if u, err := url.Parse(a.WebURL); err == nil && a.WebURL != "" {
				if p := u.Port(); p != "" {
					u.Host = core.HotspotAddress + ":" + p
				} else {
					u.Host = core.HotspotAddress
				}
				fmt.Printf("  %s  %s\n", a.Name, cyan(u.String()))
			}
		}
		return nil
	})
}

func hotspotNow(h core.HotspotInfo) string {
	if !h.Running {
		return red("the service is not running")
	}
	if h.State == nil {
		return "waiting for Wi-Fi"
	}
	switch h.State.State {
	case "wifi":
		return "on Wi-Fi"
	case "hotspot":
		if h.State.Stations != nil {
			return "open, " + plural(*h.State.Stations, "device")
		}
		return "open"
	case "other-hotspot":
		return "another hotspot is up; leaving it alone"
	case "off":
		return "Wi-Fi is off"
	}
	return "waiting for Wi-Fi"
}

// readPSK reads a Wi-Fi password from the terminal (or standard input) and checks it.
func readPSK(stdin bool, prompt string) (string, error) {
	var psk string
	if stdin {
		line, err := bufio.NewReader(os.Stdin).ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return "", err
		}
		psk = strings.TrimRight(line, "\r\n")
	} else {
		v, ok, err := tty{}.Secret(context.Background(), "Wi-Fi password", prompt)
		if err != nil {
			return "", err
		}
		if !ok {
			return "", context.Canceled
		}
		psk = v
	}
	return psk, core.ValidPSK(psk)
}

func orDash(s string) string {
	if s == "" {
		return "–"
	}
	return s
}

func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return strconv.Itoa(n) + " " + word + "s"
}
