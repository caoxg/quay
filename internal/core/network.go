package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/caoxg/quay/internal/board"
	"github.com/caoxg/quay/internal/remote"
	"github.com/caoxg/quay/internal/sysprofile"
)

// HotspotAddress is the board's address on its own hotspot (NetworkManager's shared mode).
const HotspotAddress = "10.42.0.1"

// NetSummary is the board's Wi-Fi at a glance, part of Status.
type NetSummary struct {
	State      int    `json:"state"`      // NetworkManager state of wlan0 (100 = connected)
	Connection string `json:"connection"` // the active profile's name
	SSID       string `json:"ssid"`
	Hotspot    bool   `json:"hotspot"`            // the board runs a hotspot
	Ours       bool   `json:"ours"`               // ... its own, the one Quay set up
	Away       string `json:"away"`               // the away plan: off | own | phone | both
	Stations   *int   `json:"stations,omitempty"` // devices on the board's own hotspot, when known
}

// Away plans: how the board stays reachable away from its ordinary saved Wi-Fi.
const (
	AwayOff   = "off"   // it does not: stays offline
	AwayBoard = "board" // it opens a hotspot of its own
	AwayPhone = "phone" // it joins a saved phone hotspot
	AwayBoth  = "both"  // it joins a phone hotspot in range, else opens its own
)

// Network is the board's Wi-Fi in detail: the device, saved networks, the away plan and the
// board's own hotspot.
type Network struct {
	Iface    string        `json:"iface"`
	NMCLI    bool          `json:"nmcli"` // NetworkManager is there to manage
	AP       bool          `json:"ap"`    // the Wi-Fi can run a hotspot
	State    int           `json:"state"`
	Active   string        `json:"active"` // uuid of the active profile
	IP       string        `json:"ip"`
	Profiles []WifiProfile `json:"profiles"` // saved Wi-Fi networks (not the board's own hotspot)
	Away     string        `json:"away"`
	Hotspot  HotspotInfo   `json:"hotspot"`
	// System is the board's system when a system profile recognises it, and Findings what that
	// profile says about its networking (e.g. another tool's hotspot that comes up at boot).
	System   *sysprofile.Detected `json:"system,omitempty"`
	Findings []sysprofile.Finding `json:"findings"`
}

// WifiProfile is a saved Wi-Fi network (a NetworkManager profile).
type WifiProfile struct {
	UUID        string `json:"uuid"`
	ID          string `json:"id"`
	SSID        string `json:"ssid"`
	Mode        string `json:"mode"` // infrastructure, or ap for another hotspot
	Hidden      bool   `json:"hidden"`
	Autoconnect bool   `json:"autoconnect"`
	Priority    int    `json:"priority"`
	Secure      bool   `json:"secure"`
	Active      bool   `json:"active"`
	Phone       bool   `json:"phone"` // a phone hotspot: joined only as the away plan says
}

// HotspotInfo is the board's own hotspot: its settings, and what its service is doing.
type HotspotInfo struct {
	Configured bool   `json:"configured"` // a name and password are saved
	Installed  bool   `json:"installed"`  // the service is installed (away plan own or both)
	Enabled    bool   `json:"enabled"`
	Running    bool   `json:"running"`
	SSID       string `json:"ssid"`
	UUID       string `json:"uuid,omitempty"`
	Active     bool   `json:"active"`
	State      *struct {
		State    string `json:"state"` // wifi | hotspot | waiting | other-hotspot | off
		Stations *int   `json:"stations"`
		Since    int64  `json:"since"`
	} `json:"state"`
}

// WifiSeen is a Wi-Fi network in range.
type WifiSeen struct {
	SSID     string `json:"ssid"`
	Signal   int    `json:"signal"`
	Security string `json:"security"`
}

// Network reads the board's Wi-Fi state.
func (s *Session) Network(ctx context.Context) (*Network, error) {
	n, _, _, err := s.network(ctx)
	return n, err
}

