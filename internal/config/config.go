// Package config stores saved boards and knows where Quay keeps its files.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/caoxg/quay/internal/board"
)

// Config is persisted as JSON in Dir()/config.json.
type Config struct {
	Default string         `json:"default,omitempty"`
	Boards  []board.Target `json:"boards"`
	Lang    string         `json:"lang,omitempty"` // GUI language override: en | zh
	// RelayUpstream is the proxy this computer's relay forwards board downloads to (e.g. a local
	// Clash at http://127.0.0.1:7890); empty means the system proxy settings, or direct.
	RelayUpstream string `json:"relayUpstream,omitempty"`
	// PolicyIndexes are policy indexes read in addition to the community one (URLs or local paths).
	PolicyIndexes []string `json:"policyIndexes,omitempty"`
	// ProfilesManual turns off fetching newer profiles (github.com/caoxg/quay-profiles) by
	// themselves; Quay then uses the copy it has until asked to fetch.
	ProfilesManual bool `json:"profilesManual,omitempty"`
}

var mu sync.Mutex

// Dir is the per-user config directory (QUAY_HOME overrides).
func Dir() string {
	if d := os.Getenv("QUAY_HOME"); d != "" {
		return d
	}
	base, err := os.UserConfigDir()
	if err != nil {
		base = os.TempDir()
	}
	return filepath.Join(base, "Quay")
}

func file() string           { return filepath.Join(Dir(), "config.json") }
func KnownHostsFile() string { return filepath.Join(Dir(), "known_hosts") }
func BackupsDir() string     { return filepath.Join(Dir(), "backups") }
func PolicyCacheDir() string {
	if d, err := os.UserCacheDir(); err == nil {
		return filepath.Join(d, "Quay", "policies")
	}
	return filepath.Join(Dir(), "policies")
}

// ProfileCacheDir keeps the copy of the quay-profiles repository (system profiles).
func ProfileCacheDir() string {
	if d, err := os.UserCacheDir(); err == nil {
		return filepath.Join(d, "Quay", "profiles")
	}
	return filepath.Join(Dir(), "profiles")
}
func RepoCacheDir() string {
	if d, err := os.UserCacheDir(); err == nil {
		return filepath.Join(d, "Quay", "repos")
	}
	return filepath.Join(Dir(), "repos")
}

// Load reads the config; a missing file is an empty config.
func Load() (*Config, error) {
	mu.Lock()
	defer mu.Unlock()
	b, err := os.ReadFile(file())
	if errors.Is(err, os.ErrNotExist) {
		return &Config{}, nil
	}
	if err != nil {
		return nil, err
	}
	var c Config
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("%s: %w", file(), err)
	}
	return &c, nil
}

// Save writes the config atomically.
func (c *Config) Save() error {
	mu.Lock()
	defer mu.Unlock()
	if err := os.MkdirAll(Dir(), 0o700); err != nil {
		return err
	}
	sort.Slice(c.Boards, func(i, j int) bool { return strings.ToLower(c.Boards[i].Name) < strings.ToLower(c.Boards[j].Name) })
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	tmp := file() + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, file())
}

// Board returns a saved board by name.
func (c *Config) Board(name string) (board.Target, bool) {
	for _, b := range c.Boards {
		if strings.EqualFold(b.Name, name) {
			return b, true
		}
	}
	return board.Target{}, false
}

// Put adds or replaces a board (by name).
func (c *Config) Put(t board.Target) {
	t.Kind = board.KindADB // for Quay 0.2 and older, which know one way per board
	if t.Host != "" {
		t.Kind = board.KindSSH
	}
	for i, b := range c.Boards {
		if strings.EqualFold(b.Name, t.Name) {
			c.Boards[i] = t
			return
		}
	}
	c.Boards = append(c.Boards, t)
	if c.Default == "" {
		c.Default = t.Name
	}
}

