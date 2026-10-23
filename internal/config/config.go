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
// rename: duck.local becomes goose.local.
func (c *Config) Learn(name, hostname, serial, ip string) (string, bool) {
	i := -1
	for j, b := range c.Boards {
		if strings.EqualFold(b.Name, name) {
			i = j
			break
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
	if ip != "" && t.Kind == board.KindSSH && net.ParseIP(t.Host) == nil && t.LastIP != ip {
		t.LastIP, changed = ip, true
	}
	if hostname = strings.TrimSpace(hostname); hostname != "" && !strings.EqualFold(t.Name, hostname) {
		if n := c.UniqueName(hostname, t.BoardID); !strings.EqualFold(n, t.Name) {
			old := t.Name
			if t.Kind == board.KindSSH && strings.EqualFold(strings.TrimSuffix(t.Host, "."), old+".local") {
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