// network also returns what the system profiles were evaluated on, and the profile that
// recognised the board (nil for the stock image).
func (s *Session) network(ctx context.Context) (*Network, sysprofile.Facts, *sysprofile.Profile, error) {
	set := sysprofile.Current(ctx)
	exists, keys := sysprofile.Checks(set.Profiles)
	var raw struct {
		Network
		SysFiles map[string]sysprofile.FileFact `json:"sysFiles"`
	}
	if err := s.python(ctx, remote.WithChecks(remote.Network, exists, keys), &raw); err != nil {
		return nil, sysprofile.Facts{}, nil, fmt.Errorf("network: %w", err)
	}
	n := raw.Network
	if n.Profiles == nil {
		n.Profiles = []WifiProfile{}
	}
	facts := sysprofile.Facts{Files: raw.SysFiles, Profiles: []sysprofile.NMProfile{}}
	for _, p := range n.Profiles {
		facts.Profiles = append(facts.Profiles, sysprofile.NMProfile{UUID: p.UUID, ID: p.ID, SSID: p.SSID, Mode: p.Mode, Autoconnect: p.Autoconnect, Active: p.Active})
	}
	n.Findings = []sysprofile.Finding{}
	p := sysprofile.Identify(set.Profiles, facts)
	if p != nil {
		n.System = p.Info(facts)
		n.Findings = p.Findings(facts, "network")
	}
	return &n, facts, p, nil
}

// SystemFix applies the built-in action of a system profile's finding (by its id) to what the
// finding matches on the board now.
func (s *Session) SystemFix(ctx context.Context, sink Sink, id string) error {
	_, facts, p, err := s.network(ctx)
	if err != nil {
		return err
	}
	if p == nil {
		return errors.New("no system profile recognises this board")
	}
	for _, f := range p.Findings(facts, "") {
		if f.ID != id {
			continue
		}
		if f.Action == nil || len(f.Match) == 0 {
			return fmt.Errorf("%s has nothing to do", id)
		}
		for _, u := range f.Match {
			if err := validUUID(u); err != nil {
				return err
			}
		}
		script := map[string]string{sysprofile.DoNMAutoconnectOff: remote.NMAutoconnectOff, sysprofile.DoNMDelete: remote.NMDelete}[f.Action.Do]
		if script == "" {
			return fmt.Errorf("unknown action %q", f.Action.Do)
		}
		o := s.op(ctx, sink)
		return o.step("system", f.Action.Label, func() error {
			return o.sudo("System fix", script, map[string]string{"UUIDS": strings.Join(f.Match, " ")})
		})
	}
	return fmt.Errorf("%s does not apply to this board now", id)
}

// WifiScan lists the Wi-Fi networks in range (empty while the board runs a hotspot).
func (s *Session) WifiScan(ctx context.Context) ([]WifiSeen, error) {
	out := []WifiSeen{}
	if err := s.python(ctx, remote.WifiScan, &out); err != nil {
		return nil, fmt.Errorf("Wi-Fi scan: %w", err)
	}
	return out, nil
}

func (s *Session) python(ctx context.Context, script string, v any) error {
	var out, errb strings.Builder
	if err := s.T.Exec(ctx, "python3 -", strings.NewReader(script), &out, &errb); err != nil {
		return errors.New(orErr(lastLine(errb.String()), err))
	}
	if err := json.Unmarshal([]byte(out.String()), v); err != nil {
		return fmt.Errorf("unexpected reply: %w", err)
	}
	return nil
}

// ValidSSID checks a network name Quay writes into a NetworkManager profile: 1–32 bytes, no
// control characters, no ';' or '\', no space at either end.
func ValidSSID(ssid string) error {
	switch {
	case ssid == "" || len(ssid) > 32:
		return errors.New("a Wi-Fi name has 1 to 32 bytes")
	case !utf8.ValidString(ssid), strings.ContainsAny(ssid, ";\\"), strings.IndexFunc(ssid, unicode.IsControl) >= 0:
		return errors.New("a Wi-Fi name here cannot contain ';', '\\' or control characters")
	case strings.TrimSpace(ssid) != ssid:
		return errors.New("a Wi-Fi name cannot start or end with a space")
	}
	return nil
}

// ValidPSK checks a WPA password: 8–63 printable ASCII characters, or 64 hex digits.
func ValidPSK(psk string) error {
	if len(psk) == 64 && strings.Trim(strings.ToLower(psk), "0123456789abcdef") == "" {
		return nil
	}
	if len(psk) < 8 || len(psk) > 63 {
		return errors.New("a Wi-Fi password has 8 to 63 characters")
	}
	for _, r := range psk {
		if r < 0x20 || r > 0x7e {
			return errors.New("a Wi-Fi password uses letters, digits and ASCII symbols only")
		}
	}
	return nil
}

// secret uploads data readable only by the login user, for a root script to read and delete.
func (o *op) secret(data any) (string, error) {
	b, err := json.Marshal(data)
	if err != nil {
		return "", err
	}
	path := "/tmp/quay-secret-" + randHex(8)
	if err := o.s.T.Exec(o.ctx, "umask 077 && cat > "+board.Quote(path), strings.NewReader(string(b)), nil, nil); err != nil {
		return "", fmt.Errorf("upload: %w", err)
	}
	return path, nil
}