// Adopt saves t, a board just reached whose serial number is serial, and returns its name. A board
// saved already (same serial number) keeps its name and settings and learns the way t reached it:
// added over USB it keeps its network address, and the other way round. replaced is how it was
// reached before when t changes that same way (another address); joined says that the board can
// now be reached both ways.
func (c *Config) Adopt(t board.Target, hostname, serial string) (name, replaced string, joined bool) {
	old, ok := c.ByBoardID(serial)
	if !ok {
		t.Name = c.UniqueName(hostname, serial)
		c.Put(t)
		return t.Name, "", false
	}
	m := old
	if t.Host != "" {
		if old.Host != "" && (!strings.EqualFold(old.Host, t.Host) || old.User != t.User || old.Port != t.Port) {
			replaced = old.String()
		}
		joined = old.Host == ""
		if !strings.EqualFold(old.Host, t.Host) {
			m.LastIP = t.LastIP
		}
		m.Host, m.User, m.Port, m.KeyFile = t.Host, t.User, t.Port, t.KeyFile
		if m.Via == board.KindADB {
			m.Via = ""
		}
	} else {
		joined = old.Host != "" && old.Via != board.KindADB
		if t.Serial != "" {
			m.Serial = t.Serial
		}
		if m.Via == board.KindSSH {
			m.Via = ""
		}
	}
	c.Put(m)
	return m.Name, replaced, joined && m.Via == ""
}

// ByBoardID finds a saved board by the board's serial number.
func (c *Config) ByBoardID(id string) (board.Target, bool) {
	if id == "" {
		return board.Target{}, false
	}
	for _, b := range c.Boards {
		if b.BoardID == id {
			return b, true
		}
	}
	return board.Target{}, false
}

// UniqueName is base — normally the board's hostname — or base-2, base-3… when another board
// (one with a different serial number) already has that name.
func (c *Config) UniqueName(base, boardID string) string {
	base = strings.TrimSpace(base)
	if base == "" {
		base = "board"
	}
	taken := func(n string) bool {
		for _, b := range c.Boards {
			if strings.EqualFold(b.Name, n) && (boardID == "" || b.BoardID != boardID) {
				return true
			}
		}
		return false
	}
	name := base
	for i := 2; taken(name); i++ {
		name = fmt.Sprintf("%s-%d", base, i)
	}
	return name
}

// Learn records what a connection revealed about a saved board: its hostname (which is its name in
// Quay), its serial number and its current IPv4 address. It returns the board's name afterwards
// and whether anything changed (the caller saves). A board reached through its mDNS name follows a
// rename: duck.local becomes goose.local. A board saved under name no more, because another
// operation followed its rename first (the board page's refresh during a first setup), is found by
// its serial number.
func (c *Config) Learn(name, hostname, serial, ip string) (string, bool) {
	i := -1
	for j, b := range c.Boards {
		if strings.EqualFold(b.Name, name) {
			i = j
			break
		}
	}
	for j, b := range c.Boards {
		if i < 0 && serial != "" && b.BoardID == serial {
			i = j
		}
	}
	if i < 0 {
		return name, false
	}
	t := &c.Boards[i]
	changed := false
	if serial != "" && t.BoardID != serial {
		t.BoardID, changed = serial, true
	}
	if ip != "" && t.Host != "" && net.ParseIP(t.Host) == nil && t.LastIP != ip {
		t.LastIP, changed = ip, true
	}
	if hostname = strings.TrimSpace(hostname); hostname != "" && !strings.EqualFold(t.Name, hostname) {
		if n := c.UniqueName(hostname, t.BoardID); !strings.EqualFold(n, t.Name) {
			old := t.Name
			if strings.EqualFold(strings.TrimSuffix(t.Host, "."), old+".local") {
				t.Host = hostname + ".local"
			}
			t.Name = n
			if strings.EqualFold(c.Default, old) {
				c.Default = n
			}
			changed = true
		}
	}
	return t.Name, changed
}

// Remove deletes a board.
func (c *Config) Remove(name string) bool {
	for i, b := range c.Boards {
		if strings.EqualFold(b.Name, name) {
			c.Boards = append(c.Boards[:i], c.Boards[i+1:]...)
			if strings.EqualFold(c.Default, name) {
				c.Default = ""
				if len(c.Boards) > 0 {
					c.Default = c.Boards[0].Name
				}
			}
			return true
		}
	}
	return false
}