// WifiAdd saves a Wi-Fi network (psk "" for an open one). An ordinary network is joined whenever it
// is in range; a phone hotspot as the away plan says. The current connection stays.
func (s *Session) WifiAdd(ctx context.Context, sink Sink, ssid, psk string, hidden, phone bool) error {
	if err := ValidSSID(ssid); err != nil {
		return err
	}
	if psk != "" {
		if err := ValidPSK(psk); err != nil {
			return err
		}
	}
	o := s.op(ctx, sink)
	return o.step("wifi", "Save Wi-Fi "+ssid, func() error {
		path, err := o.secret(map[string]any{"kind": "wifi", "ssid": ssid, "psk": psk, "hidden": hidden, "phone": phone})
		if err != nil {
			return err
		}
		return o.sudo("Save Wi-Fi", remote.WifiAdd, map[string]string{"SECRETS": path})
	})
}

// WifiPhone marks a saved network as a phone hotspot (phone) or as an ordinary one.
func (s *Session) WifiPhone(ctx context.Context, sink Sink, uuid string, phone bool) error {
	if err := validUUID(uuid); err != nil {
		return err
	}
	o := s.op(ctx, sink)
	yes := map[bool]string{true: "yes", false: "no"}[phone]
	return o.step("wifi", "Mark a saved Wi-Fi", func() error {
		return o.sudo("Mark Wi-Fi", remote.WifiPhone, map[string]string{"UUID": uuid, "PHONE": yes})
	})
}

// WifiRemove deletes a saved Wi-Fi network by its uuid (not the one in use).
func (s *Session) WifiRemove(ctx context.Context, sink Sink, uuid string) error {
	if err := validUUID(uuid); err != nil {
		return err
	}
	o := s.op(ctx, sink)
	return o.step("wifi", "Remove a saved Wi-Fi", func() error {
		return o.sudo("Remove Wi-Fi", remote.WifiRemove, map[string]string{"UUID": uuid})
	})
}

func validUUID(uuid string) error {
	if uuid == "" || strings.ContainsAny(uuid, " '\"\n") {
		return fmt.Errorf("invalid profile id %q", uuid)
	}
	return nil
}

// Away sets the away plan (AwayOff, AwayBoard, AwayPhone or AwayBoth). With ssid, it saves the
// board's own hotspot first (psk "" keeps the password set before). It does not change the current
// connection, except that the board's own hotspot closes if it is open and no longer in the plan.
func (s *Session) Away(ctx context.Context, sink Sink, mode, ssid, psk string) error {
	switch mode {
	case AwayOff, AwayBoard, AwayPhone, AwayBoth:
	default:
		return fmt.Errorf("unknown away plan %q (off, board, phone or both)", mode)
	}
	if ssid != "" {
		if err := ValidSSID(ssid); err != nil {
			return err
		}
	}
	if psk != "" {
		if err := ValidPSK(psk); err != nil {
			return err
		}
	}
	o := s.op(ctx, sink)
	if ssid != "" {
		if err := o.step("hotspot", "Save hotspot \""+ssid+"\"", func() error {
			path, err := o.secret(map[string]any{"kind": "hotspot", "ssid": ssid, "psk": psk})
			if err != nil {
				return err
			}
			return o.sudo("Hotspot settings", remote.HotspotSettings, map[string]string{"SECRETS": path})
		}); err != nil {
			return err
		}
	}
	return o.step("away", AwayText[mode], func() error {
		return o.sudo("Away plan", remote.Away, map[string]string{"MODE": mode})
	})
}

// AwayText describes each away plan.
var AwayText = map[string]string{
	AwayOff:   "Away from saved Wi-Fi: stay offline",
	AwayBoard: "Away from saved Wi-Fi: open the board's own hotspot",
	AwayPhone: "Away from saved Wi-Fi: join a phone hotspot",
	AwayBoth:  "Away from saved Wi-Fi: join a phone hotspot, else open the board's own",
}

// HotspotNow asks the hotspot service to open the board's own hotspot now (open) or to close it and
// join a saved Wi-Fi in range (!open). Either may end this connection a few seconds later.
func (s *Session) HotspotNow(ctx context.Context, sink Sink, open bool) error {
	o := s.op(ctx, sink)
	what, text := "close", "Close the hotspot and join a saved Wi-Fi"
	if open {
		what, text = "open", "Open the hotspot now"
	}
	return o.step("hotspot", text, func() error {
		return o.sudo("Hotspot", remote.HotspotRequest, map[string]string{"WHAT": what})
	})
}
